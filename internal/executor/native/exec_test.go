//go:build unix

package native

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// These tests run real processes. They need bash and a writable directory and
// nothing else — no Xcode, no signing identity, no daemon — so they belong in
// the default gate rather than behind a build tag. An executor whose only
// covered paths were the pure functions would be an executor nobody had run.

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(DefaultShell); err != nil {
		t.Skipf("no %s on PATH: %v", DefaultShell, err)
	}
}

// live prepares an environment over a real workspace.
func live(t *testing.T, spec executor.Spec, opts Options) (*runEnv, string) {
	t.Helper()
	requireShell(t)

	if spec.Workspace == "" {
		spec.Workspace = t.TempDir()
	}
	if spec.RunID == "" {
		spec.RunID = "run-test"
	}
	e := newExecutor(t, opts)
	env, err := e.Prepare(t.Context(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(context.WithoutCancel(t.Context())) })

	ws, err := filepath.EvalSymlinks(spec.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	return env.(*runEnv), ws
}

// run executes one step and returns its exit code and the two streams.
func run(t *testing.T, env *runEnv, step executor.Step) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	res, err := env.Exec(t.Context(), step, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec(%q): %v\nstdout: %s\nstderr: %s", step.Run, err, stdout.String(), stderr.String())
	}
	return res.ExitCode, stdout.String(), stderr.String()
}

