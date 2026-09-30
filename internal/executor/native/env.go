package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// EnvPrefix is the namespace the agent reserves. A job may not set a
	// variable in it through $LATEM_ENV (§9), and none of the agent's own
	// LATEM_* variables are inherited by a step; see [buildEnviron].
	EnvPrefix = "LATEM_"

	// EnvFileVar is the variable naming §9's between-step channel.
	EnvFileVar = "LATEM_ENV"

	// TerminateGrace is how long a step's process group has to exit after
	// SIGTERM before it is sent SIGKILL.
	//
	// Thirty seconds because that is roughly what the things a step leaves
	// behind need to shut down cleanly: a Gradle daemon takes a few seconds, an
	// `xcodebuild` mid-link wants to finish writing the binary it has open, and
	// a simulator asked to stop while booting takes the longest of the three.
	// Killing sooner leaves half-written files in DerivedData that the next run
	// fails on, which converts one timed-out run into two.
	TerminateGrace = 30 * time.Second

	// DefaultPipeDrainDelay bounds how long [exec.Cmd.Wait] may block after the
	// step's process has exited but something still holds its stdout open.
	//
	// A build that starts a background daemon and abandons it — `adb
	// start-server`, the Gradle daemon and `metro` all do this — hands that
	// daemon the step's stdout, and Wait does not return until every writer
	// closes it. Without this bound a step that had already finished would hang
	// until the job budget expired, and the run would fail E_TIMEOUT with a log
	// showing the step succeeding.
	//
	// The consequence a job author can see: when the delay expires the pipe is
	// closed, so a daemon that keeps writing to it gets EPIPE. A step that means
	// to leave a service running for a later step should redirect its output —
	// `mydaemon >/dev/null 2>&1 &` — which is what a job would do on any CI
	// system for the same reason.
	DefaultPipeDrainDelay = 10 * time.Second
)

// runEnv is one run's host environment. It satisfies [executor.Env].
//
// There is no container and no VM to hold: the environment is the host, and all
// this owns is the workspace path, the $LATEM_ENV file, the accumulated
// between-step variables, and whichever step process is running right now.
type runEnv struct {
	spec        executor.Spec
	workspace   string
	shell       string
	envDir      string
	envFile     string
	outFile     string // $GITHUB_OUTPUT, a sibling of envFile
	pathFile    string // $GITHUB_PATH
	summaryFile string // $GITHUB_STEP_SUMMARY
	grace       time.Duration
	drain       time.Duration

	mu      sync.Mutex
	carried map[string]string // accumulated $LATEM_ENV assignments
	// pathAdds is what steps appended to $GITHUB_PATH so far, first entry
	// first on PATH.
	pathAdds []string
	active   map[*stepProc]struct{} // steps in flight
	closed   bool
	removed  bool
}

var _ executor.Env = (*runEnv)(nil)

// EnvFile is the host path exported as $LATEM_ENV.
//
// The orchestrator needs this to describe §9's between-step channel, and only
// the executor knows it: a container path for docker, a guest path for tart, a
// host path here. Draft 02 put it on [executor.Step] and had the caller fill it,
// which forced the orchestrator to import a concrete executor package to get it
// right — defeating the interface (amendment A9).
func (e *runEnv) EnvFile() string { return e.envFile }

// ActionsDir is the host path itself: a step here runs on the host, so a
// fetched action is already where the agent put it.
func (e *runEnv) ActionsDir() string { return e.spec.ActionsDir }

// Workspace is the directory every step starts in. Not part of [executor.Env];
// it exists for diagnostics and for tests.
func (e *runEnv) Workspace() string { return e.workspace }

