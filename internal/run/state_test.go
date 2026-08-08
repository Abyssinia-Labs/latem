package run

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// legalEdges is RUNNER_SPEC §11 as this package understands it. The test below
// drives every edge in it through the orchestrator's own transition helper and
// then asserts that every edge NOT in it is refused, so the table is a claim
// about the whole machine rather than about the paths that happen to be
// exercised elsewhere.
//
// Three entries are not arrows in the §11 diagram and are justified in
// internal/store's own table: any non-terminal state may reach interrupted,
// because a crash does not respect a diagram; preparing may return to queued,
// because gate 16 says "queued, wait, do not fail" (amendment A14); and
// collecting may reach cancelled, because a run cancelled while running
// reaches cancelled *through* collecting.
var legalEdges = map[protocol.RunState][]protocol.RunState{
	protocol.StateQueued: {
		protocol.StateClaimed, protocol.StateCancelled,
		protocol.StateRejected, protocol.StateInterrupted,
	},
	protocol.StateAwaitingApproval: {
		protocol.StateQueued, protocol.StateCancelled,
		protocol.StateRejected, protocol.StateInterrupted,
	},
	protocol.StateClaimed: {
		protocol.StatePreparing, protocol.StateCancelled, protocol.StateRejected,
		protocol.StateFailed, protocol.StateInterrupted,
	},
	protocol.StatePreparing: {
		protocol.StateRunning, protocol.StateQueued, protocol.StateCollecting,
		// Gate 8 parks a run for approval from here (amendment A17).
		protocol.StateAwaitingApproval,
		protocol.StateCancelled, protocol.StateRejected, protocol.StateFailed,
		protocol.StateTimedOut, protocol.StateInterrupted,
	},
	protocol.StateRunning: {
		protocol.StateCollecting, protocol.StateCancelled, protocol.StateFailed,
		protocol.StateTimedOut, protocol.StateInterrupted,
	},
	protocol.StateCollecting: {
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateTimedOut,
		protocol.StateCancelled, protocol.StateInterrupted,
	},
}

var allStates = []protocol.RunState{
	protocol.StateQueued, protocol.StateAwaitingApproval, protocol.StateClaimed,
	protocol.StatePreparing, protocol.StateRunning, protocol.StateCollecting,
	protocol.StateSucceeded, protocol.StateFailed, protocol.StateTimedOut,
	protocol.StateCancelled, protocol.StateRejected, protocol.StateInterrupted,
}

// walk is the shortest route to each state using only legal edges. Terminal
// states are entered from the closest non-terminal one rather than through a
// whole run, because the point is the edge under test and not the journey.
var walk = map[protocol.RunState][]protocol.RunState{
	protocol.StateQueued: {},
	// Created there by gate 4. It does have an inbound edge since A17
	// (preparing → awaiting_approval, gate 8), but reaching it that way costs
	// a walk through preparing for no extra coverage.
	protocol.StateAwaitingApproval: {},
	protocol.StateClaimed:          {protocol.StateClaimed},
	protocol.StatePreparing:        {protocol.StateClaimed, protocol.StatePreparing},
	protocol.StateRunning: {
		protocol.StateClaimed, protocol.StatePreparing, protocol.StateRunning,
	},
	protocol.StateCollecting: {
		protocol.StateClaimed, protocol.StatePreparing, protocol.StateCollecting,
	},
	protocol.StateSucceeded: {
		protocol.StateClaimed, protocol.StatePreparing,
		protocol.StateCollecting, protocol.StateSucceeded,
	},
	protocol.StateFailed:      {protocol.StateClaimed, protocol.StateFailed},
	protocol.StateTimedOut:    {protocol.StateClaimed, protocol.StatePreparing, protocol.StateTimedOut},
	protocol.StateCancelled:   {protocol.StateCancelled},
	protocol.StateRejected:    {protocol.StateRejected},
	protocol.StateInterrupted: {protocol.StateInterrupted},
}

// setupIn returns a harness holding run1 in the given state.
func setupIn(t *testing.T, from protocol.RunState) (*harness, *Orchestrator) {
	t.Helper()

	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	if from == protocol.StateAwaitingApproval {
		// The only way in: gate 4 parks it at Create.
		req.Ref = "feature/x"
	}
	if _, err := o.Admit(t.Context(), req, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	for _, s := range walk[from] {
		if err := o.transition(t.Context(), "run1", s, protocol.Update{}); err != nil {
			t.Fatalf("walking to %s via %s: %v", from, s, err)
		}
	}
	if got := mustGet(t, h.db, "run1").State; got != from {
		t.Fatalf("walked to %s, want %s", got, from)
	}
	return h, o
}

func TestStateMachine_EveryLegalEdgeIsAccepted(t *testing.T) {
	for from, tos := range legalEdges {
		for _, to := range tos {
			t.Run(string(from)+"→"+string(to), func(t *testing.T) {
				h, o := setupIn(t, from)
				if err := o.transition(t.Context(), "run1", to, protocol.Update{}); err != nil {
					t.Fatalf("%s → %s refused: %v", from, to, err)
				}
				if got := mustGet(t, h.db, "run1").State; got != to {
					t.Fatalf("state = %s, want %s", got, to)
				}
			})
		}
	}
}

func TestStateMachine_EveryIllegalEdgeIsRefused(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			if from == to || slices.Contains(legalEdges[from], to) {
				continue
			}
			t.Run(string(from)+"↛"+string(to), func(t *testing.T) {
				h, o := setupIn(t, from)
				err := o.transition(t.Context(), "run1", to, protocol.Update{})
				if !errors.Is(err, store.ErrIllegalTransition) {
					t.Fatalf("%s → %s was accepted (err = %v)", from, to, err)
				}
				if got := mustGet(t, h.db, "run1").State; got != from {
					t.Fatalf("a refused transition moved the run to %s", got)
				}
			})
		}
	}
}

