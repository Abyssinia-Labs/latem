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
	Exec(ctx context.Context, step Step, stdout, stderr io.Writer) (exitCode int, err error)

	// EnvFile is the environment-visible path to export as $LATEM_ENV.
	//
	// Only the executor knows it — it is a container path for docker, a guest
	// path for tart, a host path for native. Draft 02 had the caller fill
	// [Step.EnvFile], which meant the orchestrator had to import a concrete
	// executor package to get it right, defeating the interface (amendment A9).
	EnvFile() string

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

	Caches    []CacheMount
	Labels    map[string]string // includes latem.run-id, for crash reaping
	Resources Resources
}

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
