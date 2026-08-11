//go:build integration

package integration

// RUNNER_SPEC §24's integration matrix, minus the cases that already exist as
// package tests.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fullRunJobs is the fixture registry for the end-to-end cases.
const fullRunJobs = `version: 1

jobs:
  build:
    description: a whole run, end to end
    runs_on: [test]
    timeout: 5m
    inputs:
      channel:
        type: enum
        values: [internal, external]
        default: internal
    artifacts:
      - path: dist/*.txt
        retain: 1h
    steps:
      - name: prepare
        run: mkdir -p dist && echo hello > dist/one.txt
      - name: stamp
        run: echo "$RELEASE_CHANNEL" > dist/channel.txt
        env:
          RELEASE_CHANNEL: ${input.channel}
      - name: verify
        run: test -f dist/one.txt && cat dist/channel.txt

  gitinfo:
    description: the F2 regression — git works inside the environment
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.txt
    steps:
      - name: git works here
        run: |
          status=$(git status --porcelain)
          mkdir -p dist
          git rev-parse HEAD > dist/head.txt
          git describe --tags > dist/describe.txt
          printf '%s' "$status" > dist/status.txt
          test ! -f .git || { echo ".git is a file, not a directory (finding F2)"; exit 1; }

  failing:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.txt
    steps:
      - name: writes something first
        run: mkdir -p dist && echo partial > dist/partial.txt
      - name: fails
        run: exit 42
      - name: never runs
        run: echo SHOULD-NOT-APPEAR > dist/never.txt

  jobtimeout:
    runs_on: [test]
    timeout: 8s
    steps:
      - name: sleeps past the job budget
        run: sleep 600

  steptimeout:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.txt
    steps:
      - name: writes before hanging
        run: mkdir -p dist && echo before > dist/before.txt
      - name: hangs
        run: sleep 600
        timeout: 8s
      - name: never runs
        run: echo SHOULD-NOT-APPEAR > dist/never.txt

  cancelme:
    runs_on: [test]
    timeout: 5m
    artifacts:
      - path: dist/*.txt
    steps:
      - name: writes then waits
        run: mkdir -p dist && echo written > dist/written.txt && sleep 600
`

// TestIntegration_FullRunAgainstAFixtureRepo is §24's first line.
//
// A real agent, a real git repository, a real container, and a registry read
// off the checkout at the resolved commit. Everything below it varies one
// thing; this one establishes that the whole path works, which is what makes
// the others' failures attributable.
func TestIntegration_FullRunAgainstAFixtureRepo(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rfull", "build", "main", map[string]string{"channel": "external"})
	r := m.terminal(t, "rfull", 5*time.Minute)

	if r.State != protocol.StateSucceeded {
		t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s",
			r.State, r.ErrorCode, r.ErrorMsg, m.log("rfull"))
	}
	if r.ExitCode != nil && *r.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 or absent", *r.ExitCode)
	}
	if r.RefNormalized != "refs/heads/main" {
		t.Errorf("ref_normalized = %q, want refs/heads/main", r.RefNormalized)
	}
	if r.SHA == "" {
		t.Error("the run recorded no commit")
	}
	if r.Runner != "it" {
		t.Errorf("runner = %q, want it", r.Runner)
	}

	// The input reached the step environment and the step wrote it.
	got := m.artifactPaths(t, "rfull")
	if !contains(got, "dist/channel.txt") || !contains(got, "dist/one.txt") {
		t.Fatalf("artifacts = %v, want dist/one.txt and dist/channel.txt", got)
	}
	if body := readArtifact(t, m, "rfull", "dist/channel.txt"); strings.TrimSpace(body) != "external" {
		t.Errorf("dist/channel.txt = %q, want external", body)
	}

	// Every declared step ran, in order, and the log says so.
	log := m.log("rfull")
	for _, step := range []string{"prepare", "stamp", "verify"} {
		if !strings.Contains(log, step) {
			t.Errorf("the log does not mention step %q:\n%s", step, log)
		}
	}
}

