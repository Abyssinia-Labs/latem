package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestTransition_Legality walks the §11 diagram. The illegal cases are the
// reason this method exists: an executor that reports states out of order
// should fail its write, not leave behind a run history saying a build was
// queued after it finished.
func TestTransition_Legality(t *testing.T) {
	tests := []struct {
		from, to protocol.RunState
		want     bool
		why      string
	}{
		// The happy path, edge by edge.
		{protocol.StateQueued, protocol.StateClaimed, true, "a runner picks up queued work"},
		{protocol.StateClaimed, protocol.StatePreparing, true, "gates 6-18 begin"},
		{protocol.StatePreparing, protocol.StateRunning, true, "steps start"},
		{protocol.StateRunning, protocol.StateCollecting, true, "steps finished"},
		{protocol.StateCollecting, protocol.StateSucceeded, true, "every step exited 0"},

		// Waiting states and their exits.
		{protocol.StateAwaitingApproval, protocol.StateQueued, true, "latem approve"},
		{protocol.StateAwaitingApproval, protocol.StateRejected, true, "approval_ttl expired"},
		{protocol.StateQueued, protocol.StateRejected, true, "queue_ttl expired"},
		{protocol.StateAwaitingApproval, protocol.StateClaimed, false,
			"approval puts a run back in the queue; it does not hand it to a runner"},

		// Cancellation reaches every non-terminal state (§11).
		{protocol.StateQueued, protocol.StateCancelled, true, "nothing has executed"},
		{protocol.StateRunning, protocol.StateCancelled, true, "signalled mid-step"},
		{protocol.StateCollecting, protocol.StateCancelled, true,
			"collecting always runs, so a cancelled run reaches cancelled through it"},

		// Crash reconciliation (§6 step 5) does not respect the diagram.
		{protocol.StatePreparing, protocol.StateInterrupted, true, "agent died during checkout"},
		{protocol.StateCollecting, protocol.StateInterrupted, true, "agent died during teardown"},

		// Gate 16: a concurrency group sends a run back to the queue (§10).
		{protocol.StatePreparing, protocol.StateQueued, true, "concurrency group is busy"},

		// Skipping stages.
		{protocol.StateQueued, protocol.StateRunning, false, "nothing claimed or prepared it"},
		{protocol.StateQueued, protocol.StateSucceeded, false, "a run cannot succeed without executing"},
		{protocol.StateClaimed, protocol.StateRunning, false, "preparing was skipped"},

		// Going backwards.
		{protocol.StateRunning, protocol.StatePreparing, false, "steps do not un-start"},
		{protocol.StateRunning, protocol.StateQueued, false, "a running build is not queued"},
		{protocol.StateCollecting, protocol.StateRunning, false, "steps are over"},
		{protocol.StateClaimed, protocol.StateQueued, false, "a claim is not released by re-queueing"},

		// Terminal is terminal.
		{protocol.StateSucceeded, protocol.StateFailed, false, "a run has one outcome"},
		{protocol.StateFailed, protocol.StateRunning, false, "a finished run does not restart"},
		{protocol.StateRejected, protocol.StateQueued, false, "a rejected run is not re-admitted in place"},
		{protocol.StateInterrupted, protocol.StateCollecting, false, "the agent that owned it is gone"},
		{protocol.StateCancelled, protocol.StateSucceeded, false, "a cancelled run did not succeed"},
	}

	for _, tc := range tests {
		name := string(tc.from) + "_to_" + string(tc.to)
		t.Run(name, func(t *testing.T) {
			d := newTestDB(t)
			mustCreate(t, d, testRun("r", tc.from))

			err := d.Transition(t.Context(), "r", tc.to, protocol.Update{State: tc.to})
			switch {
			case tc.want && err != nil:
				t.Fatalf("%s -> %s was refused (%s): %v", tc.from, tc.to, tc.why, err)
			case !tc.want && err == nil:
				t.Fatalf("%s -> %s was allowed; %s", tc.from, tc.to, tc.why)
			case !tc.want && !errors.Is(err, ErrIllegalTransition):
				t.Fatalf("%s -> %s = %v, want ErrIllegalTransition", tc.from, tc.to, err)
			}

			got, err := d.Get(t.Context(), "r")
			if err != nil {
				t.Fatal(err)
			}
			wantState := tc.from
			if tc.want {
				wantState = tc.to
			}
			if got.State != wantState {
				t.Errorf("state = %q after the attempt, want %q", got.State, wantState)
			}
		})
	}
}