// StepArgv builds the argv a step is executed with.
//
// INV-3 in one function: `run` is the third element of the slice, whole and
// untouched. There is no place in this package where a step's text is joined to
// anything, and the unit test asserts exactly that, because the failure this
// prevents — a job whose `run` contains a quote reaching a shell as syntax — is
// silent until it is a breach.
//
// The flag is `-lc`, not `-c`. A login shell is what makes an operator's actual
// toolchain visible: Homebrew's PATH entries arrive from /etc/paths.d through
// path_helper in /etc/profile, and rbenv, nvm and asdf are initialised in
// ~/.bash_profile. `bash -c` reads none of those, and the resulting "command not
// found: pod" on a machine where `pod` is plainly installed is one of the more
// wasteful afternoons available to a CI operator.
func StepArgv(shell, run string) []string {
	if shell == "" {
		shell = DefaultShell
	}
	return []string{shell, "-lc", run}
}

// Exec runs one step.
//
// # This is finding F1
//
// Every call is a fresh `bash -lc`, which is a fresh process. Nothing the
// previous step did to its own process survives: not `cd`, not `export`, not a
// shell function, not a trap. So this method sets the working directory
// explicitly and passes the complete merged environment explicitly, every time,
// rather than relying on anything the last step left behind. The filesystem is
// the only thing that carries, and it carries for the whole run.
//
// Draft 01 of RUNNER_SPEC said the opposite, and the first job author to write
// `export PATH=…` in one step and read it in the next would have filed the bug.
// TestExec_ShellStateDoesNotLeakButFilesDo exists to stop the claim quietly
// becoming true again.
//
// # Cancellation
//
// A step that runs past its deadline, or a run that is cancelled, kills the
// step's whole process group — SIGTERM, [TerminateGrace], SIGKILL — because on
// a host with no container around it the alternative is an `xcodebuild` that
// keeps running after the run it belonged to has been reported as failed. See
// [setProcessGroup].
//
// A step that exits non-zero is not an error: the exit code is the result, and
// the agent maps a non-zero one to E_STEP. An error means the step could not be
// run or could not be waited for.
//
// # When the agent itself is killed
//
// Neither the deadline nor [runEnv.Close] runs then, and the step's group is
// reparented to launchd and keeps going. Every step therefore also writes a
// [stepGroup] record into the run's $LATEM_ENV directory, which is what lets
// [Executor.Reap] collect the group after the agent is restarted — §6 step 5 for
// the one resource this executor has.
func (e *runEnv) Exec(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (executor.Result, error) {
	none := executor.Result{ExitCode: -1}
	if step.Container != nil {
		// A backstop, not the gate: admission routes a job with a container
		// step to a docker runner (A44). Reaching here means something
		// bypassed it, and running nothing while reporting success would be
		// the worst available answer.
		return none, codeErr(protocol.ErrExecutor,
			"step %q is a container (%s), which the native executor cannot run", step.Name, step.Container.Image)
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return none, codeErr(protocol.ErrExecutor, "environment for run %s is closed", e.spec.RunID)
	}
	// [baseEnv] first, so the run's own variables and the step's both override
	// it. Amendment A33: the locale is a default the agent supplies, not an
	// assertion it enforces.
	env := mergeEnv(baseEnv(), e.spec.Env, e.carried, step.Env)
	// Both set after the merge, so that a job which wrote either name into
	// $LATEM_ENV cannot point the next step's bookkeeping somewhere else.
	env[EnvFileVar] = e.envFile
	env[executor.GitHubEnvVar] = e.envFile
	env[executor.OutputFileVar] = e.outFile
	env[executor.PathFileVar] = e.pathFile
	env[executor.SummaryFileVar] = e.summaryFile
	if len(e.pathAdds) > 0 {
		// In front of whatever PATH the step would otherwise have had: one
		// set explicitly, else the agent's own, which is what buildEnviron
		// would have inherited. "In front of" is the guarantee, not "first":
		// a step runs under `bash -lc` (§9, A33), and a login profile may
		// prepend its own entries — Homebrew's shellenv and conda's init both
		// do — after this value is set. That is the operator's shell doing
		// what it does everywhere else on the machine, and the docker
		// executor, whose steps have no profile, is where the addition is
		// literally first.
		current, set := env["PATH"]
		if !set {
			current = os.Getenv("PATH")
		}
		env["PATH"] = ghcmd.PrependPath(e.pathAdds, current)
	}
	e.mu.Unlock()

	// A zero Timeout means "whatever remains of the job budget", which is
	// already the deadline on ctx. Only a positive one tightens it.
	execCtx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	code, err := e.run(execCtx, step, env, stdout, stderr)

	// §9's between-step channel. Merged after the step rather than before the
	// next one so that the warnings land in the log of the step that caused
	// them, which is the only place an author will look.
	if mergeErr := e.mergeEnvFile(stdout); mergeErr != nil && err == nil {
		return executor.Result{ExitCode: code}, mergeErr
	}
	// Outputs are read back even from a failed step: GitHub keeps them, and a
	// later `if: failure()` step reading `steps.build.outputs.log` is the
	// idiom that depends on it.
	outputs, outErr := e.readOutputs(stdout)
	if outErr != nil && err == nil {
		return executor.Result{ExitCode: code}, outErr
	}
	if psErr := e.readPathAndSummary(stdout); psErr != nil && err == nil {
		return executor.Result{ExitCode: code}, psErr
	}

	if err != nil {
		// A step-level deadline that fired while the job budget still had room
		// is E_STEP_TIMEOUT, not E_TIMEOUT (§21).
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return none, wrapErr(protocol.ErrStepTimeout, err,
				"step %q exceeded its %s timeout", step.Name, step.Timeout)
		}
		return none, wrapErr(protocol.ErrExecutor, err, "step %q", step.Name)
	}
	return executor.Result{ExitCode: code, Outputs: outputs}, nil
}