// TestIntegration_GitWorksInsideTheContainer is the F2 regression.
//
// Draft 01 specified `git worktree add`, whose .git is a *file* holding an
// absolute gitdir: path pointing back into the mirror — a path that does not
// exist inside a container, so every git command a build script runs fails
// with "not a git repository". Build scripts call `git rev-parse HEAD` and
// `git describe --tags` constantly, for version stamping and build numbers, so
// this breaks on the first real repository.
//
// The three assertions are the three things a real build actually does, and
// the fourth — that .git is not a file — is the shape of the bug rather than
// its symptom.
func TestIntegration_GitWorksInsideTheContainer(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rgit", "gitinfo", "main", nil)
	r := m.terminal(t, "rgit", 5*time.Minute)
	if r.State != protocol.StateSucceeded {
		t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s",
			r.State, r.ErrorCode, r.ErrorMsg, m.log("rgit"))
	}

	head := strings.TrimSpace(readArtifact(t, m, "rgit", "dist/head.txt"))
	if head != r.SHA {
		t.Errorf("`git rev-parse HEAD` inside the environment gave %q; the run resolved %q",
			head, r.SHA)
	}
	if desc := strings.TrimSpace(readArtifact(t, m, "rgit", "dist/describe.txt")); desc != "v1.0.0" {
		t.Errorf("`git describe --tags` inside the environment gave %q, want v1.0.0", desc)
	}
	// §12: the workspace is a clean checkout. A dirty one means the clone or
	// the mount left something behind, and every `git describe --dirty` in the
	// wild would report it.
	if status := strings.TrimSpace(readArtifact(t, m, "rgit", "dist/status.txt")); status != "" {
		t.Errorf("`git status --porcelain` inside the environment is not clean:\n%s", status)
	}
}

// TestIntegration_AFailingStepFailsTheRunWithItsExitCode.
//
// Three claims, and the third is the one that gets forgotten: the steps after
// a failure do not run. A run that reported the right code while quietly
// executing the rest of the job would pass a test written around the first two.
func TestIntegration_AFailingStepFailsTheRunWithItsExitCode(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rfail", "failing", "main", nil)
	r := m.terminal(t, "rfail", 5*time.Minute)

	if r.State != protocol.StateFailed {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}
	if r.ExitCode == nil || *r.ExitCode != 42 {
		t.Errorf("exit code = %v, want 42", r.ExitCode)
	}
	if r.ErrorCode != protocol.ErrStep {
		t.Errorf("error code = %q, want %q", r.ErrorCode, protocol.ErrStep)
	}

	// §11: collecting always runs, including after failed — a broken build
	// still yields its artifacts.
	got := m.artifactPaths(t, "rfail")
	if !contains(got, "dist/partial.txt") {
		t.Errorf("artifacts = %v; a failed run must still collect what it produced", got)
	}
	if contains(got, "dist/never.txt") {
		t.Error("a step after the failing one ran")
	}
}

// TestIntegration_AJobTimeoutTearsDown.
//
// The teardown half is the expensive one to get wrong: a container left
// running after a job budget expires holds a bind mount open, so the workspace
// cannot be pruned and the disk fills up over a weekend.
func TestIntegration_AJobTimeoutTearsDown(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rjobto", "jobtimeout", "main", nil)
	r := m.terminal(t, "rjobto", 5*time.Minute)

	if r.State != protocol.StateTimedOut {
		t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s",
			r.State, r.ErrorCode, r.ErrorMsg, m.log("rjobto"))
	}
	if r.ErrorCode != protocol.ErrTimeout {
		t.Errorf("error code = %q, want %q", r.ErrorCode, protocol.ErrTimeout)
	}
	assertTornDown(t, m, "rjobto")
}

// TestIntegration_AStepTimeoutStillCollects is §9's last paragraph.
//
// "Exceeding it kills the step's process group, fails the run with
// E_STEP_TIMEOUT, and still runs collecting — a hung step's partial logs are
// the diagnostic." The artifact assertion is the whole point: a step timeout
// that discarded the run's output would remove the only evidence of why the
// step hung.
func TestIntegration_AStepTimeoutStillCollects(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rstepto", "steptimeout", "main", nil)
	r := m.terminal(t, "rstepto", 5*time.Minute)

	if r.State != protocol.StateFailed && r.State != protocol.StateTimedOut {
		t.Fatalf("run finished %s (%s): %s", r.State, r.ErrorCode, r.ErrorMsg)
	}
	if r.ErrorCode != protocol.ErrStepTimeout {
		t.Errorf("error code = %q, want %q", r.ErrorCode, protocol.ErrStepTimeout)
	}

	got := m.artifactPaths(t, "rstepto")
	if !contains(got, "dist/before.txt") {
		t.Errorf("artifacts = %v; §9 says a step timeout still collects", got)
	}
	if contains(got, "dist/never.txt") {
		t.Error("a step after the timed-out one ran")
	}
	assertTornDown(t, m, "rstepto")
}

