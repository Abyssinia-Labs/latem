// Package executor defines the strategy that provides an environment for a run.
//
// This file is owned by WS-00 and frozen. Three implementations satisfy it:
// docker (WS-07), tart and native (WS-13). Changing anything here is a contract
// amendment.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Executor provisions environments. One Executor serves many runs.
type Executor interface {
	// Prepare provisions an environment. The returned [Env] serves every step
	// of one run and is then closed.
	Prepare(ctx context.Context, spec Spec) (Env, error)

	// Name returns "docker", "tart", or "native".
	Name() string

	// Preflight is called at agent startup and every five minutes after
	// (RUNNER_SPEC §20). A runner whose Preflight fails is marked unavailable
	// and stops accepting work; recovery is automatic. Never accept a run onto
	// a runner that cannot execute it.
	Preflight(ctx context.Context) error

	// Reap removes what this executor left behind for runs that are no longer
	// live, and reports what it removed.
	//
	// This is RUNNER_SPEC §6 step 5, which says plainly that it is not
	// optional: a leaked container is untidy, a leaked Tart VM carrying an
	// Xcode image is 140 GB provisioned. live reports whether a run id is still in a
	// non-terminal state; anything managed by this executor whose run id is
	// not live is removed.
	//
	// Draft 02 gave the interface no such method, so each implementation grew
	// its own — docker got Discover plus Remove, native got Reap — and the
	// agent would have had to type-switch on a concrete executor to clean up
	// after a crash. That is the coupling A9 removed for EnvFile(), reappearing
	// on the one path that runs before anything else works (amendment A23).
	Reap(ctx context.Context, live func(runID string) bool) ([]Reaped, error)
}

// Reaped is one resource removed by [Executor.Reap], for the operator-facing
// log line. A reap that says nothing is indistinguishable from a reap that
// found nothing.
type Reaped struct {
	RunID string
	// Kind is "container", "vm", or "workspace".
	Kind string
	// Ref identifies it to the underlying tool: a container id, a VM name.
	Ref string
}

// Env is one run's environment.
type Env interface {
	// Exec runs one step and returns its exit code.
	//
	// Every implementation must honour the same contract, because job authors
	// otherwise get different behaviour per executor (RUNNER_SPEC §9):
	//
	// Each step is a NEW PROCESS. Docker performs a fresh exec, tart opens a
	// fresh ssh session, native spawns a fresh `bash -lc`. Working directory,
	// exported variables, shell functions and traps do not survive a step. The
	// filesystem does, for the whole run.
	//
	// Every step starts in the workspace root with [Spec.Env] overlaid by
	// [Step.Env], regardless of what the previous step did.
	//
	// [Step.Run] is handed to the shell as a SINGLE argument — an argv slice,
	// never a concatenated command string (INV-3).
	//
	// stdout and stderr are reported separately (amendment A9). Draft 02 took
	// one writer, which made [protocol.LogFrame.Stream] unfillable: the log
	// pipeline distinguishes the two and the wire type has a field for it,
	// but the executor could not say which was which. Passing the same writer
	// twice is legal for a caller that does not care.
	//
	// The [Result] carries the exit code and, since A42 phase 2, what the step
	// wrote to $GITHUB_OUTPUT. Outputs are a step's result and so belong here
	// rather than on a side channel the caller would have to know to ask
	// for — the same reasoning A9 applied to stdout and stderr.
	Exec(ctx context.Context, step Step, stdout, stderr io.Writer) (Result, error)

	// EnvFile is the environment-visible path to export as $LATEM_ENV.
	//
	// Only the executor knows it — it is a container path for docker, a guest
	// path for tart, a host path for native. Draft 02 had the caller fill
	// [Step.EnvFile], which meant the orchestrator had to import a concrete
	// executor package to get it right, defeating the interface (amendment A9).
	EnvFile() string

	// ActionsDir is the environment-visible path of [Spec.ActionsDir]: where a
	// fetched action's own files can be read from inside a step.
	//
	// Same shape as EnvFile and for the same reason (amendment A9) — only the
	// executor knows it. A host path for native, a mount target for docker.
	// Empty when no action was fetched.
	ActionsDir() string

	// TempDir is the environment-visible path of the run's temporary
	// directory, exported as $RUNNER_TEMP and answered for `runner.temp`
	// (A50). It is emptied with the run and lies outside the workspace, so
	// nothing written there reaches `git status` or a collected artifact.
	TempDir() string

	// Close tears the environment down. It MUST be idempotent and MUST make a
	// best effort even when it has already failed once — a leaked container is
	// untidy, a leaked Tart VM grows toward its image's provisioned 140 GB.
	Close(ctx context.Context) error
}