// A self-transition is legal from everywhere, including a terminal state.
// Updates are idempotent by (run id, state) and the agent replays
// unacknowledged ones after a reconnect, so re-applying the state a run is
// already in is a replay rather than an ordering bug.
func TestStateMachine_SelfTransitionsAreReplays(t *testing.T) {
	for _, s := range allStates {
		if s == protocol.StateAwaitingApproval {
			continue // covered by its own test; walk cannot revisit it
		}
		t.Run(string(s), func(t *testing.T) {
			_, o := setupIn(t, s)
			if err := o.transition(t.Context(), "run1", s, protocol.Update{}); err != nil {
				t.Fatalf("%s → %s refused: %v", s, s, err)
			}
		})
	}
}

// The two gates that park a run for approval sit in different places, and
// amendment A17 is the difference. Gate 4 runs before a row exists, so Admit
// creates it already parked. Gate 8 runs during preparing because it needs a
// fetched mirror, so it needs an edge out of preparing — and without one the
// authoritative INV-5 check had nowhere to put the run.
func TestStateMachine_AwaitingApprovalIsReachableFromBothGates(t *testing.T) {
	// Gate 4: parked at Create.
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Ref = "feature/x"
	if _, err := o.Admit(t.Context(), req, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if got := mustGet(t, h.db, "run1").State; got != protocol.StateAwaitingApproval {
		t.Fatalf("state = %s", got)
	}

	// Gate 8: parked from preparing (A17).
	_, o2 := setupIn(t, protocol.StatePreparing)
	if err := o2.transition(t.Context(), "run1", protocol.StateAwaitingApproval, protocol.Update{}); err != nil {
		t.Fatalf("preparing → awaiting_approval refused: %v", err)
	}

	// It stays unreachable from states where no gate runs. A run already in
	// the queue, or merely claimed, has not evaluated a ref policy yet.
	for _, from := range []protocol.RunState{protocol.StateQueued, protocol.StateClaimed} {
		_, o3 := setupIn(t, from)
		err := o3.transition(t.Context(), "run1", protocol.StateAwaitingApproval, protocol.Update{})
		if !errors.Is(err, store.ErrIllegalTransition) {
			t.Errorf("%s → awaiting_approval should stay illegal, got %v", from, err)
		}
	}
}

// A transition the store refuses is never reported: reporting a state the
// agent could not reach puts a control plane ahead of reality.
func TestStateMachine_RefusedTransitionsAreNotReported(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	if _, err := o.Admit(t.Context(), h.request("test"), 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := o.transition(t.Context(), "run1", protocol.StateSucceeded, protocol.Update{}); err == nil {
		t.Fatal("queued → succeeded was accepted")
	}

	time.Sleep(20 * time.Millisecond)
	if slices.Contains(h.broker.states("run1"), protocol.StateSucceeded) {
		t.Fatal("a refused transition reached the broker")
	}
}

// Local truth is written first: a broker that refuses every report loses
// nothing, and the lag is visible through store.Unreported for replay.
func TestStateMachine_PersistsBeforeReporting(t *testing.T) {
	h := newHarness(t)
	h.broker.failNext = 1000
	h.opts.ReportAttempts = 1
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	if got := mustGet(t, h.db, "run1").State; got != protocol.StateSucceeded {
		t.Fatalf("stored state = %s", got)
	}
	if len(h.broker.updates()) != 0 {
		t.Fatal("the broker recorded something it rejected")
	}

	_ = o.Close()
	unreported, err := h.db.Unreported(t.Context())
	if err != nil {
		t.Fatalf("Unreported: %v", err)
	}
	if len(unreported) != 1 || unreported[0].ID != "run1" {
		t.Fatalf("unreported = %+v, want run1", unreported)
	}
}

func TestApprove(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Ref = "feature/x"
	if _, err := o.Admit(t.Context(), req, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	if err := o.Approve(t.Context(), "run1"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	row := mustGet(t, h.db, "run1")
	if row.State != protocol.StateQueued {
		t.Fatalf("state = %s, want queued", row.State)
	}
	if row.ExpiresAt == nil {
		t.Error("an approved run has no queue_ttl deadline")
	}

	// An approved run is claimable; an unapproved one was not.
	if _, err := h.db.ClaimNext(t.Context(), nil); err != nil {
		t.Fatalf("ClaimNext after approve: %v", err)
	}
}

func TestApprove_IsIdempotentAndRefusesTheWrongState(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	req.Ref = "feature/x"
	if _, err := o.Admit(t.Context(), req, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := o.Approve(t.Context(), "run1"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if err := o.Approve(t.Context(), "run1"); err != nil {
		t.Fatalf("second approve should be a no-op: %v", err)
	}

	if err := h.db.Claim(t.Context(), "run1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := o.Approve(t.Context(), "run1"); err == nil {
		t.Fatal("approving a claimed run was accepted")
	}
	if err := o.Approve(t.Context(), "never-existed"); err == nil {
		t.Fatal("approving an unknown run was accepted")
	}
}

func TestSweepExpired_RejectsAndReports(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	queued := h.request("test")
	if _, err := o.Admit(t.Context(), queued, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	unapproved := h.request("test")
	unapproved.ID = "run2"
	unapproved.Ref = "feature/x"
	if _, err := o.Admit(t.Context(), unapproved, 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	updates, err := o.SweepExpired(t.Context(), time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("swept %d, want 2", len(updates))
	}

	codes := map[protocol.ErrorCode]bool{}
	for _, u := range updates {
		if u.State != protocol.StateRejected {
			t.Errorf("swept to %s, want rejected", u.State)
		}
		codes[u.ErrorCode] = true
	}
	if !codes[protocol.ErrQueueExpired] || !codes[protocol.ErrApprovalExpired] {
		t.Errorf("codes = %v, want both E_QUEUE_EXPIRED and E_APPROVAL_EXPIRED", codes)
	}

	waitFor(t, "both sweeps reported", func() bool {
		return len(h.broker.states("run1")) >= 2 && len(h.broker.states("run2")) >= 2
	})
}

func TestSweepExpired_LeavesRunsWithTimeLeft(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	if _, err := o.Admit(t.Context(), h.request("test"), 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	updates, err := o.SweepExpired(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("swept %d runs that had time left", len(updates))
	}
}

func TestTerminalUpdate_TruncatesAndStamps(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	long := make([]byte, MaxErrorMsgBytes*2)
	for i := range long {
		long[i] = 'x'
	}
	u := o.terminalUpdate(outcome{state: protocol.StateFailed, msg: string(long)}, nil)
	if len(u.ErrorMsg) <= MaxErrorMsgBytes || len(u.ErrorMsg) > MaxErrorMsgBytes+64 {
		t.Fatalf("ErrorMsg is %d bytes, want ~%d with a marker", len(u.ErrorMsg), MaxErrorMsgBytes)
	}
	if u.FinishedAt == nil {
		t.Error("no finish time")
	}
}

// Amendment A8 added Runner, ExpiresAt and CancelReason to protocol.Update so
// those three §22 columns would be reachable through the Store interface. The
// orchestrator fills all three; this asserts they survive the round trip,
// because a field a caller sets and the store drops is worse than no field.
func TestTransition_CarriesTheA8Fields(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	if _, err := o.Admit(t.Context(), h.request("test"), 0); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	// Runner, on the way into a non-waiting state.
	for _, s := range []protocol.RunState{protocol.StateClaimed, protocol.StatePreparing} {
		if err := o.transition(t.Context(), "run1", s, protocol.Update{Runner: "docker-1"}); err != nil {
			t.Fatalf("transition to %s: %v", s, err)
		}
	}
	if got := mustGet(t, h.db, "run1").Runner; got != "docker-1" {
		t.Errorf("Update.Runner did not reach the row: %q", got)
	}

	// ExpiresAt, on the way back into a waiting one (gate 16's requeue).
	deadline := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	if err := o.transition(t.Context(), "run1", protocol.StateQueued,
		protocol.Update{ExpiresAt: &deadline}); err != nil {
		t.Fatalf("transition to queued: %v", err)
	}
	row := mustGet(t, h.db, "run1")
	if row.ExpiresAt == nil || !row.ExpiresAt.Equal(deadline) {
		t.Errorf("Update.ExpiresAt did not reach the row: %v, want %v", row.ExpiresAt, deadline)
	}

	// CancelReason, and the expiry cleared by leaving the waiting state.
	if err := o.transition(t.Context(), "run1", protocol.StateCancelled,
		protocol.Update{CancelReason: "stopped from the phone"}); err != nil {
		t.Fatalf("transition to cancelled: %v", err)
	}
	row = mustGet(t, h.db, "run1")
	if row.CancelReason != "stopped from the phone" {
		t.Errorf("Update.CancelReason did not reach the row: %q", row.CancelReason)
	}
	if row.ExpiresAt != nil {
		t.Errorf("leaving a waiting state did not clear the deadline: %v", row.ExpiresAt)
	}
}