// TestIntegration_ACancelMidRunTerminatesAndStillCollects.
//
// A cancel is the Stop button the product promises, arriving on the local
// transport instead of a phone. It has to reach the goroutine holding the
// run's context, stop the work inside the grace window, and still collect —
// artifacts from a build that already ran are worth more than a few seconds of
// teardown latency.
func TestIntegration_ACancelMidRunTerminatesAndStillCollects(t *testing.T) {
	m := newMachine(t, machineOptions{jobs: fullRunJobs})

	m.submit(t, "rcancel", "cancelme", "main", nil)
	m.waitForState(t, "rcancel", 3*time.Minute, protocol.StateRunning)

	// The artifact has to exist before the cancel, or "it still collected"
	// would be true of a run that had produced nothing.
	waitFor(t, "the step to write its artifact", 60*time.Second, func() bool {
		return strings.Contains(m.log("rcancel"), "writes then waits")
	})
	time.Sleep(2 * time.Second)

	start := time.Now()
	m.cancel(t, "rcancel", "changed my mind")
	r := m.terminal(t, "rcancel", 3*time.Minute)
	elapsed := time.Since(start)

	if r.State != protocol.StateCancelled {
		t.Fatalf("run finished %s (%s): %s\n--- log ---\n%s",
			r.State, r.ErrorCode, r.ErrorMsg, m.log("rcancel"))
	}
	// §11 gives the executor SIGTERM → 30s → SIGKILL, and run.DefaultCancelGrace
	// is 45s on top of it. Anything approaching two minutes means nothing
	// honoured the contract and the run ended for some other reason.
	if elapsed > 2*time.Minute {
		t.Errorf("the cancel took %s to take effect", elapsed)
	}
	// The agent appends the actor, so this is a containment check rather than
	// an equality one: what matters is that the reason a person typed on a
	// phone survives to the audit trail (§7.1).
	if !strings.Contains(r.CancelReason, "changed my mind") {
		t.Errorf("cancel_reason = %q, want it to carry the reason that was sent", r.CancelReason)
	}
	if got := m.artifactPaths(t, "rcancel"); !contains(got, "dist/written.txt") {
		t.Errorf("artifacts = %v; a cancelled run still collects", got)
	}
	assertTornDown(t, m, "rcancel")
}

// ---------------------------------------------------------------------------

// assertTornDown checks that nothing survived the run.
//
// Both halves are cheap and both have failed in this project's history: a
// container outliving its run holds the bind mount open, and a workspace
// outliving its run is the leak §6's reconciliation exists to clean up — which
// it should never have to on a clean shutdown.
func assertTornDown(t *testing.T, m *machine, runID string) {
	t.Helper()

	ws := filepath.Join(m.cfg.Paths.Work, runID)
	waitFor(t, "the workspace to be removed", 90*time.Second, func() bool {
		_, err := os.Stat(ws)
		return os.IsNotExist(err)
	})

	waitFor(t, "the container to be removed", 90*time.Second, func() bool {
		return len(containersForRun(t, runID)) == 0
	})
}

// containersForRun asks the daemon directly, by the label every executor
// stamps. Going through `docker ps` rather than through the executor is
// deliberate: the claim is about the daemon's state, and asking the code under
// test whether it cleaned up is not the same question.
func containersForRun(t *testing.T, runID string) []string {
	t.Helper()
	out, err := execDocker(t, "ps", "-aq", "--filter", "label=latem.run-id="+runID)
	if err != nil {
		t.Fatalf("docker ps: %v\n%s", err, out)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			ids = append(ids, line)
		}
	}
	return ids
}

func readArtifact(t *testing.T, m *machine, runID, rel string) string {
	t.Helper()
	path := filepath.Join(artifactDir(t, m, runID), filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading collected artifact %s: %v (collected: %v)",
			rel, err, m.artifactPaths(t, runID))
	}
	return string(b)
}

func artifactDir(t *testing.T, m *machine, runID string) string {
	t.Helper()
	for _, c := range []string{
		filepath.Join(m.cfg.Paths.Artifacts, runID),
		filepath.Join(m.cfg.Paths.Artifacts, "artifacts", runID),
	} {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	t.Fatalf("no artifact directory for %s under %s", runID, m.cfg.Paths.Artifacts)
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, why string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, why)
}
