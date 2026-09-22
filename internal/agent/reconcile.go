package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// interruptedMsg is what a run interrupted by a crash says. It is the only
// error message in the system written by a process that was not there when the
// failure happened, so it says what is known and nothing more.
const interruptedMsg = "the agent died while this run was in flight; it was marked interrupted at the next startup (RUNNER_SPEC §6)"

// Reconciliation is what one startup reconciliation did.
//
// It is returned and logged rather than kept private because §6 step 5 is the
// only code path in the agent that must work on a machine where nothing else
// did, and a reap that says nothing is indistinguishable from a reap that
// found nothing.
type Reconciliation struct {
	// Interrupted are the runs a crash left mid-flight.
	Interrupted []string
	// Swept are the queued and awaiting_approval runs whose TTL had expired
	// while the agent was off.
	Swept []string
	// Reaped is what the executors removed: containers, VMs, env directories.
	Reaped []executor.Reaped
	// Workspaces are the abandoned clones under work/ that were deleted.
	Workspaces []string
	// Unowned are entries under work/ whose names are not valid run ids.
	// They are reported rather than removed: this agent will not rm -rf a path
	// it cannot vouch for having created.
	Unowned []string
}

// Empty reports whether reconciliation found nothing to do, which is the
// ordinary case for a clean shutdown.
func (r Reconciliation) Empty() bool {
	return len(r.Interrupted) == 0 && len(r.Swept) == 0 &&
		len(r.Reaped) == 0 && len(r.Workspaces) == 0 && len(r.Unowned) == 0
}

// Reconcile is RUNNER_SPEC §6 step 5, and it is the whole of the M4 acceptance
// criterion: after `kill -9` mid-run, a restart leaves zero orphaned
// containers, VMs, or workspaces, and the run reads interrupted.
//
// # Why it can only work at startup
//
// SIGKILL runs no defer, closes no client, and signals no container. Nothing
// the dying process does can help, so the guarantee has to be reconstructible
// from state written before the work began: the run row in sqlite, and the
// `latem.run-id` label every executor stamps on everything it creates. This
// function is where those two facts are joined.
//
// # The order matters
//
//  1. Interrupt the runs a crash left mid-flight. This must happen first,
//     because it is what makes them terminal, and therefore what makes their
//     containers and workspaces reapable in step 3.
//  2. Sweep expired waiting runs (§11). An agent that was off for a week comes
//     back to a queue full of runs nobody wants any more.
//  3. Reap, with a liveness predicate read after steps 1 and 2, so a run this
//     start is about to execute is never mistaken for litter.
//
// # What is deliberately not interrupted
//
// queued and awaiting_approval are non-terminal and §6's sentence is "any run
// left in a non-terminal state" — but those two are not "left" by a crash,
// they are the queue. §7.3 makes the queue the thing `latem run` writes to
// when no agent is running at all, so interrupting them at startup would
// destroy the feature the queue exists for. Their exit is the TTL sweep in
// step 2, which is what §11 says it is.
func (a *Agent) Reconcile(ctx context.Context) (Reconciliation, error) {
	var (
		rec  Reconciliation
		errs []error
	)

	interrupted, err := a.interruptCrashed(ctx)
	rec.Interrupted = interrupted
	if err != nil {
		errs = append(errs, err)
	}

	swept, err := a.sweepExpired(ctx)
	rec.Swept = swept
	if err != nil {
		errs = append(errs, err)
	}

	live, err := a.liveSet(ctx)
	if err != nil {
		// Without a liveness answer, reaping would delete the workspace of a
		// run about to execute. Refusing to reap is the safe direction: the
		// daily prune tries again, and a leaked container costs disk rather
		// than a wrong build.
		errs = append(errs, fmt.Errorf("agent: reconcile: cannot determine live runs, skipping reap: %w", err))
		a.logReconciliation(rec)
		return rec, errors.Join(errs...)
	}

	reaped, err := a.reapExecutors(ctx, live)
	rec.Reaped = reaped
	if err != nil {
		errs = append(errs, err)
	}

	gone, unowned, err := a.reapWorkspaces(live)
	rec.Workspaces, rec.Unowned = gone, unowned
	if err != nil {
		errs = append(errs, err)
	}

	a.logReconciliation(rec)
	return rec, errors.Join(errs...)
}

