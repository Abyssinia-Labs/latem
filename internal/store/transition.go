package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// legalTransitions is the RUNNER_SPEC §11 state machine as an adjacency set.
//
// It is a table rather than a chain of ifs because the interesting question is
// not "is this edge allowed" but "what is the complete set of edges", and a
// table can be read against the diagram in §11 line by line. Terminal states
// are absent by construction: they are the states with no outgoing edge, and
// the test asserts that absence equals protocol.TerminalStates() so this and
// §22's partial index cannot disagree about which states are hot.
//
// Three entries need justifying because they are not arrows in the §11 diagram:
//
//   - Every non-terminal state may reach interrupted. §6 step 5 marks whatever
//     a crash left behind, and a crash does not respect the diagram.
//   - preparing may return to queued. Gate 16 in §10 blocks a run on its
//     concurrency group with the outcome "queued — wait, do not fail", and
//     gates 6-18 run during preparing. The §11 diagram has no such arrow; §10
//     requires it.
//   - preparing may reach awaiting_approval, and without this edge INV-5 is
//     unenforceable. §11 draws "gate 4 / 8 ──▶ awaiting_approval" as though
//     both gates sat in the same place, but they do not: gate 4 is the cheap
//     pre-check that runs before a run row exists, so Admit can simply create
//     the row already parked. Gate 8 is the *authoritative* ref-policy check,
//     it runs during preparing because it needs a fetched mirror, and with no
//     edge out of preparing it had nowhere to put the run. The consequence was
//     specific: a ref that gate 4 admitted optimistically — a tag under a
//     branch policy, say — could not be parked for approval by the check that
//     actually decides (amendment A17).
//   - collecting may reach cancelled. §11 says collecting always runs, "including
//     after failed, timed_out, and cancelled", so a run cancelled while running
//     reaches cancelled *through* collecting. ("A cancel arriving during
//     collecting is ignored" is a rule for the executor, not for this table.)
var legalTransitions = map[protocol.RunState][]protocol.RunState{
	protocol.StateQueued: {
		protocol.StateClaimed,
		protocol.StateCancelled,
		protocol.StateRejected,
		protocol.StateInterrupted,
	},
	protocol.StateAwaitingApproval: {
		// `latem approve` puts an approved run back in the queue; it does not
		// hand it straight to a runner. There is no edge to claimed.
		protocol.StateQueued,
		protocol.StateCancelled,
		protocol.StateRejected,
		protocol.StateInterrupted,
	},
	protocol.StateClaimed: {
		protocol.StatePreparing,
		protocol.StateCancelled,
		protocol.StateRejected,
		protocol.StateFailed,
		protocol.StateInterrupted,
	},
	protocol.StatePreparing: {
		protocol.StateRunning,
		// A workflow job whose if: is false as it starts (A52). The
		// condition is read from the registry compiled during preparing,
		// so this is the one place the answer is known.
		protocol.StateSkipped,
		protocol.StateQueued,
		protocol.StateAwaitingApproval,
		protocol.StateCollecting,
		protocol.StateCancelled,
		protocol.StateRejected,
		protocol.StateFailed,
		protocol.StateTimedOut,
		protocol.StateInterrupted,
	},
	protocol.StateRunning: {
		protocol.StateCollecting,
		protocol.StateCancelled,
		protocol.StateFailed,
		protocol.StateTimedOut,
		protocol.StateInterrupted,
	},
	protocol.StateCollecting: {
		protocol.StateSucceeded,
		protocol.StateFailed,
		protocol.StateTimedOut,
		protocol.StateCancelled,
		protocol.StateInterrupted,
	},
}

// legal reports whether from → to is an edge of the §11 machine.
//
// A self-transition is legal everywhere, including out of a terminal state.
// Updates are idempotent by (run id, state) (§7.2) and the agent replays
// unacknowledged ones after a reconnect, so re-applying the transition a run is
// already in is a replay, not an ordering bug — and turning a replay into an
// error would make reconnect handling need a special case for its own contract.
func legal(from, to protocol.RunState) bool {
	if from == to {
		return true
	}
	return slices.Contains(legalTransitions[from], to)
}

