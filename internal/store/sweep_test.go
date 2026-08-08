package store

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// enqueueWaiting creates a run in a waiting state with a deadline.
func enqueueWaiting(t *testing.T, d *DB, id string, state protocol.RunState, expires time.Time) Run {
	t.Helper()

	r := testRun(id, state)
	r.ExpiresAt = &expires
	mustCreate(t, d, r)

	if state == protocol.StateQueued {
		if err := d.Enqueue(t.Context(), id, 0); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// TestSweepExpired_RejectsWithTheRightCode covers both deadlines in one pass,
// because one statement sweeps both and the CASE that tells them apart is the
// part that can be wrong.
func TestSweepExpired_RejectsWithTheRightCode(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	enqueueWaiting(t, d, "stale-queued", protocol.StateQueued, now.Add(-time.Minute))
	enqueueWaiting(t, d, "stale-approval", protocol.StateAwaitingApproval, now.Add(-time.Hour))
	enqueueWaiting(t, d, "fresh-queued", protocol.StateQueued, now.Add(time.Hour))

	swept, err := d.SweepExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 2 {
		t.Fatalf("swept %d runs, want 2: %+v", len(swept), swept)
	}

	want := map[string]protocol.ErrorCode{
		"stale-queued":   protocol.ErrQueueExpired,
		"stale-approval": protocol.ErrApprovalExpired,
	}
	for _, r := range swept {
		if r.State != protocol.StateRejected {
			t.Errorf("%s state = %q, want rejected", r.ID, r.State)
		}
		if r.ErrorCode != want[r.ID] {
			t.Errorf("%s error_code = %q, want %q", r.ID, r.ErrorCode, want[r.ID])
		}
		if r.ErrorMsg == "" {
			t.Errorf("%s has no error message; the operator is owed a reason", r.ID)
		}
		if r.FinishedAt == nil || !r.FinishedAt.Equal(now) {
			t.Errorf("%s finished_at = %v, want %v", r.ID, r.FinishedAt, now)
		}
		if r.ExpiresAt != nil {
			t.Errorf("%s kept its deadline after being swept", r.ID)
		}
	}

	fresh, err := d.Get(t.Context(), "fresh-queued")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != protocol.StateQueued {
		t.Errorf("a run whose deadline has not passed was swept: %q", fresh.State)
	}
}

// TestSweepExpired_UsesThePartialIndex is the assertion RUNNER_SPEC §22 asks
// for. The sweep runs daily and at startup on a machine with a year of history;
// walking the whole runs table would still produce the right answer, which is
// exactly why the plan has to be checked rather than the result.
func TestSweepExpired_UsesThePartialIndex(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	plan, err := d.sweepPlan(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")

	if !strings.Contains(joined, "runs_expiry_idx") {
		t.Fatalf("the TTL sweep does not use runs_expiry_idx; plan was:\n%s", joined)
	}
	if !strings.Contains(joined, "SEARCH") {
		t.Errorf("the sweep is not an indexed search; plan was:\n%s", joined)
	}
	if strings.Contains(joined, "SCAN runs\n") || strings.HasSuffix(joined, "SCAN runs") {
		t.Errorf("the sweep walks the runs table; plan was:\n%s", joined)
	}
}

// TestSweepExpired_IgnoresRunsWithoutDeadlines: a caller that never set an
// expiry gets a run that waits, not a run that vanishes.
func TestSweepExpired_IgnoresRunsWithoutDeadlines(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	mustCreate(t, d, testRun("no-deadline", protocol.StateQueued))
	mustCreate(t, d, testRun("running", protocol.StateRunning))
	// A running run with a stale deadline must survive too: only waiting states
	// are swept, and a deadline left over from the queue is not a licence to
	// kill a build that is halfway through.
	stale := testRun("running-stale-deadline", protocol.StateRunning)
	past := now.Add(-24 * time.Hour)
	stale.ExpiresAt = &past
	mustCreate(t, d, stale)

	swept, err := d.SweepExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept %+v, want nothing", swept)
	}
}

// TestSweepExpired_RemovesTheQueueRow: a swept run is terminal, and a terminal
// run left in the queue is a row ClaimNext considers forever and never claims.
func TestSweepExpired_RemovesTheQueueRow(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	enqueueWaiting(t, d, "stale", protocol.StateQueued, now.Add(-time.Second))
	if _, err := d.SweepExpired(t.Context(), now); err != nil {
		t.Fatal(err)
	}

	depth, err := d.QueueDepth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d after sweeping the only queued run, want 0", depth)
	}

	var rows int
	if err := d.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM queue`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d queue rows survived the sweep", rows)
	}
}

// TestSweepExpired_TransitionsAreLegal keeps the sweep and the §11 state
// machine from disagreeing. The sweep writes rejected directly, bypassing
// Transition; if that edge were not in the table, the same change would be
// legal from the ticker and illegal from anywhere else.
func TestSweepExpired_TransitionsAreLegal(t *testing.T) {
	for _, from := range waitingStates() {
		if !legal(from, protocol.StateRejected) {
			t.Errorf("the sweep moves %s -> rejected, but the §11 table forbids it", from)
		}
	}
}

// TestSweepExpired_IsIdempotent: the daily ticker and the startup sweep can
// both run within a second of each other on a restart.
func TestSweepExpired_IsIdempotent(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	enqueueWaiting(t, d, "stale", protocol.StateQueued, now.Add(-time.Second))

	first, err := d.SweepExpired(t.Context(), now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first sweep = %+v, %v", first, err)
	}
	second, err := d.SweepExpired(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep re-rejected %+v", second)
	}
}
