package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A run id becomes work/<id>, logs/<id>.log, artifacts/<id>/, and the
// container name latem-run-<id>. An id that fails protocol.ValidateID is a
// path traversal waiting for a consumer that forgot to check (amendment A6),
// so the generator has to be provably inside the class.
func TestNewRunIDIsAlwaysValid(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, err := NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		if err := protocol.ValidateID(id); err != nil {
			t.Fatalf("generated %q, which is not a legal run id: %v", id, err)
		}
		if !strings.HasPrefix(id, RunIDPrefix) {
			t.Fatalf("generated %q without the %q prefix", id, RunIDPrefix)
		}
		if seen[id] {
			t.Fatalf("generated %q twice; a collision renames somebody else's workspace", id)
		}
		seen[id] = true
	}
}

// Approval is the only local-only action in the system (INV-5, INV-2): a
// broker cannot grant it and no inbound command kind can express it. What it
// does is pin the commit the run was parked at and move it back to the queue
// with a fresh deadline.
func TestApproveQueuesAParkedRun(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	ctx := context.Background()

	parked := seedParkedRun(t, db, "r_parked")
	approvalDeadline := time.Now().Add(2 * time.Hour)
	if err := db.SetExpiry(ctx, "r_parked", approvalDeadline); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	approved, err := Approve(ctx, db, "r_parked", 48*time.Hour, now)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// What was approved is a commit. A ref can be force-pushed; a commit
	// cannot, which is the whole reason the pin exists (amendment A27).
	if approved.ApprovedSHA != parked.SHA {
		t.Errorf("approved_sha = %q, want the parked commit %q", approved.ApprovedSHA, parked.SHA)
	}

	r, err := db.Get(ctx, "r_parked")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateQueued {
		t.Fatalf("state = %s, want queued; §11 has no awaiting_approval → claimed edge", r.State)
	}
	// The deadline must be replaced rather than carried over: a run still
	// holding its approval_ttl would be swept as an expired queue entry by the
	// next ticker, minutes after a human said yes to it.
	if r.ExpiresAt == nil {
		t.Fatal("an approved run needs a queue deadline")
	}
	if !r.ExpiresAt.After(approvalDeadline) {
		t.Errorf("expires_at = %s, still the approval deadline %s", r.ExpiresAt, approvalDeadline)
	}

	// And it is actually claimable, which is the whole point.
	claimed, err := db.ClaimNext(ctx, []string{"test"})
	if err != nil {
		t.Fatalf("an approved run was not enqueued: %v", err)
	}
	if claimed.ID != "r_parked" {
		t.Errorf("claimed %q, want r_parked", claimed.ID)
	}
}

// Approving twice is something a person does. The second one must not read as
// an error.
func TestApproveIsIdempotent(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	ctx := context.Background()

	seedParkedRun(t, db, "r_twice")
	if _, err := Approve(ctx, db, "r_twice", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(ctx, db, "r_twice", time.Hour, time.Now()); err != nil {
		t.Errorf("a second approve should succeed: %v", err)
	}
}

func TestApproveRefusesWhatItCannotApprove(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	db := openStore(t, cfg)
	ctx := context.Background()

	seedRun(t, db, "r_running", protocol.StateRunning)
	seedRun(t, db, "r_done", protocol.StateSucceeded)

	for _, id := range []string{"r_running", "r_done"} {
		_, err := Approve(ctx, db, id, time.Hour, time.Now())
		if !errors.Is(err, ErrNotAwaitingApproval) {
			t.Errorf("Approve(%s) = %v, want ErrNotAwaitingApproval", id, err)
		}
	}

	// A run parked with no resolved commit — the shape an agent older than
	// A27 left behind. There is nothing to pin, and pinning nothing would mean
	// approving whatever the ref points at when the build starts.
	seedRun(t, db, "r_nocommit", protocol.StateAwaitingApproval)
	if _, err := Approve(ctx, db, "r_nocommit", time.Hour, time.Now()); !errors.Is(err, ErrNoCommit) {
		t.Errorf("Approve(r_nocommit) = %v, want ErrNoCommit", err)
	}

	if _, err := Approve(ctx, db, "r_missing", time.Hour, time.Now()); err == nil {
		t.Error("approving a run that does not exist should fail")
	}
	// An id that cannot name a run must be refused before it names a path.
	if _, err := Approve(ctx, db, "../../etc/passwd", time.Hour, time.Now()); err == nil {
		t.Error("approving a traversal id should fail at validation (INV-3, A6)")
	}
}
