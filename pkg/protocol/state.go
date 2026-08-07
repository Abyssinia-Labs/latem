package protocol

// RunState is a run's position in the lifecycle defined by RUNNER_SPEC §11.
type RunState string

const (
	// StateQueued means accepted and waiting for a slot or a concurrency group.
	StateQueued RunState = "queued"
	// StateAwaitingApproval means the ref fell outside policy (INV-5) and a
	// local `latem approve` is required. A broker cannot grant it.
	StateAwaitingApproval RunState = "awaiting_approval"
	// StateClaimed means a runner owns the run; nothing has executed.
	StateClaimed RunState = "claimed"
	// StatePreparing covers fetch, checkout, registry read, cache mount, env build.
	StatePreparing RunState = "preparing"
	// StateRunning means steps are executing.
	StateRunning RunState = "running"
	// StateCollecting covers artifact globbing, cache flush, teardown. It runs
	// even after a failure, so a broken build still yields its artifacts.
	StateCollecting RunState = "collecting"

	// StateSucceeded is terminal: every step exited 0.
	StateSucceeded RunState = "succeeded"
	// StateFailed is terminal: a step exited non-zero, or a gate failed.
	StateFailed RunState = "failed"
	// StateTimedOut is terminal: a job or step wall clock was exceeded.
	StateTimedOut RunState = "timed_out"
	// StateCancelled is terminal: an operator or a broker stopped the run.
	StateCancelled RunState = "cancelled"
	// StateRejected is terminal: an admission gate refused it (§10), or it
	// expired waiting for approval or a slot (§11).
	StateRejected RunState = "rejected"
	// StateInterrupted is terminal: the agent died mid-run. Set during startup
	// reconciliation, never by a running agent about itself.
	StateInterrupted RunState = "interrupted"
)

// terminalStates is the closed set of states from which no transition exists.
// The partial index in RUNNER_SPEC §22 is defined as the complement of this
// set, so the two must agree; a state added here without a matching migration
// silently changes which rows that index covers.
var terminalStates = map[RunState]bool{
	StateSucceeded:   true,
	StateFailed:      true,
	StateTimedOut:    true,
	StateCancelled:   true,
	StateRejected:    true,
	StateInterrupted: true,
}

// allStates is every legal value, used by [RunState.Valid].
var allStates = map[RunState]bool{
	StateQueued:           true,
	StateAwaitingApproval: true,
	StateClaimed:          true,
	StatePreparing:        true,
	StateRunning:          true,
	StateCollecting:       true,
	StateSucceeded:        true,
	StateFailed:           true,
	StateTimedOut:         true,
	StateCancelled:        true,
	StateRejected:         true,
	StateInterrupted:      true,
}

// IsTerminal reports whether the run has finished and will not transition again.
func (s RunState) IsTerminal() bool { return terminalStates[s] }

// IsWaiting reports whether the run is parked pending something external — a
// slot, a concurrency group, or a human. These are exactly the states that
// carry an expiry deadline (RUNNER_SPEC §11).
func (s RunState) IsWaiting() bool {
	return s == StateQueued || s == StateAwaitingApproval
}

// Valid reports whether s is a known state.
func (s RunState) Valid() bool { return allStates[s] }

// TerminalStates returns the terminal set. Callers that build SQL predicates
// should derive them from this rather than hardcoding a list, so §22's partial
// index and this package cannot drift apart.
func TerminalStates() []RunState {
	out := make([]RunState, 0, len(terminalStates))
	for s := range terminalStates {
		out = append(out, s)
	}
	return out
}