// TestTransition_TerminalStatesHaveNoExit derives the check from protocol
// rather than from the table, so a state that becomes terminal in pkg/protocol
// without losing its outgoing edges here is caught.
func TestTransition_TerminalStatesHaveNoExit(t *testing.T) {
	for _, s := range protocol.TerminalStates() {
		if edges := legalTransitions[s]; len(edges) != 0 {
			t.Errorf("terminal state %s has outgoing edges %v", s, edges)
		}
	}
	for from, edges := range legalTransitions {
		if from.IsTerminal() {
			t.Errorf("legalTransitions has an entry for terminal state %s", from)
		}
		for _, to := range edges {
			if !to.Valid() {
				t.Errorf("%s -> %s: unknown state", from, to)
			}
		}
	}
	// Every non-terminal state must be able to end, or a run could occupy the
	// hot partial index forever — the failure §11 added the TTLs to close.
	for s := range allNonTerminal() {
		var canEnd bool
		for _, to := range legalTransitions[s] {
			if to.IsTerminal() {
				canEnd = true
			}
		}
		if !canEnd {
			t.Errorf("non-terminal state %s has no path to a terminal state", s)
		}
	}
}

func allNonTerminal() map[protocol.RunState]bool {
	out := map[protocol.RunState]bool{}
	for _, s := range []protocol.RunState{
		protocol.StateQueued, protocol.StateAwaitingApproval, protocol.StateClaimed,
		protocol.StatePreparing, protocol.StateRunning, protocol.StateCollecting,
	} {
		out[s] = true
	}
	return out
}

// TestTransition_SelfIsIdempotent: updates are idempotent by (run id, state)
// and the agent replays unacknowledged ones after a reconnect (§7.2). Treating
// a replay as an ordering violation would make the reconnect path need a
// special case for its own contract.
func TestTransition_SelfIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateRunning))

	for range 2 {
		if err := d.Transition(t.Context(), "r", protocol.StateRunning, protocol.Update{}); err != nil {
			t.Fatalf("replaying a transition: %v", err)
		}
	}

	// Including out of a terminal state, which a broker may well re-ack.
	if err := d.Transition(t.Context(), "r", protocol.StateFailed, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Transition(t.Context(), "r", protocol.StateFailed, protocol.Update{}); err != nil {
		t.Fatalf("replaying a terminal transition: %v", err)
	}
}

