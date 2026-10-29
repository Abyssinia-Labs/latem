package local

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestClaim_RejectsATraversalID(t *testing.T) {
	b, _ := newBroker(t)
	if err := b.Claim(t.Context(), "../../etc/passwd"); err == nil {
		t.Fatal("Claim accepted a traversal run id")
	}
}

// Losing a claim must be distinguishable from every other failure, because it
// is the one the caller is meant to shrug at.
func TestClaim_LostRaceIsErrAlreadyClaimed(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_taken")

	if err := b.Claim(t.Context(), "r_taken"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	err := b.Claim(t.Context(), "r_taken")
	if !errors.Is(err, broker.ErrAlreadyClaimed) {
		t.Fatalf("second Claim = %v, want ErrAlreadyClaimed", err)
	}
	// The store's reason stays in the chain, so somebody debugging a claim can
	// still see which state the run was actually in.
	if !errors.Is(err, store.ErrNotClaimable) {
		t.Errorf("the store's reason was dropped: %v", err)
	}
}

func TestClaim_UnknownRunIsNotFound(t *testing.T) {
	b, _ := newBroker(t)
	if err := b.Claim(t.Context(), "r_nosuch"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Claim of an unknown run = %v, want ErrNotFound", err)
	}
}

// Report is what marks a transition acknowledged. Without that, every run
// stays in store.Unreported forever and the agent re-reports its whole history
// on each reconnect.
// persist writes local truth the way §11 requires before a report, which A46
// made a precondition of [Broker.Report] rather than an expectation of it.
func persist(t *testing.T, db *store.DB, id string, to protocol.RunState) {
	t.Helper()
	if err := db.Transition(t.Context(), id, to, protocol.Update{State: to}); err != nil {
		t.Fatalf("Transition %s -> %s: %v", id, to, err)
	}
}

func TestReport_PersistsAndMarksReported(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_report")
	persist(t, db, "r_report", protocol.StateClaimed)

	if err := b.Report(t.Context(), "r_report", protocol.Update{State: protocol.StateClaimed}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	run, err := db.Get(t.Context(), "r_report")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateClaimed {
		t.Errorf("state = %s, want claimed", run.State)
	}

	unreported, err := db.Unreported(t.Context(), store.BrokerLocal)
	if err != nil {
		t.Fatalf("Unreported: %v", err)
	}
	if len(unreported) != 0 {
		t.Errorf("%d runs still awaiting replay after Report", len(unreported))
	}

	// And only this transport's. "Persisted on this disk" is not "accepted by a
	// control plane", and while the two shared §22's single reported_state
	// column this call silently cleared the remote transport's replay set too —
	// so a run that finished during an outage was never sent to anybody
	// (amendment A28).
	remote, err := db.Unreported(t.Context(), store.BrokerRemote)
	if err != nil {
		t.Fatalf("Unreported(remote): %v", err)
	}
	if len(remote) != 1 || remote[0].ID != "r_report" {
		t.Errorf("Unreported(remote) = %+v; the local broker's acknowledgement satisfied another transport's replay condition", remote)
	}
}

// The interface calls Report idempotent by (run id, state) so that a replay
// after a reconnect is free. A local broker that errored on the second call
// would make reconnect handling need a special case for its own contract.
func TestReport_IsIdempotent(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_twice")

	u := protocol.Update{State: protocol.StateClaimed}
	if err := b.Report(t.Context(), "r_twice", u); err != nil {
		t.Fatalf("first Report: %v", err)
	}
	if err := b.Report(t.Context(), "r_twice", u); err != nil {
		t.Fatalf("second Report: %v", err)
	}
	_ = db
}

// Amendment A8 added Runner, ExpiresAt and CancelReason to protocol.Update
// precisely so three columns of the §22 schema became reachable. store.Store's
// Transition applies none of them, so if the broker does not, the amendment
// bought nothing.
func TestReport_AppliesTheA8Fields(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_a8")
	persist(t, db, "r_a8", protocol.StateClaimed)

	if err := b.Report(t.Context(), "r_a8", protocol.Update{
		State:  protocol.StateClaimed,
		Runner: "mac-mini-1",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	persist(t, db, "r_a8", protocol.StateCancelled)
	if err := b.Report(t.Context(), "r_a8", protocol.Update{
		State:        protocol.StateCancelled,
		CancelReason: "stopped from a phone",
	}); err != nil {
		t.Fatalf("Report cancelled: %v", err)
	}

	run, err := db.Get(t.Context(), "r_a8")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.Runner != "mac-mini-1" {
		t.Errorf("runner = %q, want mac-mini-1 (A8)", run.Runner)
	}
	if run.CancelReason != "stopped from a phone" {
		t.Errorf("cancel_reason = %q, want the reason (A8)", run.CancelReason)
	}
}

// Entering a waiting state is when a deadline is set (§11, §22), and a
// transition out of one clears it.
func TestReport_SetsAndClearsTheDeadline(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_ttl")

	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	if err := b.Report(t.Context(), "r_ttl", protocol.Update{
		State: protocol.StateQueued, ExpiresAt: &deadline,
	}); err != nil {
		t.Fatalf("Report queued: %v", err)
	}
	run, err := db.Get(t.Context(), "r_ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.ExpiresAt == nil || !run.ExpiresAt.Equal(deadline) {
		t.Fatalf("expires_at = %v, want %v", run.ExpiresAt, deadline)
	}

	persist(t, db, "r_ttl", protocol.StateClaimed)
	if err := b.Report(t.Context(), "r_ttl", protocol.Update{State: protocol.StateClaimed}); err != nil {
		t.Fatalf("Report claimed: %v", err)
	}
	run, err = db.Get(t.Context(), "r_ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.ExpiresAt != nil {
		t.Errorf("expires_at = %v after leaving the queue; the sweep would reject a run in flight", run.ExpiresAt)
	}
}

// An expiry on a running run would arm the TTL sweep against a build in
// flight. The field's own documentation says it belongs to a waiting state, so
// the contradiction is refused rather than silently honoured.
func TestReport_RefusesADeadlineOnANonWaitingState(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_bad_ttl")

	deadline := time.Now().Add(time.Hour)
	err := b.Report(t.Context(), "r_bad_ttl", protocol.Update{
		State: protocol.StateClaimed, ExpiresAt: &deadline,
	})
	if err == nil {
		t.Fatal("Report accepted an expiry on a non-waiting state")
	}
	if run, getErr := db.Get(t.Context(), "r_bad_ttl"); getErr != nil {
		t.Fatalf("Get: %v", getErr)
	} else if run.State != protocol.StateQueued {
		t.Errorf("state = %s; the refused report transitioned the run anyway", run.State)
	}
}

// A report describes a state the run was in. By the time it is delivered the
// run may have left it, and then the report must write nothing — this is A46,
// and the bug it was written for is the one below.
func TestReport_OfAStateTheRunHasLeftIsANoOp(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_order")
	persist(t, db, "r_order", protocol.StateClaimed)

	// Delivered late: the run left queued some time ago.
	if err := b.Report(t.Context(), "r_order", protocol.Update{
		State: protocol.StateQueued, ErrorMsg: "stale",
	}); err != nil {
		t.Fatalf("a late report is not an error: %v", err)
	}
	run, err := db.Get(t.Context(), "r_order")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateClaimed {
		t.Errorf("state = %s; a report rewound the run", run.State)
	}
	if run.ErrorMsg == "stale" {
		t.Error("a stale report wrote its fields anyway")
	}
}

// The failure A46 was written for, at the layer it happened on.
//
// A run is parked at gate 8 and the park is reported. A human approves it,
// which pins the commit and re-queues it (A27). Only then does the park's own
// report arrive. Applying it would rewind the run to awaiting_approval and —
// because entering that state clears the approval — throw away the human's
// decision, so the run would sit parked waiting to be approved a second time.
func TestReport_DoesNotUnapproveAnApprovedRun(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_appr")
	persist(t, db, "r_appr", protocol.StateClaimed)
	persist(t, db, "r_appr", protocol.StatePreparing)

	sha := strings.Repeat("a", 40)
	park := protocol.Update{State: protocol.StateAwaitingApproval, SHA: sha, ErrorMsg: "outside allowed_refs"}
	if err := db.Transition(t.Context(), "r_appr", protocol.StateAwaitingApproval, park); err != nil {
		t.Fatalf("park: %v", err)
	}

	now := time.Now()
	if _, err := db.Approve(t.Context(), "r_appr", now, now.Add(72*time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The park's report, arriving after all of that.
	if err := b.Report(t.Context(), "r_appr", park); err != nil {
		t.Fatalf("Report: %v", err)
	}

	run, err := db.Get(t.Context(), "r_appr")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued; the late park un-parked an approved run", run.State)
	}
	if run.ApprovedSHA != sha {
		t.Errorf("approved_sha = %q, want %q; the human's approval was discarded", run.ApprovedSHA, sha)
	}
}

func TestReport_Rejects(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_v")

	if err := b.Report(t.Context(), "../../x", protocol.Update{State: protocol.StateClaimed}); err == nil {
		t.Error("Report accepted a traversal run id")
	}
	if err := b.Report(t.Context(), "r_v", protocol.Update{State: "invented"}); err == nil {
		t.Error("Report accepted an unknown state")
	}
	if err := b.Report(t.Context(), "r_v", protocol.Update{}); err == nil {
		t.Error("Report accepted an update with no state")
	}
	_ = db
}

// A sink for a run that does not exist should fail while the caller still has
// context, not on the first batch of a build that has already started.
func TestLogs_RejectsUnknownAndInvalidRuns(t *testing.T) {
	b, _ := newBroker(t)

	if _, err := b.Logs(t.Context(), "r_nosuch"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Logs for an unknown run = %v, want ErrNotFound", err)
	}
	if _, err := b.Logs(t.Context(), "../../etc/passwd"); err == nil {
		t.Error("Logs accepted a traversal run id")
	}
}

// Acked is the agent's resume point: it persists the value and resumes from
// Acked()+1. Returning the highest sequence seen without the write would claim
// a durability the restart path does not have.
func TestSink_AckedIsPersisted(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_logs")

	sink, err := b.Logs(t.Context(), "r_logs")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if got := sink.Acked(); got != 0 {
		t.Errorf("Acked on a fresh run = %d, want 0", got)
	}

	if err := sink.Write(t.Context(), frames("r_logs", 1, 2, 3)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := sink.Acked(); got != 3 {
		t.Errorf("Acked = %d, want 3", got)
	}

	seq, err := db.LastAckedSeq(t.Context(), store.BrokerLocal, "r_logs")
	if err != nil {
		t.Fatalf("LastAckedSeq: %v", err)
	}
	if seq != 3 {
		t.Errorf("last_acked_seq(local) = %d, want 3", seq)
	}
	// Under this transport's key and no other. The remote transport has not
	// been given a frame, and a resume frame claiming otherwise would make the
	// control plane skip the whole of this run's output.
	if seq, err = db.LastAckedSeq(t.Context(), store.BrokerRemote, "r_logs"); err != nil || seq != 0 {
		t.Errorf("last_acked_seq(remote) = %d, %v; want 0 — this transport wrote to a key it does not own", seq, err)
	}

	// A sink opened after a restart resumes where the store left off rather
	// than claiming an ack of zero.
	resumed, err := b.Logs(t.Context(), "r_logs")
	if err != nil {
		t.Fatalf("Logs again: %v", err)
	}
	if got := resumed.Acked(); got != 3 {
		t.Errorf("Acked after reopen = %d, want 3", got)
	}
}

// A replayed batch must not rewind the resume point: the duplicated region is
// exactly the output covering whatever went wrong.
func TestSink_DoesNotRewind(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_replay")

	sink, err := b.Logs(t.Context(), "r_replay")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if err := sink.Write(t.Context(), frames("r_replay", 1, 2, 3, 4)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sink.Write(t.Context(), frames("r_replay", 2, 3)); err != nil {
		t.Fatalf("replayed Write: %v", err)
	}
	if got := sink.Acked(); got != 4 {
		t.Errorf("Acked = %d after a replay, want 4", got)
	}
	seq, err := db.LastAckedSeq(t.Context(), store.BrokerLocal, "r_replay")
	if err != nil {
		t.Fatalf("LastAckedSeq: %v", err)
	}
	if seq != 4 {
		t.Errorf("last_acked_seq = %d, want 4", seq)
	}
}

// One sink per run, and sequences are per-run. A frame from elsewhere would
// advance the wrong resume point, silently, in the direction that loses lines.
func TestSink_RefusesAnotherRunsFrames(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_mine")
	seedQueued(t, db, "r_yours")

	sink, err := b.Logs(t.Context(), "r_mine")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if err := sink.Write(t.Context(), frames("r_yours", 9)); err == nil {
		t.Fatal("a frame for another run was accepted")
	}
	if got := sink.Acked(); got != 0 {
		t.Errorf("Acked = %d after a refused batch, want 0", got)
	}
}

func TestSink_EmptyBatchAndCloseSemantics(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_close")

	sink, err := b.Logs(t.Context(), "r_close")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if err := sink.Write(t.Context(), nil); err != nil {
		t.Errorf("empty Write: %v", err)
	}
	if err := sink.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent: shutdown paths close things twice, and a second Close
	// reporting a failure would mask the first error that mattered.
	if err := sink.Close(t.Context()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := sink.Write(t.Context(), frames("r_close", 1)); err == nil {
		t.Error("a write after Close succeeded")
	}
}

// There is no local recipient for a heartbeat, so the honest implementation
// keeps the value where `latem doctor` can read it rather than pretending to
// send it somewhere.
func TestHeartbeat_IsReadableLocally(t *testing.T) {
	frozen := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	b, _ := newBroker(t, func(o *Options) { o.now = func() time.Time { return frozen } })

	if _, _, ok := b.Health(); ok {
		t.Error("Health reported a beat before any arrived")
	}

	h := protocol.MachineHealth{MachineID: "mini-1", Draining: true, DiskFreeGB: 120}
	if err := b.Heartbeat(t.Context(), h); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	got, at, ok := b.Health()
	if !ok {
		t.Fatal("Health reported no beat after one arrived")
	}
	if got.MachineID != "mini-1" || !got.Draining {
		t.Errorf("Health = %+v, want the value passed in", got)
	}
	if !at.Equal(frozen) {
		t.Errorf("beat time = %v, want %v", at, frozen)
	}
}

// frames builds a batch for one run at the given sequences.
func frames(runID string, seqs ...uint64) []protocol.LogFrame {
	out := make([]protocol.LogFrame, len(seqs))
	for i, s := range seqs {
		out[i] = protocol.LogFrame{
			RunID:  runID,
			Seq:    s,
			Stream: protocol.StreamStdout,
			At:     time.Unix(0, int64(s)).UTC(),
			Line:   "line",
		}
	}
	return out
}

// failingSeqStore is a *store.DB that cannot answer for a transport's resume
// point. Everything else about the local broker is exercised against a real
// database on purpose (see the note at the top of helpers_test.go); this one
// path cannot be, because the only ways to make store.LastAckedSeq fail are a
// run that does not exist — which Get has already refused by then — and a
// database that is gone.
type failingSeqStore struct {
	Store
	err error
}

func (f failingSeqStore) LastAckedSeq(context.Context, string, string) (uint64, error) {
	return 0, f.err
}

// A sink that cannot read its own resume point must refuse to open rather than
// start at zero. Starting at zero would re-send the whole of a run's log after
// a restart, and finding F6 exists to make that impossible.
func TestLogs_RefusesWhenTheResumePointCannotBeRead(t *testing.T) {
	b, db := newBroker(t)
	seedQueued(t, db, "r_noseq")

	b.st = failingSeqStore{Store: db, err: errors.New("state database is gone")}
	if _, err := b.Logs(t.Context(), "r_noseq"); err == nil {
		t.Fatal("Logs opened a sink without knowing where the transport had got to; it would resume from zero and re-send the whole log")
	}
}
