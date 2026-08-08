package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestExecute_HappyPath(t *testing.T) {
	h := newHarness(t)
	h.env.exec = func(_ context.Context, step executor.Step, stdout, stderr io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "building "+step.Name+"\n")
		_, _ = io.WriteString(stderr, "a warning\n")
		return 0, nil
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, "out/report.txt", "all green")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s), want succeeded", res.State, res.ErrorMsg)
	}
	if res.Runner != "docker-1" {
		t.Errorf("runner = %q, want docker-1", res.Runner)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Path != "out/report.txt" {
		t.Errorf("artifacts = %+v, want out/report.txt", res.Artifacts)
	}

	// Both steps ran, in order, with the run's shell command from the registry
	// on disk and nothing from the request.
	steps := h.env.executed()
	if len(steps) != 2 || steps[0].Run != "./ci/build.sh" || steps[1].Run != "./ci/verify.sh" {
		t.Fatalf("steps = %+v", steps)
	}

	row := mustGet(t, h.db, req.ID)
	if row.State != protocol.StateSucceeded {
		t.Errorf("stored state = %s", row.State)
	}
	if row.Runner != "docker-1" {
		t.Errorf("stored runner = %q", row.Runner)
	}
	if row.StartedAt == nil || row.FinishedAt == nil {
		t.Errorf("started/finished not stamped: %+v %+v", row.StartedAt, row.FinishedAt)
	}
	if row.ExpiresAt != nil {
		t.Errorf("expiry survived into a terminal state: %v", row.ExpiresAt)
	}

	stored, err := h.db.Artifacts(t.Context(), req.ID)
	if err != nil || len(stored) != 1 {
		t.Errorf("stored artifacts = %+v, %v", stored, err)
	}

	// Every resource released exactly once, on the ordinary path.
	if got := h.env.closeCount(); got != 1 {
		t.Errorf("env closed %d times, want 1", got)
	}
	if got := h.caches.releaseCount(); got != 1 {
		t.Errorf("caches released %d times, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(h.source.root, req.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace survived teardown: %v", err)
	}
}

func TestExecute_ReportsEveryTransitionInOrder(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	h.run(o, h.request("test"))

	want := []protocol.RunState{
		protocol.StateQueued,
		protocol.StatePreparing,
		protocol.StateRunning,
		protocol.StateCollecting,
		protocol.StateSucceeded,
	}
	waitFor(t, "all transitions reported", func() bool {
		return len(h.broker.states("run1")) == len(want)
	})
	got := h.broker.states("run1")
	if !slices.Equal(got, want) {
		t.Fatalf("reported %v, want %v", fmtStates(got), fmtStates(want))
	}
}

// The registry is read from the workspace at the resolved commit and never
// from a broker (INV-5). A request naming a job that exists only in the
// workspace file runs; one naming a job that does not is E_UNKNOWN_JOB, no
// matter how confidently the broker asked.
func TestExecute_RegistryComesFromTheWorkspace(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  only-here:
    runs_on: [linux]
    steps:
      - name: real
        run: /bin/echo from-the-workspace
`
	o := h.build()

	res := h.run(o, h.request("only-here"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	steps := h.env.executed()
	if len(steps) != 1 || steps[0].Run != "/bin/echo from-the-workspace" {
		t.Fatalf("ran %+v, want the workspace's own command", steps)
	}
}

func TestExecute_UnknownJobIsRejected(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	res := h.run(o, h.request("no-such-job"))
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrUnknownJob {
		t.Fatalf("got %s/%s, want rejected/E_UNKNOWN_JOB", res.State, res.ErrorCode)
	}
	if h.exec.prepared != 0 {
		t.Error("an unknown job reached the executor")
	}
	// Gate 12 fails after the workspace exists, so collecting still ran and
	// the workspace was still torn down.
	if _, err := os.Stat(filepath.Join(h.source.root, "run1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace leaked: %v", err)
	}
}

func TestExecute_NoRunnerCarriesTheLabels(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	res := h.run(o, h.request("macos"))
	if res.State != protocol.StateRejected || res.ErrorCode != protocol.ErrNoRunner {
		t.Fatalf("got %s/%s, want rejected/E_NO_RUNNER", res.State, res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMsg, "macos") {
		t.Errorf("message does not name the missing label: %q", res.ErrorMsg)
	}
}

func TestExecute_StepFailureIsEStepAndStillCollects(t *testing.T) {
	h := newHarness(t)
	h.env.exec = func(_ context.Context, step executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "working\n")
		if step.Name == "build" {
			return 3, nil
		}
		return 0, nil
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, "out/partial.txt", "half a build")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
		t.Fatalf("got %s/%s, want failed/E_STEP", res.State, res.ErrorCode)
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", res.ExitCode)
	}
	if len(res.Artifacts) != 1 {
		t.Errorf("a failed build lost its artifacts: %+v", res.Artifacts)
	}
	if len(h.env.executed()) != 1 {
		t.Errorf("the second step ran after the first failed")
	}
	waitFor(t, "collecting reported", func() bool {
		return slices.Contains(h.broker.states("run1"), protocol.StateCollecting)
	})
}

func TestExecute_StepTimeoutIsEStepTimeoutAndStillCollects(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  slow:
    runs_on: [linux]
    timeout: 1m
    artifacts:
      - path: out/*.txt
    steps:
      - name: hang
        run: ./ci/hang.sh
        timeout: 50ms
`
	h.env.exec = func(ctx context.Context, _ executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "starting\n")
		<-ctx.Done()
		return -1, ctx.Err()
	}
	o := h.build()

	req := h.request("slow")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, "out/log.txt", "partial")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateTimedOut || res.ErrorCode != protocol.ErrStepTimeout {
		t.Fatalf("got %s/%s, want timed_out/E_STEP_TIMEOUT", res.State, res.ErrorCode)
	}
	if len(res.Artifacts) != 1 {
		t.Errorf("a timed-out build lost its artifacts: %+v", res.Artifacts)
	}
	if !strings.Contains(h.logContents(req.ID), "starting") {
		t.Error("the hung step's partial output is missing from the log")
	}
}

