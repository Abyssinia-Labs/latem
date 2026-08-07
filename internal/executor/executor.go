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
	Exec(ctx context.Context, step Step, out io.Writer) (exitCode int, err error)

	// Fetch copies paths matching glob out of the environment, for artifact
	// collection. The glob is relative to the workspace root and must not
	// escape it.
	Fetch(ctx context.Context, glob string) ([]File, error)

	// Close tears the environment down. It MUST be idempotent and MUST make a
	// best effort even when it has already failed once — a leaked container is
	// untidy, a leaked Tart VM carrying an Xcode image costs ~90 GB.
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
	Env       map[string]string
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
	// EnvFile is the environment-visible path exported as $LATEM_ENV. A step
	// may append KEY=value lines to it; the agent merges them into the run
	// environment for subsequent steps, subject to the constraints in
	// RUNNER_SPEC §9. It is the only sanctioned channel between steps, because
	// shell state does not survive one.
	EnvFile string
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

// File is one artifact retrieved by [Env.Fetch].
type File struct {
	// Path is relative to the workspace root.
	Path      string
	SizeBytes int64
	Mode      uint32
	Content   io.ReadCloser
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