// This is finding F1, and it is the reason §9 reads the way it does.
//
// Draft 01 of RUNNER_SPEC claimed `cd` and `export` persist between steps. They
// do not: every step is a new `bash -lc`. The filesystem is the only thing that
// carries. If this test ever fails, the claim has quietly become true again and
// job authors are getting different behaviour per executor.
func TestExec_ShellStateDoesNotLeakButFilesDo(t *testing.T) {
	env, ws := live(t, executor.Spec{}, Options{})

	code, _, _ := run(t, env, executor.Step{
		Name: "leak everything it can",
		Run:  "echo persisted > marker.txt\ncd /tmp\nexport LEAKED=yes\nnotafunction() { :; }",
	})
	if code != 0 {
		t.Fatalf("first step exited %d", code)
	}

	code, stdout, _ := run(t, env, executor.Step{
		Name: "observe",
		Run:  `pwd; echo "LEAKED=[${LEAKED-unset}]"; cat marker.txt; type notafunction 2>/dev/null || echo "no function"`,
	})
	if code != 0 {
		t.Fatalf("second step exited %d: %s", code, stdout)
	}

	if !strings.Contains(stdout, ws) {
		t.Errorf("pwd did not report the workspace %q; got:\n%s", ws, stdout)
	}
	if got, err := filepath.EvalSymlinks(env.Workspace()); err != nil || got != ws {
		t.Errorf("Workspace() = %q, want %q (%v)", env.Workspace(), ws, err)
	}
	if !strings.Contains(stdout, "LEAKED=[unset]") {
		t.Errorf("an exported variable survived the step boundary; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no function") {
		t.Errorf("a shell function survived the step boundary; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "persisted") {
		t.Errorf("the file written by the first step is gone; got:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(ws, "marker.txt")); err != nil {
		t.Errorf("marker.txt is not in the workspace: %v", err)
	}
}

// §13.3 requires a login shell so an operator's Homebrew, rbenv and nvm are
// present. `bash -c` would pass every test a repository's own scripts can
// write, and fail only on the machine.
func TestExec_IsALoginShell(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	code, stdout, _ := run(t, env, executor.Step{
		Run: `shopt -q login_shell && echo "login shell" || echo "NOT a login shell"`,
	})
	if code != 0 || !strings.Contains(stdout, "login shell") || strings.Contains(stdout, "NOT") {
		t.Errorf("step did not run in a login shell (exit %d): %s", code, stdout)
	}
}

// A non-zero exit is a result, not an error: the agent maps it to E_STEP and
// puts the number in the update. Returning an error here would lose the code.
func TestExec_NonZeroExitIsAResult(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	for _, want := range []int{0, 1, 3, 42} {
		code, _, _ := run(t, env, executor.Step{Run: "exit " + strconv.Itoa(want)})
		if code != want {
			t.Errorf("exit %d reported as %d", want, code)
		}
	}
}

// A step killed by a signal reports 128+signal, which is what bash itself
// reports and what a job author expects. os.ProcessState would say -1, making
// "the build was killed" and "the executor lost the status" the same answer.
func TestExec_SignalledStepReportsTheShellConvention(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	code, _, _ := run(t, env, executor.Step{Run: "kill -TERM $$; sleep 5"})
	if code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit code = %d, want %d", code, 128+int(syscall.SIGTERM))
	}
}

// Amendment A9: the two streams are reported separately, because
// protocol.LogFrame.Stream has a field for it and one writer made that field
// unfillable.
func TestExec_StdoutAndStderrAreSeparate(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	var stdout, stderr bytes.Buffer
	if _, err := env.Exec(t.Context(),
		executor.Step{Run: "echo to-stdout; echo to-stderr >&2"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "to-stdout") || strings.Contains(stdout.String(), "to-stderr") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "to-stderr") || strings.Contains(stderr.String(), "to-stdout") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// Passing the same writer twice is legal for a caller that does not care
// (amendment A9), and os/exec takes a different code path for it.
func TestExec_OneWriterForBothIsLegal(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	var both bytes.Buffer
	if _, err := env.Exec(t.Context(),
		executor.Step{Run: "echo a; echo b >&2"}, &both, &both); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(both.String(), "a") || !strings.Contains(both.String(), "b") {
		t.Errorf("combined output = %q", both.String())
	}
}

func TestExec_EnvironmentLayering(t *testing.T) {
	env, _ := live(t, executor.Spec{
		Env: map[string]string{"FROM_SPEC": "spec", "OVERRIDDEN": "spec"},
	}, Options{})

	code, stdout, _ := run(t, env, executor.Step{
		Env: map[string]string{"OVERRIDDEN": "step", "FROM_STEP": "step"},
		Run: `echo "[$FROM_SPEC][$OVERRIDDEN][$FROM_STEP][$HOME]"`,
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "[spec][step][step]") {
		t.Errorf("environment layering wrong: %s", stdout)
	}
	// The host environment is inherited, which is what "runs directly on the
	// host as the logged-in user" means; without HOME, rbenv, nvm, CocoaPods
	// and the keychain all break.
	if strings.Contains(stdout, "[]\n") {
		t.Errorf("HOME was not inherited: %s", stdout)
	}
}

// The environment is rebuilt from Spec.Env on every step, so a variable a step
// unsets or overrides for itself is back to the run's value in the next one.
func TestExec_StepEnvDoesNotPersist(t *testing.T) {
	env, _ := live(t, executor.Spec{Env: map[string]string{"K": "run"}}, Options{})
	run(t, env, executor.Step{Env: map[string]string{"K": "step"}, Run: "true"})

	_, stdout, _ := run(t, env, executor.Step{Run: `echo "[$K]"`})
	if !strings.Contains(stdout, "[run]") {
		t.Errorf("step env leaked into the next step: %s", stdout)
	}
}

// §9's one sanctioned channel between steps.
func TestExec_LatemEnvCarriesBetweenSteps(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})

	if got := env.EnvFile(); got == "" {
		t.Fatal("EnvFile is empty; the orchestrator has nothing to export (amendment A9)")
	}
	code, _, _ := run(t, env, executor.Step{
		Run: `echo "BUILD_NUMBER=$((6*7))" >> "$LATEM_ENV"; echo "GREETING=hello world" >> "$LATEM_ENV"`,
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}

	_, stdout, _ := run(t, env, executor.Step{Run: `echo "[$BUILD_NUMBER][$GREETING]"`})
	if !strings.Contains(stdout, "[42][hello world]") {
		t.Errorf("$LATEM_ENV did not carry: %s", stdout)
	}

	// Truncated between steps, so a value is merged once and does not
	// accumulate: the third step must not re-apply what the first wrote.
	data, err := os.ReadFile(env.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("$LATEM_ENV was not truncated between steps: %q", data)
	}
}

// The warning lands in the log of the step that caused it, which is the only
// place a job author will look.
func TestExec_LatemEnvWarningsReachTheStepsOwnLog(t *testing.T) {
	env, _ := live(t, executor.Spec{
		Env:     map[string]string{"MATCH_PASSWORD": "hunter2"},
		Secrets: []string{"MATCH_PASSWORD"},
	}, Options{})

	var stdout, stderr bytes.Buffer
	_, err := env.Exec(t.Context(), executor.Step{
		Run: `echo "MATCH_PASSWORD=stolen" >> "$LATEM_ENV"; echo "not an assignment" >> "$LATEM_ENV"`,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "MATCH_PASSWORD") {
		t.Errorf("no warning about the refused secret: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "stolen") {
		t.Errorf("the warning echoed the value a job tried to set: %s", stdout.String())
	}

	_, out, _ := run(t, env, executor.Step{Run: `echo "[$MATCH_PASSWORD]"`})
	if !strings.Contains(out, "[hunter2]") {
		t.Errorf("a job overwrote an injected credential through $LATEM_ENV: %s", out)
	}
}

// A step is free to delete $LATEM_ENV. Absent and empty must mean the same
// thing, or an ordinary `rm -rf` in a build script fails the run.
func TestExec_MissingLatemEnvIsNotAnError(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	if _, err := env.Exec(t.Context(), executor.Step{Run: `rm -f "$LATEM_ENV"`}, nil, nil); err != nil {
		t.Fatalf("a step that removed $LATEM_ENV failed the run: %v", err)
	}
}

// §9: exceeding steps[].timeout kills the step's process group and fails the
// run E_STEP_TIMEOUT — not E_TIMEOUT, because the job budget still had room.
//
// The process group is the whole point. `xcodebuild` leaves behind a swift
// frontend and an XCBBuildService; killing only the shell's pid reparents them
// to launchd, still holding DerivedData locks. The background `sleep` here
// stands in for exactly that.
func TestExec_StepTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	env, ws := live(t, executor.Spec{}, Options{TerminateGrace: 500 * time.Millisecond})
	pidFile := filepath.Join(ws, "child.pid")

	start := time.Now()
	// Generous, because the timeout has to outlast a login shell reading the
	// operator's ~/.bash_profile — measured at roughly half a second on the
	// machine this was written on — or the step is killed before it has spawned
	// the child this test is about.
	_, err := env.Exec(t.Context(), executor.Step{
		Name:    "hangs, with a child",
		Run:     `sleep 120 & echo $! > child.pid; sleep 120`,
		Timeout: 3 * time.Second,
	}, nil, nil)

	if err == nil {
		t.Fatal("a step that ran past its timeout succeeded")
	}
	if got := CodeOf(err); got != protocol.ErrStepTimeout {
		t.Fatalf("code = %q, want %q (err: %v)", got, protocol.ErrStepTimeout, err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the step took %s to be killed", elapsed)
	}

	child := readPID(t, pidFile)
	if child == 0 {
		t.Fatal("the step never recorded its child's pid; the test proved nothing")
	}
	if alive := waitForExit(child, 5*time.Second); alive {
		t.Errorf("child %d outlived the step; the kill did not reach the process group", child)
	}
}

// SIGTERM is sent first so a build tool can flush its logs and remove its lock
// files. A process that ignores it must still die.
//
// The step signals readiness through a file rather than the test guessing a
// timeout, because the trap has to be installed before the signal arrives and a
// login shell's startup time is the operator's ~/.bash_profile, not something
// this package controls.
func TestExec_TerminateEscalatesToKill(t *testing.T) {
	const grace = 400 * time.Millisecond
	env, ws := live(t, executor.Spec{}, Options{TerminateGrace: grace})

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	var start time.Time
	go func() {
		_, err := env.Exec(ctx, executor.Step{
			Name: "ignores SIGTERM",
			Run:  `trap "" TERM; touch trapped; while :; do :; done`,
		}, nil, nil)
		done <- result{err, time.Since(start)}
	}()

	waitForFile(t, filepath.Join(ws, "trapped"))
	start = time.Now()
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a step ignoring SIGTERM was reported as successful")
		}
		// SIGTERM alone could not have ended it, so the grace must have
		// elapsed and SIGKILL must be what worked.
		if got.elapsed < grace {
			t.Errorf("the step ended after %s, sooner than the %s grace; "+
				"SIGTERM appears to have been honoured, so this proved nothing about SIGKILL",
				got.elapsed, grace)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a step ignoring SIGTERM never died; the escalation to SIGKILL did not happen")
	}
}

// A cancelled run — §11's cancel, or the job budget expiring — is not a step
// timeout. Both stop the step; only one is E_STEP_TIMEOUT (§21).
func TestExec_JobCancellationIsNotAStepTimeout(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{TerminateGrace: 300 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	_, err := env.Exec(ctx, executor.Step{Name: "long", Run: "sleep 120"}, nil, nil)
	if err == nil {
		t.Fatal("a cancelled step succeeded")
	}
	if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("code = %q, want %q — a cancellation is not E_STEP_TIMEOUT", got, protocol.ErrExecutor)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not unwrap to context.Canceled", err)
	}
}

// There is no container whose removal implicitly stops everything, so Close has
// to. Without this an abandoned run leaves an xcodebuild burning the machine's
// cores for the next forty minutes.
func TestClose_StopsAStepThatIsStillRunning(t *testing.T) {
	env, ws := live(t, executor.Spec{}, Options{TerminateGrace: 300 * time.Millisecond})
	pidFile := filepath.Join(ws, "child.pid")

	done := make(chan error, 1)
	go func() {
		_, err := env.Exec(context.WithoutCancel(t.Context()), executor.Step{
			Name: "runs past its welcome",
			Run:  `sleep 120 & echo $! > child.pid; sleep 120`,
		}, nil, nil)
		done <- err
	}()

	waitForFile(t, pidFile)
	child := readPID(t, pidFile)
	if child == 0 {
		t.Fatal("the step never started")
	}

	if err := env.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Exec did not return after Close")
	}
	if alive := waitForExit(child, 5*time.Second); alive {
		t.Errorf("child %d survived Close", child)
	}
}

// A step that hands its stdout to a daemon it abandons would otherwise keep
// Wait blocked until the job budget expired, long after the step itself
// finished. cmd.WaitDelay bounds it.
func TestExec_AbandonedChildDoesNotHoldTheStepOpen(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{PipeDrainDelay: time.Second})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var out bytes.Buffer
		if _, err := env.Exec(t.Context(),
			executor.Step{Run: `sleep 60 & echo done`}, &out, &out); err != nil {
			t.Errorf("Exec: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a step whose abandoned child held stdout open never returned")
	}
}

// The production [Runner]. Every other test of the host probes uses a fake, so
// without this the thing that actually shells out is the one part of the
// preflight path nobody has run.
func TestExecRunner_RealCommands(t *testing.T) {
	t.Run("stdout is returned", func(t *testing.T) {
		out, err := ExecRunner{}.Run(t.Context(), []string{"echo", "hello"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if strings.TrimSpace(string(out)) != "hello" {
			t.Errorf("stdout = %q", out)
		}
	})

	// The keychain probe's whole design rests on this: a timeout must unwrap to
	// DeadlineExceeded so [CheckKeychain] can tell "did not answer" — which
	// means a password dialog nobody can reach — from "answered with an error".
	t.Run("a timeout unwraps to DeadlineExceeded", func(t *testing.T) {
		start := time.Now()
		_, err := ExecRunner{Timeout: 100 * time.Millisecond}.Run(t.Context(), []string{"sleep", "30"})
		if err == nil {
			t.Fatal("a command that outran its timeout succeeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not unwrap to DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("the timeout took %s to fire", elapsed)
		}
	})

	t.Run("a non-zero exit carries the tool's own stderr", func(t *testing.T) {
		_, err := ExecRunner{}.Run(t.Context(),
			[]string{"sh", "-c", "echo 'User interaction is not allowed.' >&2; exit 1"})
		if err == nil {
			t.Fatal("a failing command succeeded")
		}
		if !strings.Contains(err.Error(), "User interaction") {
			t.Errorf("error = %q, want the tool's own diagnosis in it", err)
		}
	})

	t.Run("a missing program is an error, not a panic", func(t *testing.T) {
		if _, err := (ExecRunner{}).Run(t.Context(), []string{"no-such-program-anywhere"}); err == nil {
			t.Error("running a program that does not exist succeeded")
		}
	})
}

func TestExec_StdinIsTheNullDevice(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	code, stdout, _ := run(t, env, executor.Step{Run: `read -r line && echo "read [$line]" || echo "eof"`})
	if code == 0 && !strings.Contains(stdout, "eof") {
		t.Errorf("a step that read stdin got %q; it must see EOF, not block", stdout)
	}
}

// The workspace is the run's clone directly, no copy (§13.3). Because §12
// produces a self-contained repository, git behaves here exactly as it does
// inside a container — which is finding F2's regression test, in the one place
// where it is nearly free to run.
func TestExec_GitWorksInTheWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	ws := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "--allow-empty", "-m", "one", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git setup failed: %v: %s", err, out)
		}
	}

	env, _ := live(t, executor.Spec{Workspace: ws}, Options{})
	code, stdout, stderr := run(t, env, executor.Step{Run: "git rev-parse HEAD"})
	if code != 0 {
		t.Fatalf("git rev-parse exited %d: %s", code, stderr)
	}
	if len(strings.TrimSpace(stdout)) != 40 {
		t.Errorf("git rev-parse printed %q", stdout)
	}
}

// waitForFile blocks until a step has written the file it uses to say it has
// got as far as the test needs it to.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the step never created %s", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a path this test just built
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// waitForExit polls until the pid is gone, and reports whether it is still
// alive at the deadline. Signal 0 checks for existence without delivering
// anything.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == nil
}

// The four GitHub files, through real shells (A42 phase 2, A43). $GITHUB_ENV
// is $LATEM_ENV under its other name and now speaks the heredoc; $GITHUB_PATH
// goes in front of the next step's PATH; $GITHUB_OUTPUT comes back on the
// Result; $GITHUB_STEP_SUMMARY lands in the step's own log.
func TestExec_GitHubFiles(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})

	var stdout, stderr bytes.Buffer
	res, err := env.Exec(t.Context(), executor.Step{Run: `
printf 'NOTES<<EOF\nfirst line\nsecond line\nEOF\n' >> "$GITHUB_ENV"
echo "/opt/latem-test-bin" >> "$GITHUB_PATH"
echo "sha=abc123" >> "$GITHUB_OUTPUT"
printf 'log=%s\n' "build.log" >> "$GITHUB_OUTPUT"
echo "## Built 3 targets" >> "$GITHUB_STEP_SUMMARY"
`}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d\n%s%s", res.ExitCode, stdout.String(), stderr.String())
	}
	if res.Outputs["sha"] != "abc123" || res.Outputs["log"] != "build.log" {
		t.Errorf("outputs = %+v", res.Outputs)
	}
	if !strings.Contains(stdout.String(), "[latem] summary: ## Built 3 targets") {
		t.Errorf("the summary should be in the step's log: %s", stdout.String())
	}

	_, out, _ := run(t, env, executor.Step{Run: `printf '[%s]\n' "$NOTES"; echo "$PATH"`})
	if !strings.Contains(out, "[first line\nsecond line]") {
		t.Errorf("a heredoc value did not carry through $GITHUB_ENV: %q", out)
	}
	// The guarantee is "ahead of everything the agent supplied", not "first":
	// steps run under `bash -lc` (§9, A33), and a login profile is free to
	// prepend its own entries after the agent set PATH — Homebrew's shellenv
	// and conda's init both do. So the addition must precede the agent's own
	// PATH, which is what it was put in front of; what a profile does on top
	// is the operator's shell, not the executor's.
	// A profile reshapes PATH rather than merely prepending to it, so the
	// agent's PATH is not present verbatim; its last component is, because
	// profiles prepend. The addition must come before that.
	agentPath := strings.Split(os.Getenv("PATH"), ":")
	tail := agentPath[len(agentPath)-1]
	add, agent := strings.Index(out, "/opt/latem-test-bin:"), strings.LastIndex(out, tail)
	if tail == "" || add < 0 || agent < 0 || add > agent {
		t.Errorf("$GITHUB_PATH addition should precede the agent's PATH (add at %d, agent tail %q at %d): %q", add, tail, agent, out)
	}

	// Truncated between steps: a second read of each file finds nothing.
	for _, f := range []string{env.outFile, env.pathFile, env.summaryFile} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 0 {
			t.Errorf("%s was not truncated between steps: %q", f, data)
		}
	}
}

// A backstop, not the gate — admission routes container jobs to docker (A44).
// But a container step that did reach here must be an error, never a step that
// ran nothing and reported success.
func TestExec_ContainerStepIsRefused(t *testing.T) {
	env, _ := live(t, executor.Spec{}, Options{})
	res, err := env.Exec(t.Context(), executor.Step{
		Name: "lint", Container: &executor.Container{Image: "alpine:3.19"},
	}, nil, nil)
	if err == nil || res.ExitCode == 0 {
		t.Fatalf("a container step on the native executor must fail: %+v, %v", res, err)
	}
	if !strings.Contains(err.Error(), "alpine:3.19") {
		t.Errorf("the error should name the image: %v", err)
	}
}
