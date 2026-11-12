package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	shaA = "0123456789abcdef0123456789abcdef01234567"
	shaB = "fedcba9876543210fedcba9876543210fedcba98"
)

// parkedRun is a run in awaiting_approval with a commit already resolved,
// which is the only shape gate 8 can produce (amendment A27).
func parkedRun(t *testing.T, d *DB, id, sha string) Run {
	t.Helper()

	r := testRun(id, protocol.StateAwaitingApproval)
	r.RefNormalized = "refs/heads/feature/x"
	r.SHA = sha
	deadline := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	r.ExpiresAt = &deadline
	return mustCreate(t, d, r)
}

// Approval pins a commit, not a ref. That is the entire security content of
// this method: a ref can point at a branch a contributor controls, so an
// approval recorded against the ref alone would be granted for whatever the
// branch happens to be at build time.
func TestApprove_PinsTheResolvedCommitAndQueues(t *testing.T) {
	d := newTestDB(t)
	parked := parkedRun(t, d, "r_park", shaA)

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(72 * time.Hour)

	got, err := d.Approve(t.Context(), parked.ID, now, deadline)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if got.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued; §11 has no awaiting_approval → claimed edge", got.State)
	}
	if got.ApprovedSHA != shaA {
		t.Errorf("approved_sha = %q, want the run's resolved commit %q", got.ApprovedSHA, shaA)
	}
	if got.ApprovedAt == nil || !got.ApprovedAt.Equal(now) {
		t.Errorf("approved_at = %v, want %v", got.ApprovedAt, now)
	}
	// The approval deadline must be replaced, not carried: a run still holding
	// its approval_ttl is swept as an expired queue entry by the next ticker,
	// minutes after a human said yes to it.
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(deadline) {
		t.Errorf("expires_at = %v, want the queue deadline %v", got.ExpiresAt, deadline)
	}

	// Reading it back proves the transaction committed, not just that the
	// returned struct was assembled correctly.
	stored, err := d.Get(t.Context(), parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ApprovedSHA != shaA || stored.State != protocol.StateQueued {
		t.Errorf("stored row = %s/%q, want queued/%s", stored.State, stored.ApprovedSHA, shaA)
	}

	// And it is claimable, which is the point of approving it.
	claimed, err := d.ClaimNext(t.Context(), nil)
	if err != nil {
		t.Fatalf("an approved run was not enqueued: %v", err)
	}
	if claimed.ID != parked.ID {
		t.Errorf("claimed %q, want %q", claimed.ID, parked.ID)
	}
	if claimed.ApprovedSHA != shaA {
		t.Errorf("the claimed row lost the pin: approved_sha = %q", claimed.ApprovedSHA)
	}
}

// Approving twice is something a person does at a terminal. The second one is a
// no-op success rather than an error, and it must not move the pin.
func TestApprove_IsIdempotent(t *testing.T) {
	d := newTestDB(t)
	parked := parkedRun(t, d, "r_twice", shaA)

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	if _, err := d.Approve(t.Context(), parked.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("first Approve: %v", err)
	}

	later := now.Add(time.Minute)
	got, err := d.Approve(t.Context(), parked.ID, later, later.Add(time.Hour))
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	if got.ApprovedAt == nil || !got.ApprovedAt.Equal(now) {
		t.Errorf("approved_at = %v; a second approve rewrote the first one's record", got.ApprovedAt)
	}
	if got.ApprovedSHA != shaA {
		t.Errorf("approved_sha = %q, want %q", got.ApprovedSHA, shaA)
	}
}

// A run with no resolved commit cannot be approved, because there is nothing to
// pin. In practice this is a row parked by an agent older than A27: guessing
// "whatever the ref points at when it eventually builds" is exactly the
// force-push hole the pin exists to close.
func TestApprove_RefusesARunWithNoCommit(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r_nosha", protocol.StateAwaitingApproval))

	_, err := d.Approve(t.Context(), "r_nosha", time.Now(), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrNoCommit) {
		t.Fatalf("Approve = %v, want ErrNoCommit", err)
	}

	r, getErr := d.Get(t.Context(), "r_nosha")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if r.State != protocol.StateAwaitingApproval {
		t.Errorf("state = %s; a refused approval must not move the run", r.State)
	}
}

func TestApprove_RefusesWhatItCannotApprove(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r_running", protocol.StateRunning))
	mustCreate(t, d, testRun("r_done", protocol.StateSucceeded))

	for _, id := range []string{"r_running", "r_done"} {
		_, err := d.Approve(t.Context(), id, time.Now(), time.Now().Add(time.Hour))
		if !errors.Is(err, ErrNotAwaitingApproval) {
			t.Errorf("Approve(%s) = %v, want ErrNotAwaitingApproval", id, err)
		}
	}

	_, err := d.Approve(t.Context(), "r_missing", time.Now(), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Approve(unknown) = %v, want ErrNotFound", err)
	}
}