// Spec describes the environment one run needs.
type Spec struct {
	RunID string
	// Workspace is the host path to the run's workspace: a self-contained git
	// clone, not a worktree, so `git` works inside the environment
	// (RUNNER_SPEC §12, finding F2).
	Workspace string
	Image     string
	// Env is the run environment. It includes resolved secrets, which have
	// already been registered with the redactor before this struct is built.
	Env map[string]string

	// Secrets names the keys of Env that came from a job's secrets: block.
	//
	// RUNNER_SPEC §9 requires $LATEM_ENV to ignore keys colliding with a
	// declared secret name, so that a job cannot overwrite an injected
	// credential. Draft 02 gave the executor no way to tell a secret from any
	// other environment entry, forcing it to protect *every* key in Env — safe,
	// but stricter than the spec, and it silently refused legal overrides of
	// ordinary variables like NODE_ENV (amendment A10).
	//
	// An executor handed an empty Secrets with a non-empty Env must fall back
	// to protecting everything. Failing open here would mean a credential
	// overwrite; failing closed costs a confusing refusal.
	Secrets []string

	// ActionsDir is the host directory holding fetched actions, shared across
	// runs (A42 phase 5). An executor that isolates has to make it reachable
	// from inside; one that does not already has it. Empty means no action
	// was fetched, and nothing need be arranged.
	ActionsDir string

	Caches    []CacheMount
	Labels    map[string]string // includes latem.run-id, for crash reaping
	Resources Resources
}

// Result is what one step produced.
type Result struct {
	// ExitCode is the process's. A non-zero one is not an error: the agent
	// maps it to E_STEP. -1 means the process could not be run or waited for,
	// and the error says why.
	ExitCode int
	// Outputs is what the step wrote to the file named by $GITHUB_OUTPUT,
	// last writer winning. Keys are identifiers; the executor has already
	// dropped anything else and said so in the step's log. Nil when the step
	// wrote nothing, which is nearly always.
	Outputs map[string]string
}

// OutputFileVar names the environment variable through which a step learns
// where to write its outputs. It is GitHub's name, because that is what a
// workflow written for GitHub reads; a latem-specific alias would be a second
// name for the same file with nothing to recommend it.
const OutputFileVar = "GITHUB_OUTPUT"

// GitHubEnvVar is the second name for $LATEM_ENV's path. One file, one
// parser, two names: a workflow written for GitHub appends to $GITHUB_ENV and
// a job written for latem appends to $LATEM_ENV, and since A43 both are the
// same format read by the same code, so there is nothing to keep apart.
const GitHubEnvVar = "GITHUB_ENV"

// PathFileVar and SummaryFileVar name the remaining two files a step may
// write. Directories appended to $GITHUB_PATH are prepended to PATH for every
// later step of the run; $GITHUB_STEP_SUMMARY is read after the step and
// written to the run log, since latem has no summary panel to render it in.
const (
	PathFileVar    = "GITHUB_PATH"
	SummaryFileVar = "GITHUB_STEP_SUMMARY"
)

// WorkspaceVar and RunnerTempVar are set by every executor beside the four
// file paths: the workspace and the run's temporary directory, as the step
// sees them (A50). GitHub's names, for the same reason as GITHUB_OUTPUT. Both
// are set after the run's and the step's environment are merged, so a job
// cannot point them elsewhere.
const (
	WorkspaceVar  = "GITHUB_WORKSPACE"
	RunnerTempVar = "RUNNER_TEMP"
)

