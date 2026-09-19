package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The half of the M4 acceptance criterion that says "the run reads
// interrupted".
//
// The states a crash can leave behind are exactly the ones something was
// actively driving. queued and awaiting_approval are non-terminal too, and
// interrupting them would destroy §7.3's queue — the thing `latem run` writes
// to when no agent is running at all — so this is a table rather than a loop
// over protocol's non-terminal set.
func TestReconcileInterruptsOnlyTheStatesACrashCanLeave(t *testing.T) {
	tests := []struct {
		state protocol.RunState
		want  protocol.RunState
	}{
		{protocol.StateClaimed, protocol.StateInterrupted},
		{protocol.StatePreparing, protocol.StateInterrupted},
		{protocol.StateRunning, protocol.StateInterrupted},
		{protocol.StateCollecting, protocol.StateInterrupted},

		// The queue survives a restart. That is the whole of §7.3.
		{protocol.StateQueued, protocol.StateQueued},
		{protocol.StateAwaitingApproval, protocol.StateAwaitingApproval},

		// Terminal states are already final and must not be rewritten.
		{protocol.StateSucceeded, protocol.StateSucceeded},
		{protocol.StateFailed, protocol.StateFailed},
	}

	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	ctx := context.Background()

	ids := make([]string, 0, len(tests))
	for i, tt := range tests {
		id := "r_state" + string(rune('a'+i))
		ids = append(ids, id)
		seedRun(t, db, id, tt.state)
	}
	_ = db.Close()

	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	after := openStore(t, cfg)
	for i, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			r, err := after.Get(ctx, ids[i])
			if err != nil {
				t.Fatal(err)
			}
			if r.State != tt.want {
				t.Fatalf("a run left in %s came back as %s, want %s", tt.state, r.State, tt.want)
			}
			if tt.want != protocol.StateInterrupted {
				return
			}
			if r.ErrorCode != protocol.ErrInterrupted {
				t.Errorf("error code = %q, want %s", r.ErrorCode, protocol.ErrInterrupted)
			}
			if r.FinishedAt == nil {
				t.Error("an interrupted run must be stamped finished, or it stays in the hot partial index forever")
			}
			if !protocol.ErrInterrupted.Retryable() {
				t.Error("E_INTERRUPTED is retryable in §21; a run the machine dropped deserves a retry button")
			}
		})
	}
}

