package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runArtifactStep performs an upload-artifact or download-artifact step
// (A49).
//
// It is the run loop's rather than an executor's because both ends of the
// copy — the workspace and this run's artifact store — are host paths the
// agent owns, on docker as on native; handing it to an executor would mean a
// container reaching the artifact store, which is exactly the boundary §17
// keeps.
//
// What was only known as the step runs — a name built from a step's outputs,
// a path in runner.temp or read from a step's outputs — is resolved here,
// and the step's outputs are what actually happened (A77).
func (x *execution) runArtifactStep(
	ctx context.Context, s registry.ResolvedStep, label string, sc registry.StepScope,
) (outcome, map[string]string) {
	a := s.Artifact
	if x.o.artifacts == nil || x.ws == nil {
		return fail(errors.New("this agent has no artifact store"), protocol.ErrArtifact), nil
	}
	name, err := resolveArtifactValue(a.Name, lateName(a), "name", sc)
	if err != nil {
		return fail(err, protocol.ErrBadRegistry), nil
	}
	if a.Download {
		return x.download(ctx, a, name, label, sc)
	}
	return x.upload(ctx, a, name, label, sc)
}

func (x *execution) download(
	ctx context.Context, a *registry.ArtifactStep, name, label string, sc registry.StepScope,
) (outcome, map[string]string) {
	where, dest := x.artifactPlaces()[0], a.Path
	if a.Late != nil && a.Late.Path != nil {
		raw, err := a.Late.Path.Resolve(sc)
		if err != nil {
			return fail(&registry.Error{Code: protocol.ErrBadRegistry, Msg: "artifact download path", Err: err},
				protocol.ErrBadRegistry), nil
		}
		if where, dest, err = x.placeArtifactPath(strings.TrimSpace(raw)); err != nil {
			return artifactRefused(label, err), nil
		}
	}
	if dest == "" {
		dest = "."
	}
	runs, err := x.artifactRuns(ctx, a.From)
	if err != nil {
		return fail(err, protocol.ErrArtifact), nil
	}

	base := where.host
	if base == "" {
		// runner.temp in a container: restored here, then copied in (A84).
		staged, done, err := x.stage()
		if err != nil {
			return fail(err, protocol.ErrArtifact), nil
		}
		defer done()
		base = staged
	}
	n, from, err := x.restoreFrom(runs, base, name, dest)
	if err != nil {
		if errors.Is(err, artifact.ErrNoSuchArtifact) {
			return outcome{state: protocol.StateFailed, code: protocol.ErrArtifact,
				msg: fmt.Sprintf("step %q: %v — %s", label, err, x.searched(a.From, runs))}, nil
		}
		return fail(err, protocol.ErrArtifact), nil
	}
	if where.host == "" {
		if err := where.copier.CopyToTemp(ctx, base); err != nil {
			return fail(err, protocol.ErrArtifact), nil
		}
	}
	if len(from) > 0 {
		x.note("step %s: restored %d file(s), uploaded by %s", label, n, strings.Join(from, ", "))
	} else {
		x.note("step %s: restored %d file(s)", label, n)
	}
	return outcome{state: protocol.StateSucceeded}, map[string]string{"download-path": where.join(dest)}
}

// maxArtifactRuns bounds the runs a download reads past its own: a workflow
// run's jobs that it needs, on this machine, matrix legs and second attempts
// included. sokie's release reads one; a hundred legs would be a matrix no
// workflow here has.
const maxArtifactRuns = 64

// sourceRun is a run of a job whose artifacts a download may read (A84).
type sourceRun struct{ id, job string }

// artifactRuns is the runs on this machine of the jobs from names, in this
// workflow run, newest first: the runs a download reads after its own (A84).
//
// from is the commit's answer, compiled from the workspace; nothing the
// control plane sent chooses a run. A run counts only if this machine recorded
// it as the same workflow run, the same file, the same commit and the same run
// number, and only once it has finished, so a download never reads an upload
// still being copied. The runs are this machine's own builds of the
// repository, so the most a control plane could do by lying about which
// workflow run a job is part of is point it at another build of the same
// commit and file — never at bytes it wrote.
func (x *execution) artifactRuns(ctx context.Context, from []string) ([]sourceRun, error) {
	w := x.run.Workflow
	if len(from) == 0 || w == nil || w.WorkflowRunID == "" {
		return nil, nil
	}
	rows, err := x.o.store.WorkflowRunJobs(ctx, x.run.Repo, w.WorkflowRunID, from, maxArtifactRuns)
	if err != nil {
		return nil, err
	}
	var out []sourceRun
	for _, r := range rows {
		o := r.Workflow
		if r.ID == x.run.ID || o == nil || !r.State.IsTerminal() || r.IsLocal() != x.run.IsLocal() ||
			o.Workflow != w.Workflow || o.SHA != w.SHA || o.RunNumber != w.RunNumber {
			continue
		}
		out = append(out, sourceRun{id: r.ID, job: r.Job})
	}
	return out, nil
}

