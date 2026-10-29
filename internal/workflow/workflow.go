// Package workflow reads .github/workflows/*.yml as a job registry, per
// amendment A42.
//
// It exists so that the repository latem is being bolted onto does not have to
// re-author a pipeline it already has. The unit of adoption is one job: a hook
// binds a repository and a ref pattern to one job on one machine, and that job
// may now be named in a workflow file rather than only in .latem/jobs.yaml.
//
// Two rules shape everything here.
//
// Decoding is strict, as it is for the native registry. An unknown key is an
// error rather than a warning, because the alternative is running something
// other than what the file says — a workflow with a `strategy:` latem ignored
// would build one of the four configurations the author asked for and report
// success.
//
// And no context value is ever written into a command. A42 makes that
// normative: `${{ github.event.pull_request.title }}` interpolated into `run:`
// is the injection class GitHub issues advisories for, so [Compile] binds every
// expression in a `run:` to an environment variable and rewrites the command to
// reference it. The value reaches the shell as data. A branch named
// `; rm -rf /` is a branch name.
package workflow

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Abyssinia-Labs/latem/internal/registry"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Dir is where workflows live in a repository.
const Dir = ".github/workflows"

// Workflow is one parsed workflow file.
type Workflow struct {
	Name    string          `json:"name,omitempty"`
	RunName string          `json:"run-name,omitempty"`
	On      json.RawMessage `json:"on,omitempty"`
	// OnYAML11 is the same `on:` block, arriving under the key "true".
	//
	// Not a workaround to be tidied away later. YAML 1.1 — which is what this
	// decoder speaks, and what GitHub's own parser tolerates — resolves the
	// bare plain scalars `on`, `off`, `yes` and `no` to booleans, *including*
	// when they are mapping keys. So the first line of essentially every
	// workflow ever written, `on: [push]`, arrives here as the key `true`.
	// Without this field strict decoding rejects every real file, and with a
	// non-strict decoder it would instead be dropped in silence.
	OnYAML11    json.RawMessage   `json:"true,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Defaults    *Defaults         `json:"defaults,omitempty"`
	Permissions json.RawMessage   `json:"permissions,omitempty"`
	Concurrency json.RawMessage   `json:"concurrency,omitempty"`
	Jobs        map[string]*Job   `json:"jobs"`

	// Path is the file this came from, for messages that name it.
	Path string `json:"-"`
}

// Defaults is the `defaults: run:` block, at workflow or job level.
type Defaults struct {
	Run *RunDefaults `json:"run,omitempty"`
}

// RunDefaults are the shell settings a step inherits.
type RunDefaults struct {
	Shell            string `json:"shell,omitempty"`
	WorkingDirectory string `json:"working-directory,omitempty"`
}

// Job is one job in a workflow.
type Job struct {
	Name        string            `json:"name,omitempty"`
	RunsOn      StringList        `json:"runs-on,omitempty"`
	If          string            `json:"if,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Defaults    *Defaults         `json:"defaults,omitempty"`
	Steps       []Step            `json:"steps,omitempty"`
	TimeoutMins *float64          `json:"timeout-minutes,omitempty"`

	// Everything below is modelled so that strict decoding accepts a real
	// workflow file, and then refused by [Job.unsupported] with a message
	// naming the construct. Silently dropping any of them would run something
	// other than what the file says.
	Needs           StringList      `json:"needs,omitempty"`
	Strategy        json.RawMessage `json:"strategy,omitempty"`
	Container       json.RawMessage `json:"container,omitempty"`
	Services        json.RawMessage `json:"services,omitempty"`
	Outputs         json.RawMessage `json:"outputs,omitempty"`
	Environment     json.RawMessage `json:"environment,omitempty"`
	Permissions     json.RawMessage `json:"permissions,omitempty"`
	Concurrency     json.RawMessage `json:"concurrency,omitempty"`
	ContinueOnError json.RawMessage `json:"continue-on-error,omitempty"`
	Uses            string          `json:"uses,omitempty"`
	With            json.RawMessage `json:"with,omitempty"`
	Secrets         json.RawMessage `json:"secrets,omitempty"`
}

