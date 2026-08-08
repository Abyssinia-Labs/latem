//go:build integration && unix

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// dockerTestImage is small, has /bin/sh for the container's holding process,
// and has bash for the step contract (§9). Nothing else about it matters here.
const dockerTestImage = "bash:5"

// The M4 acceptance criterion against real containers.
//
// TestKill9LeavesNoOrphansAfterRestart proves the same claim with the native
// executor and no daemon, so the default gate stays runnable everywhere. This
// one closes the gap that test names: a `latem.run-id`-labelled container is
// what §6 step 5 is actually written about, and the reaping path through
// docker's Discover and Remove is not exercised by a directory of marker files.
//
// Run it with: go test -tags=integration ./internal/agent/
func TestKill9LeavesNoOrphanedContainers(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed")
	}
	if err := dockerReachable(t); err != nil {
		t.Skipf("docker is not reachable: %v", err)
	}
	pullImage(t, dockerTestImage)

	signals := shortTempDir(t)
	stop := filepath.Join(signals, "stop")
	started := filepath.Join(signals, "started")

	// The step announces itself on the host — the signals directory is not
	// inside the workspace, so it is not bind-mounted — by touching a file the
	// container can see through the workspace mount is not possible, so the
	// announcement is made by the container's own existence instead. The wait
	// loop is what keeps the run alive until the kill.
	jobs := `version: 1

jobs:
  test:
    runs_on: [docker]
    timeout: 5m
    steps:
      - name: hold
        run: 'for i in $(seq 1 1800); do sleep 0.1; done'
`
	repo := shortTempDir(t)
	newRepo(t, repo, jobs)

	cfg := newConfigDir(t, configOptions{
		repoURL: repo,
		runners: fmt.Sprintf(`
[[runner]]
name        = "docker-1"
executor    = "docker"
labels      = ["docker"]
concurrency = 1

  [runner.docker]
  default_image = %q
  min_free_gb   = 0.001
`, dockerTestImage),
	})
	t.Cleanup(func() {
		_ = os.WriteFile(stop, []byte("stop\n"), 0o600)
		_ = os.Remove(started)
		removeContainer(t, "latem-run-r_dockerkill")
	})

	enqueueDirectly(t, cfg, "r_dockerkill", "main")
	child := startChildAgent(t, cfg.Paths.Dir)

	waitFor(t, "the run's container to exist", 120*time.Second, func() bool {
		return len(containersForRun(t, "r_dockerkill")) > 0
	})
	workspace := filepath.Join(cfg.Paths.Work, "r_dockerkill")

	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill -9: %v", err)
	}
	_ = child.Wait()

	// The negative control: the crash must have left the container and the
	// workspace, or the assertions after the restart prove nothing.
	if got := containersForRun(t, "r_dockerkill"); len(got) == 0 {
		t.Fatal("the crash left no container, so this test cannot fail")
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("the crash left no workspace, so this test cannot fail: %v", err)
	}
	t.Log("after kill -9: container and workspace present")

	restarted := newTestAgent(t, cfg, func(o *Options) {
		o.NewExecutor = DefaultExecutor
		o.ClaimInterval = time.Hour
	})
	restarted.run(t)

	if got := containersForRun(t, "r_dockerkill"); len(got) != 0 {
		t.Errorf("orphaned containers survived the restart (M4): %v", got)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Errorf("the orphaned workspace survived the restart (M4): %v", err)
	}

	after := runRow(t, cfg, "r_dockerkill")
	if after.State != protocol.StateInterrupted {
		t.Errorf("after restart the run reads %s, want interrupted (M4)", after.State)
	}
	if after.ErrorCode != protocol.ErrInterrupted {
		t.Errorf("error code = %q, want %s", after.ErrorCode, protocol.ErrInterrupted)
	}
}

func dockerReachable(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run()
}

func pullImage(t *testing.T, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "pull", ref).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", ref, err, out)
	}
}

// containersForRun lists every container the agent labelled with this run id,
// running or not. Reconciliation must remove all of them.
func containersForRun(t *testing.T, runID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=latem.run-id="+runID, "--format", "{{.ID}} {{.Names}}")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

func removeContainer(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
}
