//go:build unix

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// envKill9Dir turns this test binary into an agent for the child half of
// TestKill9LeavesNoOrphansAfterRestart.
//
// Re-execing the test binary is how the crash is made real. A test cannot
// SIGKILL itself and keep running to make assertions, and a simulated crash —
// dropping the orchestrator on the floor, or calling the reconciler by hand —
// would exercise the recovery while quietly assuming the thing recovery exists
// to survive. The child here is a genuine `latem agent` process: it takes the
// flock, opens sqlite, claims a run, starts a shell, and is then killed with
// signal 9 by its parent.
const envKill9Dir = "LATEM_TEST_KILL9_DIR"

// TestKill9AgentChild is the child process. It is a no-op unless the parent
// asked for it.
func TestKill9AgentChild(t *testing.T) {
	dir := os.Getenv(envKill9Dir)
	if dir == "" {
		t.Skip("child process entry point; TestKill9LeavesNoOrphansAfterRestart invokes it")
	}

	cfg, err := config.Load(config.Options{Dir: dir, Home: filepath.Dir(dir)})
	if err != nil {
		t.Fatalf("child: loading config: %v", err)
	}
	a, err := New(Options{
		Config:        cfg,
		Version:       "kill9-child",
		Logger:        testLogger(t),
		ClaimInterval: 100 * time.Millisecond,
		NewExecutor:   crashExecutor,
	})
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	// Runs until SIGKILL. There is no clean exit from this function by design.
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("child: %v", err)
	}
}

// crashExecutor is the real executor without its preflight.
//
// Native preflight asks this Mac about its session and its Xcode, and this test
// is about what a crash leaves behind. A real answer would make the test report
// on the machine: a runner refused for a licence waits for a preflight five
// minutes off, and the failure reads as a step that never started.
func crashExecutor(r *config.Runner, paths config.Paths) (executor.Executor, error) {
	e, err := DefaultExecutor(r, paths)
	if err != nil {
		return nil, err
	}
	return noPreflight{e}, nil
}

type noPreflight struct{ executor.Executor }

func (noPreflight) Preflight(context.Context) error { return nil }

// The M4 acceptance criterion, demonstrated rather than asserted:
//
//	kill -9 mid-run leaves zero orphaned containers, VMs or workspaces after
//	restart, and the run reads interrupted.
//
// # What this covers
//
// A real second process runs a real agent against a real git checkout and a
// real executor. It is killed with SIGKILL — no defer runs, no teardown, no
// signal reaches the step — while a run is executing. The test then asserts,
// before restarting anything, that the crash did leave litter: a workspace, an
// executor resource, and a run row still saying `running`. Without that
// negative control the assertions afterwards would pass just as happily
// against an agent that had never started.
//
// # What it does not cover
//
// The executor here is native, not docker, so "container" and "VM" are stood
// in for by the native executor's own $LATEM_ENV directory. The docker variant
// with real containers is TestKill9LeavesNoOrphanedContainers in
// kill9_docker_test.go, behind the integration build tag, so the default gate
// stays daemon-free.
//
// What *was* written here, and was true when it was written, is that the step's
// own process survives reconciliation. It does not any more: the native
// executor records each step's process group before the step starts, and Reap
// signals those groups for runs that are no longer live. So the stray is
// asserted rather than described — the step writes its process group id down,
// and the restart has to kill it.
//
// What remains genuinely outside the reap is a process that *left* its group:
// a step that calls `setsid`, or a daemon deliberately detached from the build.
// §13.3 says so in its own words — a surviving daemon is usually the point on a
// host that is not ephemeral — and no bookkeeping can find something that has
// removed itself from the only record of its existence.
func TestKill9LeavesNoOrphansAfterRestart(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed; the native executor needs it")
	}

	// A directory outside the config tree, so the sentinel survives the
	// workspace being deleted.
	signals := shortTempDir(t)
	stop := filepath.Join(signals, "stop")
	started := filepath.Join(signals, "started")
	// `$$` in the step's shell is the process group leader, because the
	// executor starts every step with Setpgid. Written down so the reap can be
	// checked rather than assumed.
	pgidFile := filepath.Join(signals, "pgid")

	// The step announces itself, then waits for the parent to release it. A
	// plain `sleep` would leave a stray process for its whole duration after
	// the agent is killed, because the kill does not reach the step's own
	// process group.
	jobs := fmt.Sprintf(`version: 1

jobs:
  test:
    runs_on: [test]
    timeout: 5m
    steps:
      - name: hold
        run: 'echo $$ > %s; touch %s; for i in $(seq 1 1800); do [ -f %s ] && exit 0; sleep 0.1; done; exit 0'
`, pgidFile, started, stop)

	repo := shortTempDir(t)
	newRepo(t, repo, jobs)
	cfg := newConfigDir(t, configOptions{repoURL: repo})

	// Release any stray step process however the test ends.
	t.Cleanup(func() { _ = os.WriteFile(stop, []byte("stop\n"), 0o600) })

	enqueueDirectly(t, cfg, "r_kill9", "main")

	child := startChildAgent(t, cfg.Paths.Dir)

	// Wait until the run is genuinely executing: the step is running, so an
	// environment exists and a workspace exists.
	waitFor(t, "the child agent's step to start", 90*time.Second, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	workspace := filepath.Join(cfg.Paths.Work, "r_kill9")
	envDir := cfg.Paths.RunnerCacheDir("test")
	waitFor(t, "the run's workspace to exist", 30*time.Second, func() bool {
		_, err := os.Stat(workspace)
		return err == nil
	})

	// SIGKILL: no defer, no teardown, no signal to the step. This is the crash.
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9: %v", err)
	}
	_ = child.Wait()

	// The negative control. If the crash left nothing behind, everything below
	// would pass without reconciliation having done anything at all.
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("the crash left no workspace, so this test cannot fail: %v", err)
	}
	if !hasEnvDir(t, envDir, "r_kill9") {
		t.Fatal("the crash left no executor resource, so this test cannot fail")
	}
	// The build is still running: SIGKILL to the agent reached nothing else, so
	// an abandoned `xcodebuild` would keep the machine busy while the next run
	// started beside it. Captured here, as a control — asserting the reap after
	// the restart means nothing unless there was something to reap.
	strayGroup := livingStepGroup(t, pgidFile)

	before := runRow(t, cfg, "r_kill9")
	if before.State.IsTerminal() {
		t.Fatalf("the run was already %s when the agent was killed; nothing was in flight", before.State)
	}
	t.Logf("after kill -9: run is %s, workspace and executor resource present", before.State)

	// Restart. Everything M4 promises has to happen here, because SIGKILL gave
	// the dying process no chance to do any of it.
	restarted := newTestAgent(t, cfg, func(o *Options) {
		o.NewExecutor = crashExecutor
		// Nothing should be claimed during this phase: the queue is empty and
		// the interrupted run is terminal.
		o.ClaimInterval = time.Hour
	})
	restarted.run(t)

	after := runRow(t, cfg, "r_kill9")
	if after.State != protocol.StateInterrupted {
		t.Errorf("after restart the run reads %s, want interrupted (M4)", after.State)
	}
	if after.ErrorCode != protocol.ErrInterrupted {
		t.Errorf("error code = %q, want %s", after.ErrorCode, protocol.ErrInterrupted)
	}
	if after.FinishedAt == nil {
		t.Error("an interrupted run must be stamped finished")
	}

	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the orphaned workspace survived the restart (M4): %v", err)
	}
	if hasEnvDir(t, envDir, "r_kill9") {
		t.Error("the orphaned executor resource survived the restart (M4)")
	}

	// And the step itself, which is the claim this test used to describe as an
	// open gap. Reconciliation signals the process group the executor recorded
	// before the step started; without it the abandoned build keeps running.
	assertGroupReaped(t, strayGroup)
}