// interruptCrashed moves every run a crash left mid-flight to interrupted.
func (a *Agent) interruptCrashed(ctx context.Context) ([]string, error) {
	rows, err := a.db.NonTerminal(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: reconcile: listing non-terminal runs: %w", err)
	}

	var (
		ids  []string
		errs []error
	)
	now := a.now()
	for _, r := range rows {
		if !crashed(r.State) {
			continue
		}
		u := protocol.Update{
			State:      protocol.StateInterrupted,
			ErrorCode:  protocol.ErrInterrupted,
			ErrorMsg:   interruptedMsg,
			FinishedAt: &now,
		}
		if err := a.report(ctx, r.ID, u); err != nil {
			errs = append(errs, fmt.Errorf("agent: reconcile: interrupting %s: %w", r.ID, err))
			continue
		}
		ids = append(ids, r.ID)
	}
	return ids, errors.Join(errs...)
}

// crashed reports whether a state can only be occupied by a run something was
// actively driving — which, at startup, means nothing was.
func crashed(s protocol.RunState) bool {
	switch s {
	case protocol.StateClaimed, protocol.StatePreparing,
		protocol.StateRunning, protocol.StateCollecting:
		return true
	default:
		return false
	}
}

// report persists a transition and marks it reported, through the local broker
// when there is one so that the amendment A8 columns are written too.
//
// It is deliberately the local transport alone, and not the fan-out
// [Agent.brokerList] gives the orchestrator. This runs during §6's startup
// sequence, before there has been time for a control plane connection to exist;
// asking the remote transport would fail on every cold start, turn a normal
// condition into a reconciliation error, and make an unreachable hosted layer
// look like a broken machine. Nothing is lost by not asking: the transition is
// durable, this transport's broker_state row is the one being marked, and the
// remote one picks the run up from store.Unreported(remote) on its next
// connection (amendment A28).
// Local truth first, then the report. That order is §11's, and since A46 it is
// a precondition of [local.Broker.Report] rather than an expectation of it:
// the broker persists only what the run is still doing, because a report
// delivered late is indistinguishable from a first write unless every caller
// writes first. This one used to rely on Report to do both, which is why it
// was the caller that noticed.
func (a *Agent) report(ctx context.Context, runID string, u protocol.Update) error {
	if err := a.db.Transition(ctx, runID, u.State, u); err != nil {
		return err
	}
	if brk := a.brokerOrNil(); brk != nil {
		return brk.Report(ctx, runID, u)
	}
	return nil
}

// sweepExpired rejects waiting runs past their deadline and reports each one
// (§11, finding F7). It is called at startup and from the daily ticker.
//
// The store's own sweep is used rather than run.SweepExpired because that one
// returns [protocol.Update] values, which carry no run id — on the wire the id
// is the envelope — and an operator log line that cannot name what it dropped
// is exactly the silence §14.4 warns about. The rows come back with ids, and
// reporting them here goes through the same path every other transition does.
func (a *Agent) sweepExpired(ctx context.Context) ([]string, error) {
	rows, err := a.db.SweepExpired(ctx, a.now())
	if err != nil {
		return nil, fmt.Errorf("agent: sweeping expired runs: %w", err)
	}

	var (
		ids  []string
		errs []error
	)
	for _, r := range rows {
		ids = append(ids, r.ID)
		// The store has already persisted the transition; only the report is
		// outstanding, and a self-transition is idempotent by construction.
		u := protocol.Update{
			State:      r.State,
			ErrorCode:  r.ErrorCode,
			ErrorMsg:   r.ErrorMsg,
			FinishedAt: r.FinishedAt,
		}
		if rerr := a.report(ctx, r.ID, u); rerr != nil {
			errs = append(errs, fmt.Errorf("agent: reporting swept run %s: %w", r.ID, rerr))
		}
	}
	return ids, errors.Join(errs...)
}

