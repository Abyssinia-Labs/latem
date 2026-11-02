package local

import (
	"context"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Store is the persistence the local broker needs.
//
// It is deliberately narrower than [store.Store] in most places and
// deliberately wider in exactly one, and the wider part is worth explaining
// because it looks like an accident.
//
// Amendment A8 gave [protocol.Update] three new fields — Runner, ExpiresAt and
// CancelReason — precisely so that three columns of the RUNNER_SPEC §22 schema
// became reachable through the interface that owns them. But
// [store.Store.Transition] applies none of them: the methods that do
// (SetRunner, SetExpiry, SetCancelReason) are methods of the concrete
// *store.DB and are not declared on [store.Store]. So a broker holding a
// [store.Store] can be handed an Update carrying a runner name and has no way
// to persist it, which is the same gap A8 was written to close, one layer up.
//
// Rather than silently dropping those fields, this interface asks for the
// three setters by name. *store.DB satisfies it; [store.Store] alone does not.
// The var below is what keeps that claim honest.
type Store interface {
	Create(ctx context.Context, r store.Run) error
	Get(ctx context.Context, runID string) (store.Run, error)
	// HoldToken keeps a workflow job's GitHub token in memory (A52).
	HoldToken(runID, token string)
	// HoldFetchToken keeps the token its fetches use in memory (A72).
	HoldFetchToken(runID, token string)
	Enqueue(ctx context.Context, runID string, priority int) error

	// Claim is the conditional UPDATE behind [Broker.Claim]. Its atomicity is
	// the whole reason the local broker owns no queue of its own.
	Claim(ctx context.Context, runID string) error

	Transition(ctx context.Context, runID string, to protocol.RunState, u protocol.Update) error

	// ReapplyIfCurrent is Transition guarded on the run still being where the
	// update says it is. [Broker.Report] uses it rather than Transition
	// because a report is delivered asynchronously and may describe a state
	// the run has since left.
	ReapplyIfCurrent(ctx context.Context, runID string, u protocol.Update) (bool, error)

	// The broker_state trio (amendment A28). Each takes the key of the
	// transport asking, and this package always passes [store.BrokerLocal]:
	// what the local transport has accepted is a different fact from what a
	// control plane has accepted, and before the key existed the two were one
	// column that the local broker always won.
	MarkReported(ctx context.Context, brk, runID string, state protocol.RunState) error
	SetLastAckedSeq(ctx context.Context, brk, runID string, seq uint64) error
	LastAckedSeq(ctx context.Context, brk, runID string) (uint64, error)

	// The A8 setters. See the type comment.
	SetRunner(ctx context.Context, runID, runner string) error
	SetExpiry(ctx context.Context, runID string, at time.Time) error
	SetCancelReason(ctx context.Context, runID, reason string) error
}

// The local broker is built against *store.DB, not against store.Store, and
// this line is where that stops being a footnote and becomes a compile error
// if the two drift.
var _ Store = (*store.DB)(nil)