// run starts one shell, streams it, and reaps it.
func (e *runEnv) run(
	ctx context.Context, step executor.Step, env map[string]string, stdout, stderr io.Writer,
) (int, error) {
	argv := StepArgv(e.shell, step.Run)

	// #nosec G204 -- this is the INV-3 surface and it is safe by construction.
	// argv is a slice of exactly three elements built by [StepArgv]: argv[0] is
	// the shell resolved from PATH at New and never from a broker, argv[1] is
	// the literal "-lc", and argv[2] is step.Run passed to execve as one
	// element. Nothing is concatenated, so no character in step.Run can be
	// read as argv structure — the shell parses it as a script, which is the
	// documented contract (§9), rather than the agent parsing it as a command
	// line, which would be the injection. step.Run itself comes from the job
	// registry at the resolved commit and never from a broker (INV-2).
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = e.workspace
	cmd.Env = buildEnviron(os.Environ(), env)
	// A nil Stdin gives the step the null device. A build tool that decides to
	// ask a question therefore reads EOF and fails, rather than blocking until
	// the job budget expires with nothing in the log to say what it wanted.
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = e.drain
	setProcessGroup(cmd)

	// Before Start, because the record's descriptor has to be in the child's
	// table from its first instruction — that inheritance is what lets a reap
	// after a crash tell this group from a recycled pid. See [stepGroup].
	group, err := newStepGroup(e.envDir, cmd)
	if err != nil {
		return -1, err
	}
	// Released on every path out of this function, including the two that go
	// through p.stop(), which does not return until the group is gone. A record
	// that outlived its step would make the next reap look at a group that has
	// already been dealt with.
	defer group.release()

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start %s: %w", e.shell, err)
	}
	// A write to a file created a moment ago, so a failure here means the
	// filesystem is in trouble and the step is about to find that out for
	// itself. It is not worth killing a started step over: the cost is one
	// process group that a crash would leak, which is the state every step was
	// in before this record existed.
	_ = group.note(processGroupOf(cmd))

	p := &stepProc{cmd: cmd, grace: e.grace, done: make(chan struct{})}
	go p.reap()

	// Registered so [runEnv.Close] can stop a step that is still running when
	// the run is torn down. Without a container to remove, this is the only
	// thing between an abandoned run and a build that keeps burning the
	// machine's cores after everyone has stopped watching it.
	//
	// track reports false for a Close that landed between [runEnv.Exec]'s
	// closed check and this line. That window is narrow and entirely real: Close
	// would have read an empty set of running steps, and this process — already
	// started — would have run to completion with nobody left to stop it.
	if !e.track(p) {
		p.stop()
		return -1, fmt.Errorf("environment for run %s was closed while the step was starting", e.spec.RunID)
	}
	defer e.untrack(p)

	select {
	case <-p.done:
		return exitStatus(cmd, p.err)
	case <-ctx.Done():
		p.stop()
		return -1, ctx.Err()
	}
}

