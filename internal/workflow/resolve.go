package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// MaxWorkflowBytes bounds one file read by [Resolve] — a workflow or
	// .latem/jobs.yaml alike. Both come from a repository and are read into
	// memory before they are parsed, so both get a size they cannot exceed as
	// well as the field limits the parsers enforce. One constant rather than
	// two, because two caps on the same class of input drift apart and the
	// looser one becomes the real limit.
	MaxWorkflowBytes = 1 << 20
	// MaxWorkflowFiles bounds how many are read in one resolution. A
	// repository with more than this many workflows is not a repository latem
	// should be walking on every run, and an unbounded loop over
	// attacker-authored directory contents is the shape of a cheap denial of
	// service against gate 11.
	MaxWorkflowFiles = 64
)

// Source records where a resolved job came from, so a run's log can say.
type Source struct {
	// Path is the file, relative to the workspace root.
	Path string
	// Native is true for .latem/jobs.yaml.
	Native bool
	// Workflow is the workflow's `name:`, or its path when it has none, which
	// is what GitHub reports as github.workflow and $GITHUB_WORKFLOW (A50).
	// Empty for a jobs.yaml job.
	Workflow string
	// JobFile and Job are set for a job of a called workflow (A81): the file
	// that declares it, and its id there, which is github.job and
	// $GITHUB_JOB. Path stays the triggered workflow's.
	JobFile, Job string
}

func (s Source) String() string { return s.Path }

// Resolve finds a job by name at the resolved commit.
//
// Precedence is settled and narrow: **.latem/jobs.yaml wins**. If the native
// registry declares the name, that is the job, and no workflow is read. A
// repository adopting latem gradually can therefore take over one job at a time
// by adding it to jobs.yaml, and nothing about the workflow it came from has to
// change for the takeover to be complete.
//
// Otherwise the name is looked for across .github/workflows. The name is the
// only selector — there is no file argument — so it has to be unambiguous
// there, and a name declared by two workflows is an error that names both
// rather than a coin toss. That a native job of the same name would have won
// is stated in the message, because adding one is the fix.
//
// A malformed .latem/jobs.yaml is returned as an error rather than fallen
// through: falling back would turn a syntax error into a silent change of which
// file defines the build.
func Resolve(ctx context.Context, workspace, jobName string, sc expr.Scope) (*registry.Registry, Source, error) {
	return ResolveWith(ctx, workspace, jobName, sc, Options{})
}

// ResolveWith is [Resolve] against a machine's action policy.
func ResolveWith(
	ctx context.Context, workspace, jobName string, sc expr.Scope, opts Options,
) (*registry.Registry, Source, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, Source{}, &registry.Error{
			Code: protocol.ErrNoRegistry,
			Msg:  "opening the workspace",
			Err:  err,
		}
	}
	defer func() { _ = root.Close() }()

	native, err := readNative(root)
	if err != nil {
		return nil, Source{}, err
	}
	if native != nil {
		if _, ok := native.Jobs[jobName]; ok {
			return native, Source{Path: registry.Path, Native: true}, nil
		}
	}

	files, err := workflowFiles(root.FS())
	if err != nil {
		return nil, Source{}, err
	}

	type hit struct {
		file string
		wf   *Workflow
	}
	var hits []hit
	var allNames []string
	for _, f := range files {
		data, err := readCapped(root.FS(), f, MaxWorkflowBytes)
		if err != nil {
			return nil, Source{}, err
		}
		wf, err := Parse(data, f)
		if err != nil {
			return nil, Source{}, err
		}
		for _, n := range wf.JobNames() {
			allNames = append(allNames, f+":"+n)
		}
		if _, ok := wf.Jobs[jobName]; ok {
			hits = append(hits, hit{file: f, wf: wf})
		}
	}

	switch {
	case len(hits) == 1:
		// The same root the workflow was read through, so a local action is
		// read at the same commit under the same symlink discipline.
		opts.Actions = rootLoader{root: root}
		job, err := CompileWith(ctx, hits[0].wf, jobName, sc, opts)
		if err != nil {
			return nil, Source{}, err
		}
		reg := RegistryFor(jobName, job)
		if err := reg.Validate(); err != nil {
			return nil, Source{}, &registry.Error{
				Code: protocol.ErrBadRegistry,
				Msg:  fmt.Sprintf("%s: job %q compiled to a registry job that does not validate", hits[0].file, jobName),
				Err:  err,
			}
		}
		name := hits[0].wf.Name
		if name == "" {
			name = hits[0].file
		}
		return reg, Source{Path: hits[0].file, Workflow: name}, nil

	case len(hits) > 1:
		var where []string
		for _, h := range hits {
			where = append(where, h.file)
		}
		return nil, Source{}, &registry.Error{
			Code: protocol.ErrUnknownJob,
			Msg: fmt.Sprintf(
				"job %q is declared by %s. A job is selected by name alone, so the name has to be "+
					"unique across workflows — rename one, or declare %q in %s, which wins over every workflow",
				jobName, strings.Join(where, " and "), jobName, registry.Path),
		}
	}

	if native == nil && len(files) == 0 {
		return nil, Source{}, &registry.Error{
			Code: protocol.ErrNoRegistry,
			Msg: fmt.Sprintf("neither %s nor any %s/*.yml is present at this commit",
				registry.Path, Dir),
		}
	}
	return nil, Source{}, &registry.Error{
		Code: protocol.ErrUnknownJob,
		Msg:  fmt.Sprintf("no job named %q at this commit; %s", jobName, available(native, allNames)),
	}
}