// restoreFrom restores an artifact from this run, else from the first of runs
// that has it. A download of every artifact takes each name from the first
// run that has one by that name, this one first, each under dest/<name>, as
// GitHub lays them out. from names the jobs the files came from, when not
// this one.
func (x *execution) restoreFrom(runs []sourceRun, base, name, dest string) (int, []string, error) {
	ids := append([]sourceRun{{id: x.run.ID}}, runs...)
	if name == "" {
		total, seen := 0, map[string]bool{}
		var from []string
		for _, r := range ids {
			names, err := x.o.artifacts.Names(r.id)
			if err != nil {
				return total, from, err
			}
			for _, n := range names {
				if seen[n] {
					continue
				}
				seen[n] = true
				c, err := x.o.artifacts.Restore(r.id, base, n, path.Join(dest, n))
				total += c
				if err != nil {
					return total, from, err
				}
				if r.job != "" && !slices.Contains(from, r.job) {
					from = append(from, r.job)
				}
			}
		}
		return total, from, nil
	}
	var err error
	for _, r := range ids {
		var n int
		n, err = x.o.artifacts.Restore(r.id, base, name, dest)
		if err == nil {
			if r.job == "" {
				return n, nil, nil
			}
			return n, []string{r.job}, nil
		}
		if !errors.Is(err, artifact.ErrNoSuchArtifact) {
			return n, nil, err
		}
	}
	return 0, nil, err
}

// searched says where a download looked, for the step that found nothing.
func (x *execution) searched(from []string, runs []sourceRun) string {
	if len(from) == 0 {
		return "download-artifact finds what this job uploaded, and what a job it needs uploaded (A84); " +
			"this job reads no other job's"
	}
	jobs := strings.Join(from, ", ")
	if len(runs) == 0 {
		return fmt.Sprintf("it reads what %s uploaded, and no run of %s in this workflow run finished on "+
			"this machine; the control plane places this job on the machine that ran them (A84)", jobs, jobs)
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.id
	}
	return fmt.Sprintf("it reads what %s uploaded, and their runs here (%s) have no such artifact, or "+
		"retention has removed it", jobs, strings.Join(ids, ", "))
}