func TestTransition_PersistsTheUpdate(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateCollecting))

	started := time.Date(2026, 8, 7, 9, 1, 0, 0, time.UTC)
	finished := time.Date(2026, 8, 7, 9, 5, 0, 0, time.UTC)
	exit := 2

	err := d.Transition(t.Context(), "r", protocol.StateFailed, protocol.Update{
		State:      protocol.StateFailed,
		ExitCode:   &exit,
		ErrorCode:  protocol.ErrStep,
		ErrorMsg:   "step 2 exited 2",
		StartedAt:  &started,
		FinishedAt: &finished,
		Artifacts: []protocol.ArtifactRef{
			{Path: "build/app.apk", SizeBytes: 42, SHA256: "deadbeef"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != protocol.StateFailed || got.ErrorCode != protocol.ErrStep {
		t.Errorf("state/code = %q/%q", got.State, got.ErrorCode)
	}
	if got.ExitCode == nil || *got.ExitCode != exit {
		t.Errorf("exit code = %v", got.ExitCode)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v", got.StartedAt)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %v", got.FinishedAt)
	}

	// The artifacts ride along with the terminal update and must land in the
	// same transaction: a crash between the two would leave a finished run with
	// no record of what it produced.
	arts, err := d.Artifacts(t.Context(), "r")
	if err != nil || len(arts) != 1 || arts[0].Path != "build/app.apk" {
		t.Errorf("artifacts = %+v, %v", arts, err)
	}
}

// TestTransition_RedactsErrorMessage: the field is documented as arriving
// redacted, and redacting again is one pass over a short string. Trusting the
// caller costs INV-4 the one time they forget.
func TestTransition_RedactsErrorMessage(t *testing.T) {
	const secret = "keychain-password-value"
	d := newTestDB(t, secret)
	mustCreate(t, d, testRun("r", protocol.StateRunning))

	err := d.Transition(t.Context(), "r", protocol.StateFailed, protocol.Update{
		ErrorCode: protocol.ErrStep,
		ErrorMsg:  "security unlock-keychain -p " + secret + " failed",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.ErrorMsg, secret) {
		t.Fatalf("error_message holds the secret: %q", got.ErrorMsg)
	}
	if !strings.Contains(got.ErrorMsg, "REDACTED") {
		t.Errorf("error_message = %q; it did not pass through the redactor", got.ErrorMsg)
	}
}

// TestTransition_ClearsExpiryOnLeavingAWaitingState: a claimed run that kept
// its queue_ttl deadline would be rejected by the next sweep while it was
// building.
func TestTransition_ClearsExpiryOnLeavingAWaitingState(t *testing.T) {
	d := newTestDB(t)
	deadline := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	r := testRun("r", protocol.StateAwaitingApproval)
	r.ExpiresAt = &deadline
	mustCreate(t, d, r)

	// Approval moves it to another waiting state; the deadline is the caller's
	// to reset (there is no field for it on Update), so it must survive.
	if err := d.Transition(t.Context(), "r", protocol.StateQueued, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("moving between two waiting states dropped the deadline")
	}

	if err := d.Transition(t.Context(), "r", protocol.StateClaimed, protocol.Update{}); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Get(t.Context(), "r"); err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v after leaving the queue; the sweep would reject a run in flight", got.ExpiresAt)
	}
}

// TestTransition_DequeuesOnLeavingTheQueue: a cancelled run left in the queue
// is a row ClaimNext looks at forever and never claims.
func TestTransition_DequeuesOnLeavingTheQueue(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateQueued))
	if err := d.Enqueue(t.Context(), "r", 0); err != nil {
		t.Fatal(err)
	}

	if err := d.Transition(t.Context(), "r", protocol.StateCancelled, protocol.Update{}); err != nil {
		t.Fatal(err)
	}

	var rows int
	if err := d.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM queue`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d queue rows survived a cancellation", rows)
	}
	if _, err := d.ClaimNext(t.Context(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClaimNext after cancelling the only queued run = %v, want ErrNotFound", err)
	}
}

func TestTransition_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateQueued))

	if err := d.Transition(t.Context(), "r", protocol.RunState("done-ish"), protocol.Update{}); err == nil {
		t.Error("Transition accepted an unknown state")
	}
	// An update built for a different transition means the caller assembled it
	// somewhere else; resolving that silently in favour of either field hides
	// the mistake.
	err := d.Transition(t.Context(), "r", protocol.StateClaimed,
		protocol.Update{State: protocol.StateRunning})
	if err == nil {
		t.Error("Transition accepted an update whose State disagreed with the target")
	}
	if err := d.Transition(t.Context(), "ghost", protocol.StateClaimed, protocol.Update{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Transition of an unknown run = %v, want ErrNotFound", err)
	}
}

// TestTransition_RejectsNonOutcomeErrorCodes: §21's codes are API that the web
// app maps to human copy, and E_AGENT_DOWN in particular is a CLI condition
// that never belongs to a run.
func TestTransition_RejectsNonOutcomeErrorCodes(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateRunning))

	for _, code := range []protocol.ErrorCode{protocol.ErrAgentDown, protocol.ErrorCode("E_MADE_UP")} {
		err := d.Transition(t.Context(), "r", protocol.StateFailed, protocol.Update{ErrorCode: code})
		if err == nil {
			t.Errorf("Transition accepted error code %q", code)
		}
	}

	rejected := testRun("bad-code", protocol.StateRejected)
	rejected.ErrorCode = protocol.ErrAgentDown
	if err := d.Create(t.Context(), rejected); err == nil {
		t.Error("Create accepted E_AGENT_DOWN as a run outcome")
	}

	// A real outcome code still passes.
	if err := d.Transition(t.Context(), "r", protocol.StateFailed,
		protocol.Update{ErrorCode: protocol.ErrStep}); err != nil {
		t.Errorf("Transition with E_STEP = %v", err)
	}
}

// TestTransition_IllegalChangesNothing: the refusal must roll back the whole
// transaction, artifacts included.
func TestTransition_IllegalChangesNothing(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateSucceeded))

	err := d.Transition(t.Context(), "r", protocol.StateRunning, protocol.Update{
		ErrorMsg:  "should not be written",
		ErrorCode: protocol.ErrStep,
		Artifacts: []protocol.ArtifactRef{{Path: "x", SizeBytes: 1, SHA256: "y"}},
	})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("err = %v, want ErrIllegalTransition", err)
	}

	got, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != protocol.StateSucceeded || got.ErrorMsg != "" || got.ErrorCode != "" {
		t.Errorf("a refused transition still wrote fields: %+v", got)
	}
	if arts, _ := d.Artifacts(t.Context(), "r"); len(arts) != 0 {
		t.Errorf("a refused transition still wrote artifacts: %+v", arts)
	}
}