// Transition persists a state change and the fields that accompany it.
//
// Local truth is written here before anything is reported (§11), so the whole
// operation is one transaction: state, outcome, timestamps, artifacts, and the
// queue row all move together or not at all. A crash between the state change
// and the artifact rows would otherwise produce a succeeded run with no record
// of what it produced.
//
// Three side effects are part of the contract rather than conveniences:
//
//   - Leaving a waiting state clears expires_at (§22). A claimed run must not
//     keep a queue_ttl deadline, or the sweep would reject a run in flight.
//   - Leaving queued deletes the queue row. The queue is the set of runs waiting
//     to be claimed; a cancelled or rejected run left in it is a row that
//     ClaimNext will look at forever and never claim.
//   - u.Artifacts is recorded, because the collecting → succeeded update is
//     where §7.2 carries them.
//
// It returns [ErrNotFound] if the run does not exist and [ErrIllegalTransition]
// if the §11 machine has no such edge.
func (d *DB) Transition(ctx context.Context, runID string, to protocol.RunState, u protocol.Update) error {
	if !to.Valid() {
		return fmt.Errorf("store: Transition: unknown state %q", to)
	}
	// Update.State is the same fact as `to`. Disagreement means the caller built
	// the update for a different transition, which is worth refusing rather than
	// silently resolving in favour of either one.
	if u.State != "" && u.State != to {
		return fmt.Errorf("store: Transition to %q with an update for %q", to, u.State)
	}
	if err := checkErrorCode(u.ErrorCode); err != nil {
		return fmt.Errorf("store: Transition: %w", err)
	}

	err := d.tx(ctx, func(t *sql.Tx) error {
		_, err := d.apply(ctx, t, runID, to, u, "")
		return err
	})
	if err == nil && to.IsTerminal() {
		d.tokens.Delete(runID)
		d.fetchTokens.Delete(runID)
	}
	return err
}

// ReapplyIfCurrent re-applies an update the run is still in the state of, and
// reports whether it did.
//
// It exists for the store half of a broker report (§7.2). A report describes a
// transition the agent has already written, so applying it again is meant to be
// an idempotent self-transition — and "meant to be" was the defect. A report is
// delivered asynchronously, so by the time it lands the run may legitimately
// have moved on: approved, re-queued and claimed again. Applying it then is not
// idempotent, it is a rewind, and §11's graph cannot tell the two apart because
// the edge a rewind travels is a real one. preparing → awaiting_approval is how
// gate 8 parks a run; it is also how a late report of an old park un-approves a
// run a human has already said yes to, discarding the approval on the way
// (amendment A27's clearing rule, doing exactly what it should to the wrong
// row).
//
// The guard is the update's own state. If the run is no longer there, the store
// holds something newer and the report is a description of the past, so nothing
// is written and the caller is told so.
func (d *DB) ReapplyIfCurrent(ctx context.Context, runID string, u protocol.Update) (bool, error) {
	to := u.State
	if !to.Valid() {
		return false, fmt.Errorf("store: ReapplyIfCurrent: unknown state %q", to)
	}
	if err := checkErrorCode(u.ErrorCode); err != nil {
		return false, fmt.Errorf("store: ReapplyIfCurrent: %w", err)
	}

	var applied bool
	err := d.tx(ctx, func(t *sql.Tx) error {
		var err error
		applied, err = d.apply(ctx, t, runID, to, u, to)
		return err
	})
	return applied, err
}