// DefaultPath is the PATH a step gets when nothing else supplies one — an
// image with no PATH in its config, for the docker executor. It is the value
// a stock Debian image ships, which is what most CI images are built from.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Step is one unit of work.
type Step struct {
	Name string
	// Run is literal. This is the only place in the entire system where a
	// command may be written, and it comes from the job registry at the
	// resolved commit — never from a broker (INV-2).
	Run string
	// Env is merged over [Spec.Env] for this step only.
	Env map[string]string
	// Timeout bounds this step alone. Zero means "whatever remains of the job
	// budget", so it is opt-in and never tightens a job by accident.
	Timeout time.Duration
	// Shell says how Run reaches a shell. The zero value is the executor's own
	// §9 contract; the others are GitHub's, for a step compiled from a
	// workflow (A50).
	Shell Shell
	// Container, when set, makes this step a container run from another image
	// rather than a command in the run's environment (A44). Run is then empty.
	// Only the docker executor can honour it, and admission has already
	// routed the job there; an executor that cannot answers E_EXECUTOR rather
	// than running nothing and reporting success.
	Container *Container
}

// Shell is how a step's Run is handed to a shell.
type Shell string

const (
	// ShellLatem is §9's contract and the zero value: the executor's own
	// shell, `bash -lc` natively and `bash -c` in a container.
	ShellLatem Shell = ""
	// ShellGitHub is GitHub's for a step that names no shell: `bash -e`.
	ShellGitHub Shell = "github"
	// ShellGitHubBash is GitHub's `shell: bash`: no profile or rc file,
	// errexit and pipefail.
	ShellGitHubBash Shell = "bash"
	// ShellGitHubSh is GitHub's `shell: sh`: `sh -e`.
	ShellGitHubSh Shell = "sh"
)

// Valid reports whether s is one of the shells above.
func (s Shell) Valid() bool {
	switch s {
	case ShellLatem, ShellGitHub, ShellGitHubBash, ShellGitHubSh:
		return true
	}
	return false
}

// GitHubArgv is the argv for one of GitHub's shells, or nil for [ShellLatem],
// whose argv belongs to the executor.
//
// INV-3 holds the same way it does for §9's shells: run is the last element,
// whole, and nothing is joined to it. GitHub writes the script to a file and
// passes the path; `-c` with the text as one argument is the same program
// without a file the step could rewrite under itself.
func GitHubArgv(s Shell, run string) []string {
	switch s {
	case ShellGitHub:
		return []string{"bash", "-e", "-c", run}
	case ShellGitHubBash:
		return []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", run}
	case ShellGitHubSh:
		return []string{"sh", "-e", "-c", run}
	}
	return nil
}

// Container is the image a container step runs and what it runs it with. Args
// is an argv: each element is one argument, whatever it contains (INV-3).
type Container struct {
	Image      string
	Entrypoint string
	Args       []string
}

// CacheMount is one cache made available to a run. Caches are mounted, not
// archived: there is no save step, no restore step, and no size ceiling,
// because these machines are not ephemeral (RUNNER_SPEC §14).
type CacheMount struct {
	Name string
	// Source is the host volume name or directory.
	Source string
	// Target is the path inside the environment, from the registry's
	// caches[].path.
	Target string
}

// Resources bounds a run.
type Resources struct {
	CPUs   float64
	Memory string // "8g"
	GPU    bool
}

// NotImplemented satisfies [Executor] by refusing every call. See
// broker.NotImplemented for why this exists.
type NotImplemented struct{}

var _ Executor = NotImplemented{}

func (NotImplemented) Prepare(context.Context, Spec) (Env, error) {
	return nil, fmt.Errorf("executor: Prepare: %w", errors.ErrUnsupported)
}

func (NotImplemented) Name() string { return "not-implemented" }

func (NotImplemented) Preflight(context.Context) error {
	return fmt.Errorf("executor: Preflight: %w", errors.ErrUnsupported)
}

func (NotImplemented) Reap(context.Context, func(string) bool) ([]Reaped, error) {
	return nil, fmt.Errorf("executor: Reap: %w", errors.ErrUnsupported)
}