// The other half of M4: zero orphaned containers, VMs, or workspaces.
//
// The crash is simulated at the level that matters — a resource on disk with
// no process behind it and a run row still saying `running` — because that is
// exactly the state SIGKILL leaves. The genuinely killed variant is
// TestKill9LeavesNoOrphans in kill9_test.go; this one pins the reaping rules,
// including the two things reconciliation must NOT delete.
func TestReconcileReapsOrphansButNotLiveWork(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)

	// r_dead crashed mid-run: a resource, a workspace, and a `running` row.
	seedRun(t, db, "r_dead", protocol.StateRunning)
	// r_wait is queued and untouched by the crash. Its workspace does not
	// exist, but a queued run must never be reaped as litter.
	seedRun(t, db, "r_wait", protocol.StateQueued)
	// r_gone finished cleanly and left a workspace behind — the shape of a
	// teardown that failed on the last step.
	seedRun(t, db, "r_gone", protocol.StateSucceeded)
	_ = db.Close()

	if err := os.MkdirAll(cfg.Paths.Work, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"r_dead", "r_wait", "r_gone", "not-a-run-id.tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Paths.Work, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ta := newTestAgent(t, cfg, nil)
	ta.exec.leak(t, "r_dead")
	ta.exec.leak(t, "r_gone")
	ta.exec.leak(t, "r_wait")

	ta.run(t)

	// Executor resources: everything whose run is not still live.
	left := ta.exec.resources(t)
	if slices.Contains(left, "r_dead") {
		t.Error("the container of a run killed mid-flight was not reaped (M4)")
	}
	if slices.Contains(left, "r_gone") {
		t.Error("the container of a finished run was not reaped")
	}
	if !slices.Contains(left, "r_wait") {
		t.Error("a queued run's resource was reaped; only runs that are no longer live may be")
	}

	// Workspaces: same rule, plus the one exception.
	for _, name := range []string{"r_dead", "r_gone"} {
		if _, err := os.Stat(filepath.Join(cfg.Paths.Work, name)); !os.IsNotExist(err) {
			t.Errorf("workspace %s survived reconciliation (M4): %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.Work, "r_wait")); err != nil {
		t.Errorf("a queued run's workspace was deleted: %v", err)
	}
	// A directory whose name is not a run id was not created by this agent.
	// Reporting it is right; rm -rf on a path it cannot vouch for is not.
	if _, err := os.Stat(filepath.Join(cfg.Paths.Work, "not-a-run-id.tmp")); err != nil {
		t.Errorf("reconciliation deleted a directory it did not create: %v", err)
	}
}

// An agent that was off for a week comes back to a queue full of runs nobody
// wants any more (§11, finding F7). Sweeping at startup is what stops the
// partial index in §22 growing without bound.
func TestReconcileSweepsExpiredWaitingRuns(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	seedRun(t, db, "r_stale", protocol.StateQueued)
	if err := db.SetExpiry(ctx, "r_stale", past); err != nil {
		t.Fatal(err)
	}
	seedRun(t, db, "r_parked", protocol.StateAwaitingApproval)
	if err := db.SetExpiry(ctx, "r_parked", past); err != nil {
		t.Fatal(err)
	}
	seedRun(t, db, "r_fresh", protocol.StateQueued)
	if err := db.SetExpiry(ctx, "r_fresh", future); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	ta := newTestAgent(t, cfg, func(o *Options) {
		// Nothing should execute; the fake refuses to prepare so a claim that
		// slipped through would be visible as a failure rather than a pass.
		o.ClaimInterval = time.Hour
	})
	ta.run(t)

	after := openStore(t, cfg)
	for _, tc := range []struct {
		id   string
		want protocol.RunState
		code protocol.ErrorCode
	}{
		{"r_stale", protocol.StateRejected, protocol.ErrQueueExpired},
		{"r_parked", protocol.StateRejected, protocol.ErrApprovalExpired},
		{"r_fresh", protocol.StateQueued, ""},
	} {
		r, err := after.Get(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != tc.want {
			t.Errorf("%s = %s, want %s", tc.id, r.State, tc.want)
		}
		if tc.code != "" && r.ErrorCode != tc.code {
			t.Errorf("%s error code = %q, want %s", tc.id, r.ErrorCode, tc.code)
		}
	}
}

// Reconciliation runs on a machine where nothing else worked, so one reaper
// that cannot answer must not strand the rest.
//
// Refusing to start on a failed reap would mean a single undeletable container
// costs an operator their entire build machine; abandoning the remaining
// sweeps would leave the workspaces of a crashed run on disk forever. Both
// halves are asserted: the agent comes up, and everything reapable is reaped.
func TestReconcileContinuesWhenOneReaperFails(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	seedRun(t, db, "r_dead", protocol.StateRunning)
	_ = db.Close()

	if err := os.MkdirAll(filepath.Join(cfg.Paths.Work, "r_dead"), 0o700); err != nil {
		t.Fatal(err)
	}

	ta := newTestAgent(t, cfg, nil)
	ta.exec.mu.Lock()
	ta.exec.reapErr = errBoom
	ta.exec.mu.Unlock()

	ta.run(t)

	after := openStore(t, cfg)
	r, err := after.Get(context.Background(), "r_dead")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateInterrupted {
		t.Errorf("run state = %s, want interrupted despite the reap failure", r.State)
	}
	if _, err := os.Stat(filepath.Join(cfg.Paths.Work, "r_dead")); !os.IsNotExist(err) {
		t.Errorf("the workspace sweep was abandoned because a different reaper failed: %v", err)
	}
}

// A runner whose executor cannot even be constructed must not stop the agent,
// and must say why it is dark.
//
// This is the Mac whose Docker socket is missing: its Xcode runner should
// keep working, and the heartbeat should carry a sentence an operator can act
// on rather than the runner silently disappearing from the machine's
// advertised capabilities.
func TestARunnerWithNoExecutorIsUnavailableRatherThanFatal(t *testing.T) {
	cfg := newConfigDir(t, configOptions{runners: `
[[runner]]
name        = "broken"
executor    = "docker"
labels      = ["linux", "docker"]
concurrency = 1

  [runner.docker]
  min_free_gb = 0.001

[[runner]]
name        = "works"
executor    = "native"
labels      = ["test"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`})

	ta := newTestAgent(t, cfg, func(o *Options) {
		fx := newFakeExecutor(t, "native")
		o.NewExecutor = func(r *configRunner, _ configPaths) (executorIface, error) {
			if r.Name == "broken" {
				return nil, errBoom
			}
			return fx, nil
		}
	})
	ta.run(t)

	h := ta.health(context.Background())
	if len(h.Runners) != 2 {
		t.Fatalf("heartbeat carries %d runners, want both", len(h.Runners))
	}
	byName := map[string]protocol.RunnerInfo{}
	for _, r := range h.Runners {
		byName[r.Name] = r
	}
	if byName["broken"].Available {
		t.Error("a runner with no executor must not advertise itself as available")
	}
	if byName["broken"].Unavailable == "" {
		t.Error("an unavailable runner must carry a reason; a dark runner with no explanation is a support question")
	}
	if !byName["works"].Available {
		t.Errorf("the working runner should be available: %q", byName["works"].Unavailable)
	}
}

// Reconciliation has to be repeatable. An agent that crashed twice, or one
// whose daily prune runs the same predicates, must not turn a clean machine
// into an error the second time round — and the report is what an operator
// reads, so it has to say "nothing" when there was nothing.
func TestReconcileIsRepeatableAndReportsWhatItDid(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	seedRun(t, db, "r_crashed", protocol.StateRunning)
	_ = db.Close()

	if err := os.MkdirAll(filepath.Join(cfg.Paths.Work, "r_crashed"), 0o700); err != nil {
		t.Fatal(err)
	}

	ta := newTestAgent(t, cfg, nil)
	ta.exec.leak(t, "r_crashed")
	ta.run(t)

	// The first pass happened during startup. A second one must find nothing.
	rec, err := ta.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("a second reconciliation failed: %v", err)
	}
	if !rec.Empty() {
		t.Errorf("the second pass found work to do: %+v", rec)
	}

	// And the first pass did the work, which is what makes the above meaningful.
	after := openStore(t, cfg)
	r, err := after.Get(context.Background(), "r_crashed")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateInterrupted {
		t.Errorf("state = %s, want interrupted", r.State)
	}
	if len(ta.exec.resources(t)) != 0 {
		t.Errorf("resources survived: %v", ta.exec.resources(t))
	}
}

// The report is the deliverable as much as the cleanup is: a reap that says
// nothing is indistinguishable from a reap that found nothing (§14.4).
//
// The litter is arranged after startup rather than before it, because startup
// runs reconciliation itself — so a report captured from a second call would
// otherwise be empty by construction, and the test would pass without looking
// at anything.
func TestReconciliationReportNamesEverythingItTouched(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, func(o *Options) { o.ClaimInterval = time.Hour })
	ta.run(t)

	db := openStore(t, cfg)
	ctx := context.Background()
	seedRun(t, db, "r_c1", protocol.StateRunning)
	seedRun(t, db, "r_c2", protocol.StatePreparing)
	seedRun(t, db, "r_exp", protocol.StateQueued)
	if err := db.SetExpiry(ctx, "r_exp", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"r_c1", "not-an-id.tmp"} {
		if err := os.MkdirAll(filepath.Join(cfg.Paths.Work, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ta.exec.leak(t, "r_c2")

	rec, err := ta.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(rec.Interrupted) != 2 {
		t.Errorf("Interrupted = %v, want both crashed runs", rec.Interrupted)
	}
	if len(rec.Swept) != 1 || rec.Swept[0] != "r_exp" {
		t.Errorf("Swept = %v, want [r_exp]", rec.Swept)
	}
	if len(rec.Reaped) != 1 || rec.Reaped[0].RunID != "r_c2" {
		t.Errorf("Reaped = %+v, want the orphaned resource of r_c2", rec.Reaped)
	}
	if len(rec.Workspaces) != 1 || rec.Workspaces[0] != "r_c1" {
		t.Errorf("Workspaces = %v, want [r_c1]", rec.Workspaces)
	}
	// A directory whose name is not a run id is reported, never removed: this
	// agent will not rm -rf a path it cannot vouch for having created.
	if len(rec.Unowned) != 1 || rec.Unowned[0] != "not-an-id.tmp" {
		t.Errorf("Unowned = %v, want [not-an-id.tmp]", rec.Unowned)
	}
	if rec.Empty() {
		t.Error("Empty() is true for a report that found five things")
	}
}
