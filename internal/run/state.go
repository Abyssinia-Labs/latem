package run

import (
	"context"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// transition persists a state change and then reports it (RUNNER_SPEC §11).
//
// The order is the rule. Local truth is written first, so a broker that is
// unreachable, slow, or lying never loses what happened on this machine; and a
// transition the store refuses — an edge the §11 machine does not have — is
// never reported at all, because reporting a state the agent could not reach
// would put a control plane ahead of reality rather than behind it.
//
// Reporting is asynchronous and the caller does not wait for it.
func (o *Orchestrator) transition(
	ctx context.Context, runID string, to protocol.RunState, u protocol.Update,
) error {
	u.State = to
	if err := o.store.Transition(ctx, runID, to, u); err != nil {
		return fmt.Errorf("run %s: transition to %s: %w", runID, to, err)
	}
	o.reporter.enqueue(runID, u)
	return nil
}

// terminalUpdate builds the update that ends a run.
func (o *Orchestrator) terminalUpdate(out outcome, refs []protocol.ArtifactRef) protocol.Update {
	finished := o.now()
	return protocol.Update{
		State:        out.state,
		ExitCode:     out.exitCode,
		ErrorCode:    out.code,
		ErrorMsg:     truncate(out.msg, MaxErrorMsgBytes),
		FinishedAt:   &finished,
		Artifacts:    refs,
		CancelReason: out.reason,
	}
}

// Approve moves an awaiting_approval run back into the queue (RUNNER_SPEC §11,
// `latem approve`).
//
// It puts the run in queued and not straight into a runner's hands: the §11
// machine has no awaiting_approval → claimed edge, and approving a run is
// saying it may be built, not that it must be built next.
//
// A run that is already queued is a success — approving twice is something a
// person does, and a second `latem approve` should not read as an error.
func (o *Orchestrator) Approve(ctx context.Context, runID string) error {
	r, err := o.store.Get(ctx, runID)
	if err != nil {
		return fmt.Errorf("run: approve %s: %w", runID, err)
	}
	switch r.State {
	case protocol.StateQueued:
		return nil
	case protocol.StateAwaitingApproval:
	default:
		return fmt.Errorf("run: approve %s: run is %s, not %s",
			runID, r.State, protocol.StateAwaitingApproval)
	}

	expires := o.now().Add(o.queueTTL())
	u := protocol.Update{State: protocol.StateQueued, ExpiresAt: &expires}
	if err := o.transition(ctx, runID, protocol.StateQueued, u); err != nil {
		return err
	}
	if err := o.store.Enqueue(ctx, runID, 0); err != nil {
		return fmt.Errorf("run: approve %s: enqueue: %w", runID, err)
	}
	return nil
}

// SweepExpired rejects waiting runs past their deadline and reports each one
// (RUNNER_SPEC §11, finding F7).
//
// The store does the sweep in a single statement, which is what makes it safe
// to run from the daily ticker and from startup at once. This adds the half
// the store cannot do: telling the broker. A run swept at startup after the
// agent was off for a week is precisely the one a control plane is still
// showing as queued.
func (o *Orchestrator) SweepExpired(ctx context.Context, now time.Time) ([]protocol.Update, error) {
	swept, err := o.store.SweepExpired(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("run: sweep: %w", err)
	}

	updates := make([]protocol.Update, 0, len(swept))
	for _, r := range swept {
		u := protocol.Update{
			State:      r.State,
			ErrorCode:  r.ErrorCode,
			ErrorMsg:   r.ErrorMsg,
			FinishedAt: r.FinishedAt,
		}
		// The sweep is a write the orchestrator did not make, so the store has
		// already persisted it; only the report is outstanding.
		o.reporter.enqueue(r.ID, u)
		o.reporter.forget(r.ID)
		updates = append(updates, u)
	}
	return updates, nil
}