// livingStepGroup reads the process group the step wrote down and requires it
// to still exist.
//
// `kill(-pgid, 0)` asks "is there anything in this group" without signalling
// it.
func livingStepGroup(t *testing.T, pgidFile string) int {
	t.Helper()

	raw, err := os.ReadFile(pgidFile) //nolint:gosec // a path this test constructed
	if err != nil {
		t.Fatalf("the step never recorded its process group, so the reap cannot be checked: %v", err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pgid <= 1 {
		t.Fatalf("the step recorded %q as its process group", strings.TrimSpace(string(raw)))
	}
	if syscall.Kill(-pgid, 0) != nil {
		t.Fatalf("the crashed step's process group %d is already gone, so the reap assertion "+
			"below would pass against an agent that reaped nothing. The step polls for a file "+
			"this test has not written yet, so it should still be running here.", pgid)
	}
	return pgid
}

// assertGroupReaped waits for a process group to stop existing.
//
// Generous, because the reap sends SIGTERM and escalates to SIGKILL after a
// grace: the answer that matters is whether the group goes at all.
func assertGroupReaped(t *testing.T, pgid int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the crashed step's process group %d is still running after the restart: "+
		"reconciliation reaped the workspace and the executor resource but left the build "+
		"itself on the machine (§6, §13.3)", pgid)
}

// startChildAgent re-execs this test binary as an agent and waits for it to
// take the lock.
func startChildAgent(t *testing.T, configDir string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestKill9AgentChild$", "-test.v")
	cmd.Env = append(os.Environ(), envKill9Dir+"="+configDir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Its own process group, so a stray child cannot be signalled by accident
	// and so the kill below targets exactly one process — which is what a
	// crash looks like.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child agent: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if t.Failed() {
			t.Logf("child agent output:\n%s", out.String())
		}
	})

	waitFor(t, "the child agent to take the lock", 60*time.Second, func() bool {
		pid := readPID(filepath.Join(configDir, "agent.lock"))
		return pid == cmd.Process.Pid
	})
	return cmd
}

// hasEnvDir reports whether the native executor's per-run directory is still
// there. It is the native executor's answer to a container.
func hasEnvDir(t *testing.T, dir, runID string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), runID) {
			return true
		}
	}
	return false
}

// runRow reads one run straight from sqlite.
func runRow(t *testing.T, cfg *config.Config, id string) store.Run {
	t.Helper()
	db := openStore(t, cfg)
	got, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("reading run %s: %v", id, err)
	}
	return got
}