// Parking a run always discards any approval on it. Without this, a run
// approved at commit A whose ref then moved to B would be parked again by gate
// 8 and still carry A — and the next attempt would compare a fresh commit
// against a pin the operator has not been shown since.
func TestTransition_ParkingClearsAnApproval(t *testing.T) {
	d := newTestDB(t)
	parked := parkedRun(t, d, "r_moved", shaA)

	now := time.Now()
	if _, err := d.Approve(t.Context(), parked.ID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.Claim(t.Context(), parked.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Transition(t.Context(), parked.ID, protocol.StatePreparing, protocol.Update{}); err != nil {
		t.Fatal(err)
	}

	// Gate 8 re-parks it, carrying the commit the ref now resolves to (A21).
	deadline := now.Add(24 * time.Hour)
	err := d.Transition(t.Context(), parked.ID, protocol.StateAwaitingApproval, protocol.Update{
		SHA:       shaB,
		ExpiresAt: &deadline,
		ErrorMsg:  "the ref moved since it was approved",
	})
	if err != nil {
		t.Fatalf("re-park: %v", err)
	}

	r, err := d.Get(t.Context(), parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.ApprovedSHA != "" || r.ApprovedAt != nil {
		t.Errorf("approval survived the re-park: %q at %v", r.ApprovedSHA, r.ApprovedAt)
	}
	if r.SHA != shaB {
		t.Errorf("sha = %q, want the newly resolved %q", r.SHA, shaB)
	}

	// And the operator can approve the commit they are now being shown.
	got, err := d.Approve(t.Context(), parked.ID, now, deadline)
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if got.ApprovedSHA != shaB {
		t.Errorf("approved_sha = %q, want %q", got.ApprovedSHA, shaB)
	}
}

// Create round-trips the approval columns, so a row read back is the row that
// was written. Nothing creates an already-approved run today; the assertion is
// that the projection and the insert agree about these two columns.
func TestCreate_RoundTripsApproval(t *testing.T) {
	d := newTestDB(t)

	at := time.Date(2026, 8, 7, 10, 30, 0, 0, time.UTC)
	r := testRun("r_seed", protocol.StateQueued)
	r.SHA = shaA
	r.ApprovedAt = &at
	r.ApprovedSHA = shaA
	mustCreate(t, d, r)

	got, err := d.Get(t.Context(), "r_seed")
	if err != nil {
		t.Fatal(err)
	}
	if got.ApprovedSHA != shaA {
		t.Errorf("approved_sha = %q, want %q", got.ApprovedSHA, shaA)
	}
	if got.ApprovedAt == nil || !got.ApprovedAt.Equal(at) {
		t.Errorf("approved_at = %v, want %v", got.ApprovedAt, at)
	}
}

// ApproveCommit pins the commit it names or nothing (A89). `latem approve`
// shows a person the commit before it approves, and the run can be held at
// another by then only if somebody else approved it, it built, and its ref
// moved: that commit nobody has looked at.
func TestApproveCommit_PinsOnlyTheCommitItNames(t *testing.T) {
	d := newTestDB(t)
	parked := parkedRun(t, d, "r_park", shaA)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	if _, err := d.ApproveCommit(t.Context(), parked.ID, shaB, now, now.Add(time.Hour)); !errors.Is(err, ErrCommitChanged) {
		t.Fatalf("approving %s for a run held at %s = %v, want ErrCommitChanged", shaB, shaA, err)
	}
	r, err := d.Get(t.Context(), parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateAwaitingApproval || r.ApprovedSHA != "" || r.ApprovedAt != nil {
		t.Fatalf("a refused approval wrote the row: state %s, approved %q at %v", r.State, r.ApprovedSHA, r.ApprovedAt)
	}
	if _, err := d.ClaimNext(t.Context(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("a refused approval queued the run: ClaimNext = %v", err)
	}

	if _, err := d.ApproveCommit(t.Context(), parked.ID, "", now, now.Add(time.Hour)); err == nil {
		t.Error("an approval that names no commit succeeded")
	}

	got, err := d.ApproveCommit(t.Context(), parked.ID, shaA, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ApproveCommit of the commit it is held at: %v", err)
	}
	if got.State != protocol.StateQueued || got.ApprovedSHA != shaA {
		t.Errorf("state %s, approved %q; want queued at %s", got.State, got.ApprovedSHA, shaA)
	}
}
