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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
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
	})
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	// Runs until SIGKILL. There is no clean exit from this function by design.
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("child: %v", err)
	}
}

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
// The step's own process is also not reaped by reconciliation, and that is a
// genuine gap rather than a test artefact: §6's reap covers containers, VMs
// and workspaces, and a native step's children are reparented to init by the
// kill. The test tells the stray to exit rather than pretending the agent did.
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
        run: 'touch %s; for i in $(seq 1 1800); do [ -f %s ] && exit 0; sleep 0.1; done; exit 0'
`, started, stop)

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
	before := runRow(t, cfg, "r_kill9")
	if before.State.IsTerminal() {
		t.Fatalf("the run was already %s when the agent was killed; nothing was in flight", before.State)
	}
	t.Logf("after kill -9: run is %s, workspace and executor resource present", before.State)

	// Restart. Everything M4 promises has to happen here, because SIGKILL gave
	// the dying process no chance to do any of it.
	restarted := newTestAgent(t, cfg, func(o *Options) {
		o.NewExecutor = DefaultExecutor
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