// stepProc is one running step, and the machinery for stopping it.
//
// Completion is signalled by closing a channel rather than by sending on one,
// which is the whole reason this type exists rather than a bare channel. Two
// goroutines legitimately wait for the same step to finish — [runEnv.Exec], and
// a [runEnv.Close] arriving while the step is still going — and a single send
// can only be received once, so the second waiter would block forever on a
// process that had already exited. A closed channel is readable by everyone,
// and the happens-before it establishes is what makes err safe to read without
// a mutex.
type stepProc struct {
	cmd   *exec.Cmd
	grace time.Duration
	done  chan struct{}
	err   error // valid only after done is closed
}

func (p *stepProc) reap() {
	p.err = p.cmd.Wait()
	close(p.done)
}

// stop is §9's "kills the step's process group": SIGTERM, wait, SIGKILL.
//
// It waits for the process to be reaped rather than returning as soon as the
// signal is sent, so that by the time [runEnv.Exec] returns, the step's tree is
// genuinely gone and `collecting` is not racing an `xcodebuild` still writing
// into the workspace it is about to archive.
//
// Safe to call concurrently and repeatedly: [signalGroup] treats an
// already-dead group as success, and every wait here is a receive from a closed
// channel.
func (p *stepProc) stop() {
	_ = terminateGroup(p.cmd)
	select {
	case <-p.done:
		return
	case <-time.After(p.grace):
	}
	_ = killGroup(p.cmd)
	// Bounded by cmd.WaitDelay, which closes the step's pipes if something
	// outside the group is still holding them open after the leader is reaped.
	<-p.done
}

// track registers a running step so Close can find it, reporting false if the
// environment was closed first.
func (e *runEnv) track(p *stepProc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.active[p] = struct{}{}
	return true
}

func (e *runEnv) untrack(p *stepProc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.active, p)
}

// exitStatus turns what [exec.Cmd.Wait] returned into a step result.
//
// A non-zero exit is a result, not an error: the agent maps it to E_STEP and
// puts the number in the update. [exec.ErrWaitDelay] means the process finished
// but something it left behind held the output pipes past the drain window —
// the step's own status is still the right answer, and the truncated tail is
// noted rather than converted into a failure the job did not have.
func exitStatus(cmd *exec.Cmd, err error) (int, error) {
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.As(err, &exitErr):
		return stepExitCode(cmd.ProcessState), nil
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		return stepExitCode(cmd.ProcessState), nil
	default:
		return -1, err
	}
}

// mergeEnv layers the three environment sources in the order §9 gives them.
//
// Spec.Env is the run environment including resolved secrets; the accumulated
// $LATEM_ENV assignments sit on top of it, because that is what "merges them
// into the run environment for every subsequent step" means; Step.Env is last
// because it is declared in the job for this step specifically, so it must be
// able to override both.
//
// Keys that cannot survive being rendered as KEY=value are dropped rather than
// silently corrupting the whole environment block.
func mergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, layer := range layers {
		for k, v := range layer {
			if !usableEnvKey(k) {
				continue
			}
			out[k] = v
		}
	}
	return out
}

func usableEnvKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}

// inheritDrop are variables from the agent's own environment that describe the
// agent's process rather than the machine, and would therefore be actively
// wrong in a step that runs somewhere else. Both name a directory that is not
// the workspace.
var inheritDrop = map[string]struct{}{
	"PWD":    {},
	"OLDPWD": {},
}