// Step is one step in a job.
type Step struct {
	ID               string            `json:"id,omitempty"`
	Name             string            `json:"name,omitempty"`
	If               string            `json:"if,omitempty"`
	Run              string            `json:"run,omitempty"`
	Uses             string            `json:"uses,omitempty"`
	With             json.RawMessage   `json:"with,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"working-directory,omitempty"`
	TimeoutMins      *float64          `json:"timeout-minutes,omitempty"`
	ContinueOnError  json.RawMessage   `json:"continue-on-error,omitempty"`

	// container is set by the expander for a `uses: docker://…` step (A44).
	// Unexported, so no file can write it.
	container *containerSpec
	// artifact is set by the expander for upload-artifact and
	// download-artifact (A49). Unexported for the same reason.
	artifact *registry.ArtifactStep
	// fromAction marks a step inlined from a composite action. A job's
	// `defaults.run` does not reach into an action's steps on GitHub, and does
	// not here (A50).
	fromAction bool
	// actionUse ties a JavaScript action's pre, main and post steps together,
	// for $GITHUB_STATE (A51).
	actionUse string
}

// StringList accepts either a scalar or a sequence, which is how `runs-on` and
// `needs` are both written in practice.
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
//
// The object form of `runs-on` — {group, labels} — is a hosted-runner-group
// concept with no meaning on a machine you own, so it is refused by name
// rather than half-read.
func (s *StringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*s = many
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		if _, ok := obj["group"]; ok {
			return fmt.Errorf("the {group, labels} form of runs-on names a hosted runner group, which does not exist on a machine you own; use a list of labels")
		}
	}
	return fmt.Errorf("expected a string or a list of strings, got %s", string(data))
}

// Parse decodes one workflow file.
//
// filename is used only in messages. Parsing does not decide whether a job can
// run — [Compile] does that, because the answer depends on the job.
func Parse(data []byte, filename string) (*Workflow, error) {
	var w Workflow
	if err := yaml.UnmarshalStrict(data, &w); err != nil {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("parsing %s", filename),
			Err:  err,
		}
	}
	w.Path = filename
	if len(w.Jobs) == 0 {
		return nil, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("%s declares no jobs", filename),
		}
	}
	for name, j := range w.Jobs {
		if j == nil {
			return nil, &registry.Error{
				Code: protocol.ErrBadRegistry,
				Msg:  fmt.Sprintf("%s: job %q is empty", filename, name),
			}
		}
		// The same YAML 1.1 resolution that turns `on:` into `true` applies to
		// a job named on, off, yes or no. It would otherwise present as
		// "no such job", sending the author to look at a name that is right in
		// the file in front of them.
		switch name {
		case "true", "false":
			return nil, &registry.Error{
				Code: protocol.ErrBadRegistry,
				Msg: fmt.Sprintf(
					"%s: a job key resolved to the boolean %s — YAML reads a bare on/off/yes/no "+
						"as a boolean, so quote the job's name", filename, name),
			}
		}
	}
	return &w, nil
}

// JobNames returns the job names in sorted order.
func (w *Workflow) JobNames() []string {
	out := make([]string, 0, len(w.Jobs))
	for k := range w.Jobs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TriggersAreNotHonoured reports whether the file carries an `on:` block.
//
// Latem does not read it. A run reaches a machine because a hook bound a
// repository and a ref pattern to a job, or because someone typed `latem run`;
// the workflow's own triggers describe what GitHub would do with the file, and
// acting on them as well would give one push two different meanings. The caller
// surfaces this rather than the parser refusing it, because a workflow that
// still runs on GitHub *and* has a job pointed at your Mac is the whole
// bolt-on case.
func (w *Workflow) TriggersAreNotHonoured() bool {
	return len(w.On) > 0 || len(w.OnYAML11) > 0
}

// IsWorkflowPath reports whether a repository path is a workflow file.
func IsWorkflowPath(p string) bool {
	if !strings.HasPrefix(p, Dir+"/") {
		return false
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".yml", ".yaml":
		return true
	}
	return false
}

// badRegistry builds the error this package returns for a malformed or
// unsupported workflow.
//
// It is a [registry.Error] rather than a type of this package's own. That type's
// doc comment says why: a caller should never have to infer a code from a
// message, and two error types carrying the same codes is how a taxonomy rots.
// Concretely, a parallel type is invisible to [registry.CodeOf], so every
// refusal here would reach the run state machine as the default — which is
// exactly what happened the first time this was written.
func badRegistry(format string, args ...any) *registry.Error {
	return &registry.Error{Code: protocol.ErrBadRegistry, Msg: fmt.Sprintf(format, args...)}
}