// liveSet is the predicate every reaper consults: is this run id still in a
// non-terminal state in sqlite (§6 step 5, verbatim).
func (a *Agent) liveSet(ctx context.Context) (func(string) bool, error) {
	rows, err := a.db.NonTerminal(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rows))
	for _, r := range rows {
		set[r.ID] = true
	}
	return func(runID string) bool {
		// The orchestrator's own view is consulted as well, so that a run this
		// process started between the query and the reap is never treated as
		// litter. It cannot happen during startup reconciliation, and this
		// function is also the one the daily ticker uses.
		return set[runID] || a.isLive(runID)
	}, nil
}

// reapExecutors asks every executor to remove what it left behind (A23).
//
// Every runner is asked, including unavailable ones, because a Docker daemon
// that is unreachable now was reachable when it created the container and will
// be again. A runner with no executor at all is skipped: there is nothing to
// ask.
func (a *Agent) reapExecutors(ctx context.Context, live func(string) bool) ([]executor.Reaped, error) {
	var (
		all  []executor.Reaped
		errs []error
	)
	for _, r := range a.runners {
		if !r.usable() {
			continue
		}
		reaped, err := r.ex.Reap(ctx, live)
		all = append(all, reaped...)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent: reaping runner %q: %w", r.name, err))
		}
	}
	return all, errors.Join(errs...)
}

// reapWorkspaces deletes the clones under work/ whose runs are finished.
//
// §12's workspaces are standalone clones, so deleting one is a plain rm -rf
// with no administrative state left behind in the mirror — which is exactly
// why the design chose clones over worktrees and is what makes this three
// lines rather than a repair procedure.
func (a *Agent) reapWorkspaces(live func(string) bool) (gone, unowned []string, err error) {
	names, err := a.src.ListWorkspaces()
	if err != nil {
		return nil, nil, fmt.Errorf("agent: reconcile: listing workspaces: %w", err)
	}

	var errs []error
	for _, name := range names {
		if live(name) {
			continue
		}
		if verr := protocol.ValidateID(name); verr != nil {
			// Reported, never removed. A directory whose name is not a run id
			// was not created by this agent, and an rm -rf on a path it cannot
			// vouch for is how a reconciliation becomes an incident.
			unowned = append(unowned, name)
			continue
		}
		if rerr := a.src.RemoveWorkspace(name); rerr != nil {
			errs = append(errs, fmt.Errorf("agent: removing workspace %s: %w", name, rerr))
			continue
		}
		gone = append(gone, name)
	}
	return gone, unowned, errors.Join(errs...)
}

func (a *Agent) logReconciliation(rec Reconciliation) {
	if rec.Empty() {
		a.log.Info("reconciliation: nothing to clean up")
		return
	}
	if len(rec.Interrupted) > 0 {
		a.log.Warn("reconciliation: runs interrupted by a crash",
			"count", len(rec.Interrupted), "runs", rec.Interrupted)
	}
	if len(rec.Swept) > 0 {
		a.log.Info("reconciliation: expired waiting runs rejected",
			"count", len(rec.Swept), "runs", rec.Swept)
	}
	for _, r := range rec.Reaped {
		a.log.Warn("reconciliation: reaped an orphan",
			"kind", r.Kind, "ref", r.Ref, "run_id", r.RunID)
	}
	if len(rec.Workspaces) > 0 {
		a.log.Warn("reconciliation: removed abandoned workspaces",
			"count", len(rec.Workspaces), "runs", rec.Workspaces)
	}
	for _, name := range rec.Unowned {
		a.log.Error("reconciliation: an entry under work/ is not a run id and was left alone",
			"name", name, "dir", a.cfg.Paths.Work)
	}
}

// brokerOrNil returns the local broker, or nil when there is none.
//
// It exists so that [Agent.report]'s tolerance of having no broker at all —
// §7.3 has the agent doing useful work with no control plane — stays a single
// expression rather than a nil check at every call site.
func (a *Agent) brokerOrNil() broker.Broker {
	if a.localBrk == nil {
		return nil
	}
	return a.localBrk
}