// apply is the body both entry points share.
//
// guard, when non-empty, is a state the run must already be in for the update
// to be written at all. It answers false and writes nothing when the run has
// moved elsewhere, which is a report arriving late rather than an error.
func (d *DB) apply(
	ctx context.Context, t *sql.Tx, runID string, to protocol.RunState, u protocol.Update, guard protocol.RunState,
) (bool, error) {
	var from protocol.RunState
	err := t.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	if err != nil {
		return false, fmt.Errorf("store: Transition %s: %w", runID, err)
	}

	// The guard, before legality: a late report is not an illegal transition,
	// it is a true statement about a state the run has already left.
	if guard != "" && from != guard {
		return false, nil
	}

	if !legal(from, to) {
		return false, fmt.Errorf("%w: run %s: %s -> %s", ErrIllegalTransition, runID, from, to)
	}

	set := []string{"state = ?"}
	args := []any{string(to)}

	if u.ExitCode != nil {
		set = append(set, "exit_code = ?")
		args = append(args, *u.ExitCode)
	}
	if u.ErrorCode != "" {
		set = append(set, "error_code = ?")
		args = append(args, string(u.ErrorCode))
	}
	if u.ErrorMsg != "" {
		// The field is documented as redacted before it reaches here.
		// Redacting again is idempotent and costs one pass over a short
		// string; trusting the caller costs INV-4 the one time they forget.
		set = append(set, "error_message = ?")
		args = append(args, d.red.Redact(u.ErrorMsg))
	}
	if u.StartedAt != nil {
		set = append(set, "started_at = ?")
		args = append(args, nullTime(u.StartedAt))
	}
	if u.FinishedAt != nil {
		set = append(set, "finished_at = ?")
		args = append(args, nullTime(u.FinishedAt))
	}
	// Amendment A8 added these three to [protocol.Update] precisely so the
	// columns would be reachable through the Store interface. Applying
	// them here is the other half of that change: without it the fields
	// exist, callers set them, and Transition drops them silently — which
	// is worse than not having added them, because the caller has no way
	// to tell.
	if u.Runner != "" {
		set = append(set, "runner = ?")
		args = append(args, u.Runner)
	}
	// Amendment A21. Both are decided at gates 7 and 9, long after Create,
	// and §22 has columns for exactly this pair — but nothing could write
	// them, so the audit trail INV-5 exists to leave was permanently NULL.
	if u.RefNormalized != "" {
		set = append(set, "ref_normalized = ?")
		args = append(args, u.RefNormalized)
	}
	if u.SHA != "" {
		set = append(set, "sha = ?")
		args = append(args, u.SHA)
	}
	if u.CancelReason != "" {
		// Display-only text from outside the trust boundary (§7.1), so it
		// gets the same treatment as ErrorMsg.
		set = append(set, "cancel_reason = ?")
		args = append(args, d.red.Redact(u.CancelReason))
	}

	switch {
	case !to.IsWaiting():
		// Leaving a waiting state always clears the deadline, whatever the
		// caller passed.
		set = append(set, "expires_at = NULL")
	case u.ExpiresAt != nil:
		set = append(set, "expires_at = ?")
		args = append(args, nullTime(u.ExpiresAt))
	}

	// Entering awaiting_approval always discards any approval on the row
	// (amendment A27), and it is done here rather than left to the caller
	// so that no path can forget. The case it exists for is the whole
	// reason approval pins a commit: a run approved at commit A, whose ref
	// then moved to B, is parked again by gate 8 — and if the old approval
	// survived, the next attempt would find a stale approved_sha and the
	// operator would never be asked about B. Clearing it in the same
	// statement that records the new sha means the row can never say
	// "approved" about a commit nobody was shown.
	if to == protocol.StateAwaitingApproval {
		set = append(set, "approved_at = NULL", "approved_sha = NULL")
	}

	args = append(args, runID)
	// The assignments joined here are literals appended above, one per field
	// the update actually carries; every value is bound.
	res, err := t.ExecContext(ctx,
		`UPDATE runs SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...) //nolint:gosec // no caller-supplied value is concatenated into this SQL
	if err != nil {
		return false, fmt.Errorf("store: Transition %s: %w", runID, err)
	}
	if err := affectedOne(res, runID, "Transition"); err != nil {
		return false, err
	}

	if to != protocol.StateQueued {
		if _, err := t.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
			return false, fmt.Errorf("store: Transition %s: dequeue: %w", runID, err)
		}
	}
	return true, insertArtifacts(ctx, t, runID, u.Artifacts)
}