func TestExecute_JobTimeoutIsETimeout(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  slow:
    runs_on: [linux]
    timeout: 60ms
    steps:
      - name: hang
        run: ./ci/hang.sh
`
	h.env.exec = func(ctx context.Context, _ executor.Step, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	o := h.build()

	res := h.run(o, h.request("slow"))
	if res.State != protocol.StateTimedOut || res.ErrorCode != protocol.ErrTimeout {
		t.Fatalf("got %s/%s, want timed_out/E_TIMEOUT", res.State, res.ErrorCode)
	}
}

// The job wall clock outranks the step's: a step with its own budget that is
// still running when the job runs out is E_TIMEOUT, not E_STEP_TIMEOUT.
func TestExecute_JobTimeoutBeatsStepTimeout(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  slow:
    runs_on: [linux]
    timeout: 60ms
    steps:
      - name: hang
        run: ./ci/hang.sh
        timeout: 10m
`
	h.env.exec = func(ctx context.Context, _ executor.Step, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	o := h.build()

	res := h.run(o, h.request("slow"))
	if res.ErrorCode != protocol.ErrTimeout {
		t.Fatalf("code = %s, want E_TIMEOUT", res.ErrorCode)
	}
}

// A job whose budget expires between two steps must not start the next one.
func TestExecute_BudgetCheckedBetweenSteps(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  two:
    runs_on: [linux]
    timeout: 80ms
    steps:
      - name: one
        run: ./one.sh
      - name: two
        run: ./two.sh
`
	h.env.exec = func(_ context.Context, _ executor.Step, _, _ io.Writer) (int, error) {
		time.Sleep(120 * time.Millisecond)
		return 0, nil
	}
	o := h.build()

	res := h.run(o, h.request("two"))
	if res.State != protocol.StateTimedOut {
		t.Fatalf("state = %s, want timed_out", res.State)
	}
	if n := len(h.env.executed()); n != 1 {
		t.Fatalf("%d steps ran, want 1", n)
	}
}

func TestExecute_ExecutorPrepareFailureIsEExecutor(t *testing.T) {
	h := newHarness(t)
	h.exec.prepareErr = errors.New("no such image")
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrExecutor {
		t.Fatalf("got %s/%s, want failed/E_EXECUTOR", res.State, res.ErrorCode)
	}
	// The cache lock taken at gate 18 is still released.
	if h.caches.releaseCount() != 1 {
		t.Error("cache lock leaked when the environment would not start")
	}
}

func TestExecute_CacheFailureIsECache(t *testing.T) {
	h := newHarness(t)
	h.caches.acquireErr = errors.New("volume busy")
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrCache {
		t.Fatalf("got %s/%s, want failed/E_CACHE", res.State, res.ErrorCode)
	}
	// Release is registered even for a failed Acquire, because a partial
	// acquisition leaves locks the caller cannot enumerate.
	if h.caches.releaseCount() != 1 {
		t.Error("Release was not called after a failed Acquire")
	}
}

func TestExecute_Gate17RunnerDiskFloor(t *testing.T) {
	h := newHarness(t)
	h.caches.free = 5 // below the runner's 40 GB floor, above the gate 5 floor
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrDisk {
		t.Fatalf("got %s/%s, want failed/E_DISK", res.State, res.ErrorCode)
	}
	if !res.ErrorCode.Retryable() {
		t.Error("E_DISK should be retryable")
	}
}

func TestExecute_SourceFailureIsESource(t *testing.T) {
	h := newHarness(t)
	h.source.fetchErr = errors.New("host unreachable")
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrSource {
		t.Fatalf("got %s/%s, want failed/E_SOURCE", res.State, res.ErrorCode)
	}
	// Nothing was created, so nothing had to be torn down and collecting is
	// skipped: there is no workspace to collect from.
	if slices.Contains(h.broker.states("run1"), protocol.StateCollecting) {
		t.Error("collecting ran with no workspace")
	}
}

func TestExecute_AmbiguousRefIsRefused(t *testing.T) {
	h := newHarness(t)
	h.source.refs["refs/tags/main"] = true
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.ErrorCode != protocol.ErrAmbiguousRef {
		t.Fatalf("code = %s, want E_AMBIGUOUS_REF", res.ErrorCode)
	}
}

// Gate 8 is authoritative and gate 4 is optimistic: "v1.0" could have been an
// allowed tag, so it passes gate 4; it turns out to be a branch the policy does
// not allow, and §11 says gate 8 parks it for approval.
//
// Before amendment A17 it could not: awaiting_approval had no inbound edge at
// all, so the state was reachable only at Create — the gate 4 half worked and
// the authoritative half did not.
func TestExecute_Gate8ParksAnUnapprovedRef(t *testing.T) {
	h := newHarness(t)
	h.source.refs = map[string]bool{"refs/heads/v1.0": true}
	o := h.build()

	req := h.request("test")
	req.Ref = "v1.0"
	res := h.run(o, req)

	if h.exec.prepared != 0 {
		t.Error("an unapproved ref reached the executor")
	}

	if res.State != protocol.StateAwaitingApproval {
		t.Fatalf("state = %s, want awaiting_approval", res.State)
	}
	// The deadline has to travel with the transition, because only the caller
	// knows the configured approval_ttl (amendment A8).
	if got := mustGet(t, h.db, "run1"); got.ExpiresAt == nil {
		t.Error("no approval_ttl deadline was persisted with the parked run")
	}
	if !strings.Contains(res.ErrorMsg, "latem approve") {
		t.Errorf("message does not say how to approve: %q", res.ErrorMsg)
	}
}

func TestExecute_RepoRemovedFromAllowlistAfterAdmission(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	delete(h.cfg.Repos, "app")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ErrorCode != protocol.ErrUnknownRepo {
		t.Fatalf("code = %s, want E_UNKNOWN_REPO", res.ErrorCode)
	}
	if len(h.source.fetched) != 0 {
		t.Error("a de-allowlisted repo was fetched")
	}
}

func TestExecute_RefusesARunThatIsNotClaimed(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	if _, err := o.Admit(t.Context(), h.request("test"), 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	r := mustGet(t, h.db, "run1") // still queued

	if _, err := o.Execute(t.Context(), r); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("err = %v, want ErrNotClaimed", err)
	}
}

func TestExecute_RefusesAnInvalidRunID(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	_, err := o.Execute(t.Context(), store.Run{
		ID: "../../etc/passwd", Repo: "app", Job: "test", Ref: "main",
		State: protocol.StateClaimed,
	})
	if err == nil || !strings.Contains(err.Error(), "run id") {
		t.Fatalf("err = %v, want a run id complaint", err)
	}
}

func TestExecute_RefusesTwoGoroutinesOnOneRun(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.env.exec = func(context.Context, executor.Step, io.Writer, io.Writer) (int, error) {
		<-release
		return 0, nil
	}
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = o.Execute(t.Context(), r)
	}()

	waitFor(t, "the run to be live", func() bool { return o.IsLive("run1") })
	if _, err := o.Execute(t.Context(), r); !errors.Is(err, ErrAlreadyExecuting) {
		t.Fatalf("err = %v, want ErrAlreadyExecuting", err)
	}
	close(release)
	<-done
}

// A store that refuses the first transition means something else already moved
// the run. Execute reports what the store says rather than inventing an error.
func TestExecute_LosesTheRaceToTheStore(t *testing.T) {
	h := newHarness(t)
	h.opts.Store = &errStore{Store: h.db, failTransitionTo: protocol.StatePreparing}
	o := h.build()

	r := h.admitAndClaim(o, h.request("test"))
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateClaimed {
		t.Fatalf("state = %s, want the store's own answer (claimed)", res.State)
	}
	if h.exec.prepared != 0 {
		t.Error("execution continued past a refused transition")
	}
}

func TestExecute_NoBrokerIsNotAnError(t *testing.T) {
	h := newHarness(t)
	h.brokerless = true
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	if _, err := os.Stat(filepath.Join(h.cfg.Paths.Logs, "run1.log")); err != nil {
		t.Errorf("the local log is missing: %v", err)
	}
}

// Amendment A21 added RefNormalized and SHA to protocol.Update, and the store
// applies them — but for a while nothing set them, so every completed run left
// ref_normalized and sha NULL. That is precisely the audit trail INV-5 exists
// to leave: "requested as main, built refs/heads/main at 3bf1261".
func TestExecute_RecordsTheResolvedRefAndSHA(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s: %s", res.State, res.ErrorMsg)
	}

	got := mustGet(t, h.db, "run1")
	if got.RefNormalized != "refs/heads/main" {
		t.Errorf("ref_normalized = %q, want the fully-qualified name policy matched", got.RefNormalized)
	}
	if got.SHA != h.source.sha {
		t.Errorf("sha = %q, want %q", got.SHA, h.source.sha)
	}
	// The raw request survives beside it, so an audit can answer both halves.
	if got.Ref != "main" {
		t.Errorf("ref = %q, want the ref as requested", got.Ref)
	}
}
