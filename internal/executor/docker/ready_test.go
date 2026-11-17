package docker

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// LAT-23: Kiri 0.2.1 answered about one exec in ten that came right after a
// container's start with "stat /bin/sh: no such file or directory", exit 127.
// The engine's lag must be waited out before the first step, not reported as
// the step's failure.
func TestPrepare_WaitsOutAnEngineThatHasNoShellRightAfterTheStart(t *testing.T) {
	f := &fakeAPI{readyFailures: 1, envFileMissing: true}
	e, ws := newExecutor(t, f, func(o *Options) { o.User = "1000:1000" })

	env, err := e.Prepare(context.Background(), executor.Spec{RunID: "run-1", Workspace: ws})
	if err != nil {
		t.Fatalf("Prepare failed where one retry would have found the shell: %v", err)
	}
	if len(f.readyExecs) != 2 {
		t.Fatalf("got %d readiness execs, want 2: the refused one and the one that ran", len(f.readyExecs))
	}
	for i, o := range f.readyExecs {
		if o.User != "1000:1000" || o.WorkingDir != WorkspacePath {
			t.Errorf("readiness exec %d ran as %q in %q, want the run user in %s as a step runs",
				i, o.User, o.WorkingDir, WorkspacePath)
		}
	}
	if len(f.execOpts) != 0 {
		t.Errorf("Prepare ran a step's exec: %v", f.execOpts)
	}

	res, err := env.Exec(context.Background(), executor.Step{Name: "first", Run: "uname -m"}, io.Discard, io.Discard)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("the first step after a late engine: %+v, %v", res, err)
	}
}

func TestPrepare_AnEngineThatNeverHasAShellIsAnExecutorFailure(t *testing.T) {
	f := &fakeAPI{readyNever: true}
	e, ws := newExecutor(t, f, nil)
	e.readyBudget = 100 * time.Millisecond

	start := time.Now()
	env, err := e.Prepare(context.Background(), executor.Spec{RunID: "run-1", Workspace: ws})
	if err == nil {
		t.Fatal("Prepare succeeded although no exec ever found /bin/sh")
	}
	if env != nil {
		t.Errorf("Prepare returned an env with its error")
	}
	if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("code = %q, want %s: no step ran, so none failed (%v)", got, protocol.ErrExecutor, err)
	}
	if !strings.Contains(err.Error(), "never readied") || !strings.Contains(err.Error(), "stat /bin/sh") {
		t.Errorf("the error does not say the engine never readied the container, nor what it answered: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Prepare took %s to give up on a 100ms budget", took)
	}
	// Tries at 0, 10, 30 and 70 ms, then a pause cut to the 30 ms left: 5.
	if n := len(f.readyExecs); n < 3 || n > 6 {
		t.Errorf("got %d readiness execs in 100ms, want a doubling backoff from 10ms (5)", n)
	}
	if len(f.removed) != 1 {
		t.Errorf("the container that never readied was not removed: %v", f.removed)
	}
	if len(f.execOpts) != 0 {
		t.Errorf("a step ran in a container that never readied: %v", f.execOpts)
	}
}

// Only the readiness exec is retried. A step that exits 127 failed, whatever
// it printed, and is run once.
func TestExec_AStepsOwnExit127IsTheStepsAndIsNotRetried(t *testing.T) {
	f := &fakeAPI{
		envFileMissing: true,
		execExitCodes:  []int{127},
		execStderr:     []string{kiriShellMissing},
	}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: t.TempDir()})

	res, err := env.Exec(context.Background(), executor.Step{Name: "missing", Run: "no-such-tool"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("a step's exit 127 became an executor error: %v", err)
	}
	if res.ExitCode != 127 {
		t.Errorf("exit = %d, want the step's own 127", res.ExitCode)
	}
	if len(f.execOpts) != 1 {
		t.Errorf("the step ran %d times, want once", len(f.execOpts))
	}
}

func TestPrepare_WaitsForTheContainerToRunWithAPid(t *testing.T) {
	f := &fakeAPI{inspectStates: []container.State{
		{Status: container.StateCreated},
		{Status: container.StateRunning, Running: true},
		{Status: container.StateRunning, Running: true, Pid: 7},
	}}
	e, ws := newExecutor(t, f, nil)
	if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "run-1", Workspace: ws}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if f.inspects != 3 {
		t.Errorf("inspected %d times, want 3: until running with a pid", f.inspects)
	}
	if len(f.readyExecs) != 1 {
		t.Errorf("got %d readiness execs, want 1 once it ran", len(f.readyExecs))
	}
}

func TestPrepare_AContainerThatExitedBeforeItsFirstStepIsAnExecutorFailure(t *testing.T) {
	f := &fakeAPI{inspectStates: []container.State{
		{Status: container.StateExited, ExitCode: 1, Error: "no such image layer"},
	}}
	e, ws := newExecutor(t, f, nil)
	_, err := e.Prepare(context.Background(), executor.Spec{RunID: "run-1", Workspace: ws})
	if CodeOf(err) != protocol.ErrExecutor || !strings.Contains(err.Error(), "no such image layer") {
		t.Fatalf("got %v, want E_EXECUTOR with the engine's reason", err)
	}
	if f.inspects != 1 || len(f.readyExecs) != 0 {
		t.Errorf("an exited container was waited on: %d inspects, %d execs", f.inspects, len(f.readyExecs))
	}
}