// buildEnviron renders the step's environment, layered over the agent's own.
//
// # Why the agent's environment is inherited at all
//
// Because there is no image here. A container brings its own HOME, its own
// TMPDIR and its own PATH; a host process that is handed neither gets a login
// shell with no HOME, and every tool §13.3 exists to reach — rbenv, nvm,
// CocoaPods, the keychain itself — resolves paths relative to it. Inheriting is
// not a convenience, it is what "runs directly on the host as the logged-in
// user" means.
//
// # Except LATEM_*
//
// Every variable in the agent's own environment under [EnvPrefix] is dropped
// before the run's is applied. The agent may be holding LATEM_SECRET_* for
// another repository's credentials (§15), and a step has no business seeing a
// secret that was never declared in its job's `secrets:` block. Dropping the
// whole namespace rather than only the secret prefix also means an agent
// variable can never be mistaken for one the run defined — the run's LATEM_*
// entries are exactly the ones in env, which is what §9 says a step should see.
//
// # And except the agent's own working directory
//
// A step's working directory is the workspace, and the agent's inherited $PWD
// names wherever the agent was started. bash notices and repairs it; anything
// that is not a shell does not, so a build script asking $PWD where it is gets
// a confident wrong answer. See [inheritDrop].
//
// Later entries win in os/exec, so overriding by appending would work; the
// shadowed entries are removed anyway so the result is correct by construction
// rather than by an implementation detail, and legible in `ps -E`.
func buildEnviron(base []string, env map[string]string) []string {
	out := make([]string, 0, len(base)+len(env))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(name, EnvPrefix) {
			continue
		}
		if _, stale := inheritDrop[name]; stale {
			continue
		}
		if _, overridden := env[name]; overridden {
			continue
		}
		out = append(out, kv)
	}

	// Sorted so that two identical runs produce identical environments and a
	// test can assert on one.
	overlay := make([]string, 0, len(env))
	for k, v := range env {
		if !usableEnvKey(k) {
			continue
		}
		overlay = append(overlay, k+"="+v)
	}
	sort.Strings(overlay)
	return append(out, overlay...)
}

// Close tears the environment down.
//
// Two things, in order. Any step still running is stopped as a process group,
// because there is no container whose removal would do it implicitly and a
// forgotten `xcodebuild` is a machine that stays busy for an hour after its run
// was abandoned. Then the $LATEM_ENV directory is removed; it is outside the
// workspace, so nothing else will ever clean it up.
//
// Idempotent, and it retries after a failure: the second call is not a no-op
// unless the first one actually removed something. The workspace is deliberately
// untouched — §12 owns it, [runEnv.Fetch] still needs it, and an executor that
// deleted a run's clone on Close would take the artifacts with it.
//
// # What Close does not stop
//
// A daemon that a *completed* step started and left running. Removing a
// container takes those with it; here they have been reparented to launchd and
// nothing links them to the run any more. This is a deliberate limitation
// rather than a gap: on a host that is not ephemeral, the surviving daemon is
// usually the point. A Gradle daemon or a warm Metro bundler left running is
// exactly the reuse §14 is built around, and killing every process group a run
// ever created would make the second run on a machine as cold as the first.
// The cost is that a job which leaks a daemon leaks it onto the host, which is
// one more thing "no isolation" means.
func (e *runEnv) Close(_ context.Context) error {
	e.mu.Lock()
	e.closed = true
	running := make([]*stepProc, 0, len(e.active))
	for p := range e.active {
		running = append(running, p)
	}
	alreadyRemoved := e.removed
	e.mu.Unlock()

	for _, p := range running {
		p.stop()
	}

	if alreadyRemoved {
		return nil
	}
	if err := os.RemoveAll(e.envDir); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "remove $LATEM_ENV directory %s", e.envDir)
	}

	e.mu.Lock()
	e.removed = true
	e.mu.Unlock()
	return nil
}
