package remote

import "github.com/Abyssinia-Labs/latem/pkg/protocol"

// maxPendingPerRun bounds the transitions this transport remembers for one run
// while it cannot deliver them.
//
// A run has fewer than ten transitions in its whole life (§11), so this cannot
// fill in practice. The overflow path exists because "cannot happen" is not a
// reason to grow without a bound on the one code path that runs during an
// outage of unknown length.
const maxPendingPerRun = 32

// rememberReport records a transition this transport could not deliver, so the
// reconnect can replay it.
//
// broker_state.reported_state now answers per transport (amendment A28), so
// store.Unreported(remote) is a correct backstop rather than a permanently empty
// query. This in-memory copy is still the primary record and is not redundant
// with it: the row carries a *state*, and what a replay owes the control plane
// is the whole [protocol.Update] — the exit code, the finish time, the error
// message. Rebuilding one from the run row loses nothing important after a
// restart, which is what the backstop is for, but during a partition this
// transport can do better and does.
//
// Order is preserved, because a control plane that sees `collecting` after
// `succeeded` shows a finished build as still working.
func (b *Broker) rememberReport(runID string, u protocol.Update) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	q := b.pending[runID]
	// A repeat of a state already queued is the same transition, and §7.2 makes
	// it idempotent — queueing it twice would only replay it twice.
	for i, have := range q {
		if have.State == u.State {
			q[i] = u
			b.pending[runID] = q
			return
		}
	}
	if len(q) >= maxPendingPerRun {
		// Drop the oldest. The newest state is the one a control plane most
		// needs, and the history is still whole in sqlite.
		q = q[1:]
	}
	b.pending[runID] = append(q, u)
}

// forgetReport drops a transition that has been acknowledged.
func (b *Broker) forgetReport(runID string, state protocol.RunState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[runID]
	kept := q[:0]
	for _, u := range q {
		if u.State != state {
			kept = append(kept, u)
		}
	}
	if len(kept) == 0 {
		delete(b.pending, runID)
		return
	}
	b.pending[runID] = kept
}

// takePending removes and returns everything waiting to be replayed.
//
// It empties the map rather than iterating it in place so that a replay which
// fails halfway can put back only what it did not reach — and so that a
// [Broker.Report] arriving during the replay is not lost to a concurrent write.
func (b *Broker) takePending() map[string][]protocol.Update {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = map[string][]protocol.Update{}
	return out
}

// pendingCount reports how many transitions are waiting to be replayed. It
// exists for the tests that assert the memory both fills and empties.
func (b *Broker) pendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, q := range b.pending {
		n += len(q)
	}
	return n
}