// stage is a directory on the host standing in for a place the agent reaches
// only through the executor (A84): docker's runner.temp. A download restores
// into it and the executor copies it in; an upload has the executor copy out
// to it and collects from there. It is beside the workspace, in the work
// directory, which is on the disk builds have room on and which a restart
// empties; done removes it.
func (x *execution) stage() (string, func(), error) {
	dir, err := os.MkdirTemp(filepath.Dir(x.ws.Path), x.run.ID+".artifact-")
	if err != nil {
		return "", func() {}, fmt.Errorf("staging an artifact: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// maxStagedBytes bounds what one upload copies out of a container's
// runner.temp. An .ipa is 50–300 MB; ten gigabytes is a fifth of the whole
// artifact budget (§17), and an upload that size is a pattern that matched
// more than anyone meant.
const maxStagedBytes int64 = 10 << 30

// stageFromTemp copies what an upload's patterns could match out of a
// container's runner.temp into a staging directory, at the same relative
// paths, so the collector reads it as it reads a host directory. Each
// pattern's literal prefix is copied, once: `pack/app.tgz` copies one file,
// `pack/*.tgz` the directory pack, `**` all of runner.temp.
func (x *execution) stageFromTemp(ctx context.Context, where artifactPlace, include []string) (string, func(), error) {
	staged, done, err := x.stage()
	if err != nil {
		return "", done, err
	}
	var prefixes []string
	for _, p := range include {
		prefixes = append(prefixes, literalPrefix(p))
	}
	slices.Sort(prefixes)
	var copied []string
	budget := maxStagedBytes
	for _, p := range slices.Compact(prefixes) {
		if slices.ContainsFunc(copied, func(c string) bool { return c == "" || strings.HasPrefix(p, c+"/") }) {
			continue
		}
		n, err := where.copier.CopyFromTemp(ctx, p, staged, budget)
		if err != nil {
			done()
			return "", func() {}, err
		}
		budget -= n
		copied = append(copied, p)
	}
	return staged, done, nil
}

// literalPrefix is a pattern's path up to its first segment with a glob in
// it: the file or directory the pattern searches. "" is the whole directory.
func literalPrefix(pattern string) string {
	var lit []string
	for _, seg := range strings.Split(path.Clean(strings.TrimSpace(pattern)), "/") {
		if seg == "." || strings.ContainsAny(seg, "*?[") {
			break
		}
		lit = append(lit, seg)
	}
	return strings.Join(lit, "/")
}

func (x *execution) upload(
	ctx context.Context, a *registry.ArtifactStep, name, label string, sc registry.StepScope,
) (outcome, map[string]string) {
	where := x.artifactPlaces()[0]
	include, exclude := a.Include, a.Exclude
	if a.Late != nil && len(a.Late.Include) > 0 {
		var err error
		if where, include, exclude, err = x.placeLatePaths(a, sc); err != nil {
			var reg *registry.Error
			if errors.As(err, &reg) {
				return fail(err, protocol.ErrBadRegistry), nil
			}
			return artifactRefused(label, err), nil
		}
	}

	base := where.host
	if base == "" {
		// runner.temp in a container: copied out first, then collected (A84).
		staged, done, err := x.stageFromTemp(ctx, where, include)
		if err != nil {
			return fail(err, protocol.ErrArtifact), nil
		}
		defer done()
		base = staged
	}
	refs, err := x.o.artifacts.CollectNamed(ctx, x.run.ID, base, artifact.Upload{
		Name:    name,
		Include: include,
		Exclude: exclude,
		Retain:  a.Retain.Duration(),
		Single:  a.Single,
	})
	if err != nil {
		return fail(err, protocol.ErrArtifact), nil
	}
	if len(refs) == 0 {
		paths := include
		if len(paths) == 0 && a.Late != nil {
			for _, v := range a.Late.Include {
				paths = append(paths, v.Source())
			}
		}
		what := fmt.Sprintf("artifact %q", name)
		if a.Single {
			what = "the artifact (archive: false)"
		}
		switch a.IfNoFilesFound {
		case "error":
			return outcome{
				state: protocol.StateFailed,
				code:  protocol.ErrArtifact,
				msg:   fmt.Sprintf("step %q: nothing matched %v in %s for %s", label, paths, where.name, what),
			}, nil
		case "warn":
			x.note("step %s: nothing matched %v in %s; %s has no files", label, paths, where.name, what)
		}
		return outcome{state: protocol.StateSucceeded}, map[string]string{"artifact-id": "", "artifact-url": "", "artifact-digest": ""}
	}
	digest := ""
	if a.Single {
		// The file is the artifact, so its digest is the artifact's, as
		// GitHub's is for an unarchived upload.
		name, _, _ = strings.Cut(refs[0].Path, "/")
		digest = refs[0].SHA256
	}
	x.stepArtifacts = append(x.stepArtifacts, refs...)
	x.note("step %s: uploaded artifact %q from %s, %d file(s)", label, name, where.name, len(refs))
	// artifact-id is the name, which is how this store knows it; there is no
	// web page to link to, so the url is empty rather than invented.
	return outcome{state: protocol.StateSucceeded}, map[string]string{
		"artifact-id": name, "artifact-url": "", "artifact-digest": digest,
	}
}

func lateName(a *registry.ArtifactStep) registry.LateValue {
	if a.Late == nil {
		return nil
	}
	return a.Late.Name
}

// resolveArtifactValue is the static value, or the late one resolved now
// against the scope the step runs in.
func resolveArtifactValue(static string, late registry.LateValue, field string, sc registry.StepScope) (string, error) {
	if late == nil {
		return static, nil
	}
	v, err := late.Resolve(sc)
	if err != nil {
		return "", &registry.Error{Code: protocol.ErrBadRegistry, Msg: "artifact " + field, Err: err}
	}
	return strings.TrimSpace(v), nil
}

// placeLatePaths resolves an upload's late `path:` lines and places them. The
// lines that resolve to nothing — an output a skipped step never wrote — are
// left out, and an upload left with no lines matches nothing, which its
// if-no-files-found decides about.
//
// Every line has to be in the same place. upload-artifact roots an artifact at
// the directory its paths share, and the workspace and runner.temp share none
// a step could name.
func (x *execution) placeLatePaths(a *registry.ArtifactStep, sc registry.StepScope) (
	artifactPlace, []string, []string, error,
) {
	where := x.artifactPlaces()[0]
	first := ""
	place := func(lines []registry.LateValue) ([]string, error) {
		var out []string
		for _, v := range lines {
			raw, err := resolveArtifactValue("", v, "path", sc)
			if err != nil {
				return nil, err
			}
			if raw == "" {
				continue
			}
			pl, rel, err := x.placeArtifactPath(raw)
			if err != nil {
				return nil, err
			}
			if first == "" {
				first, where = raw, pl
			} else if pl.name != where.name {
				return nil, fmt.Errorf("%q is in %s and %q in %s; an upload's paths are all in one of the two (A77)",
					first, where.name, raw, pl.name)
			}
			if rel == "." {
				// The whole directory, less the hidden files, as `path: .`
				// is at compile time.
				rel = "**"
			}
			out = append(out, rel)
		}
		return out, nil
	}
	include, err := place(a.Late.Include)
	if err != nil {
		return where, nil, nil, err
	}
	exclude, err := place(a.Late.Exclude)
	if err != nil {
		return where, nil, nil, err
	}
	return where, include, append(exclude, a.Exclude...), nil
}

func artifactRefused(label string, err error) outcome {
	return outcome{state: protocol.StateFailed, code: protocol.ErrArtifact, msg: fmt.Sprintf("step %q: %v", label, err)}
}

// artifactPlace is one of the two directories an artifact step reaches: the
// workspace, where a step starts and where a relative path is, and the run's
// temporary directory, runner.temp (A77). seen is the directory as a step
// sees it, host as the agent reaches it; host is "" for a temporary directory
// that is not on the host, which copier reaches instead (docker's, A84), and
// for one neither reaches.
type artifactPlace struct {
	name, seen, host string
	workspace        bool
	copier           executor.TempCopier
}

// join is dest as a step names it, for download-path: relative in the
// workspace, as the compiler substitutes it when the path is known at
// admission, and absolute in runner.temp, which no step starts in.
func (p artifactPlace) join(dest string) string {
	if p.workspace {
		return dest
	}
	dir := p.seen
	if dir == "" {
		dir = p.host
	}
	return path.Join(filepath.ToSlash(dir), dest)
}

func (x *execution) artifactPlaces() [2]artifactPlace {
	ws := artifactPlace{name: "the workspace", host: x.ws.Path, workspace: true}
	temp := artifactPlace{name: "runner.temp"}
	if x.env != nil {
		ws.seen = x.env.WorkspaceDir()
		temp.seen = x.env.TempDir()
		if ht, ok := x.env.(executor.HostTemp); ok {
			temp.host = ht.HostTempDir()
		}
		if tc, ok := x.env.(executor.TempCopier); ok && temp.host == "" {
			temp.copier = tc
		}
	}
	return [2]artifactPlace{ws, temp}
}

// placeArtifactPath answers which place p is in, and p relative to it.
//
// p is a path as the step that wrote it sees it: relative to the workspace,
// or absolute in the workspace or in runner.temp — the executor's path for
// either, or the host's, or the host's with its symlinks resolved, since on a
// Mac a temporary directory is reached through /var → /private/var and a step
// that printed `pwd -P` wrote the second. The longest match wins. Anything
// else is refused rather than collected: an artifact step copies between this
// run's store and those two directories, and nowhere else (A49, §17).
func (x *execution) placeArtifactPath(p string) (artifactPlace, string, error) {
	places := x.artifactPlaces()
	if strings.HasPrefix(p, "~") {
		return places[0], "", fmt.Errorf(
			"%q is in a home directory, and an artifact step reaches the workspace and runner.temp only (A77)", p)
	}
	if !strings.HasPrefix(p, "/") {
		for strings.HasPrefix(p, "./") {
			p = strings.TrimPrefix(p, "./")
		}
		if p == "" || p == "." {
			return places[0], ".", nil
		}
		return places[0], p, nil
	}

	clean := path.Clean(p)
	var found *artifactPlace
	bestLen, rel := -1, ""
	for i := range places {
		for _, dir := range candidates(places[i]) {
			r, ok := under(clean, dir)
			if ok && len(dir) > bestLen {
				found, bestLen, rel = &places[i], len(dir), r
			}
		}
	}
	if found == nil {
		return places[0], "", fmt.Errorf(
			"%q is outside the workspace and runner.temp, the two places an artifact step reaches (A77); "+
				"copy the file into one of them in an earlier step", p)
	}
	if found.host == "" && found.copier == nil {
		return *found, "", fmt.Errorf(
			"%q is in runner.temp, which on this runner is inside its VM, where the agent cannot "+
				"reach it (A77, A84); copy the file into the workspace in an earlier step", p)
	}
	return *found, rel, nil
}

func candidates(pl artifactPlace) []string {
	var out []string
	for _, d := range []string{pl.seen, pl.host} {
		if d == "" {
			continue
		}
		out = append(out, path.Clean(filepath.ToSlash(d)))
		if real, err := filepath.EvalSymlinks(d); err == nil {
			out = append(out, path.Clean(filepath.ToSlash(real)))
		}
	}
	return out
}

// under is p relative to dir, "." for dir itself.
func under(p, dir string) (string, bool) {
	if p == dir {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
		return rest, true
	}
	return "", false
}