// available renders what the commit does declare, so the answer to "no such
// job" is in the same line as the question.
func available(native *registry.Registry, workflowNames []string) string {
	var parts []string
	if native != nil && len(native.Jobs) > 0 {
		names := make([]string, 0, len(native.Jobs))
		for n := range native.Jobs {
			names = append(names, n)
		}
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("%s declares %s", registry.Path, strings.Join(names, ", ")))
	}
	if len(workflowNames) > 0 {
		sort.Strings(workflowNames)
		parts = append(parts, "workflows declare "+strings.Join(workflowNames, ", "))
	}
	if len(parts) == 0 {
		return "nothing declares any job"
	}
	return strings.Join(parts, "; ")
}

// readNative reads .latem/jobs.yaml, answering (nil, nil) when it is absent.
func readNative(root *os.Root) (*registry.Registry, error) {
	data, err := readCapped(root.FS(), registry.Path, MaxWorkflowBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	reg, parseErr := registry.Parse(data)
	if parseErr != nil {
		return nil, parseErr
	}
	if valErr := reg.Validate(); valErr != nil {
		return nil, valErr
	}
	return reg, nil
}

// workflowFiles lists .github/workflows/*.yml and *.yaml, sorted.
//
// Read through an [fs.FS] — a workspace's [os.Root.FS], or a commit in the
// mirror when planning (A52) — rather than by joining paths onto a directory,
// for the reason gate 11 gives: a workspace holds whatever a repository and a
// build put there, and `.github/workflows` being a symlink to somewhere else on
// the machine is a plain thing to commit.
func workflowFiles(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &registry.Error{
			Code: protocol.ErrNoRegistry,
			Msg:  fmt.Sprintf("reading %s", Dir),
			Err:  err,
		}
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := path.Join(Dir, e.Name())
		if !IsWorkflowPath(p) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > MaxWorkflowFiles {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg: fmt.Sprintf("%s holds %d workflow files, more than the %d this reads",
				Dir, len(out), MaxWorkflowFiles),
		}
	}
	return out, nil
}

// readCapped reads a file through fsys, refusing one byte past the cap so
// an oversized file is an error rather than a silent truncation that happens to
// parse. The shape matches gate 11's, deliberately: two readers of
// repository-supplied files that differ in their limits differ in what they
// admit.
func readCapped(fsys fs.FS, name string, max int) ([]byte, error) {
	f, err := fsys.Open(path.Clean(name))
	if err != nil {
		return nil, err // fs.ErrNotExist is meaningful to the caller
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("reading %s", name),
			Err:  err,
		}
	}
	if len(data) > max {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("%s is larger than %d bytes", name, max),
		}
	}
	return data, nil
}
