package remote

import (
	"context"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Store is the persistence this transport needs.
//
// It is narrower than [store.Store] in most places and wider in one, and the
// wide part has the same cause as internal/broker/local's: amendment A8 put
// Runner, ExpiresAt and CancelReason on [protocol.Update] so that three columns
// of the §22 schema became reachable, and [store.Store.Transition] applies none
// of them. Here only SetExpiry is needed — an inbound run enters queued with a
// queue_ttl deadline (§11) — and it is a method of the concrete *store.DB
// rather than of the interface.
//
// Transition is deliberately absent. A control plane does not move a run
// through the §11 state machine; the agent does, and [Broker.Report] publishes
// what already happened. A remote broker able to write a transition would be a
// remote broker able to assert that a build succeeded.
//
// The three methods that are not about accepting a run are the interesting
// ones:
//
//   - NonTerminal builds the resume frame. It is the answer to "what does this
//     machine still owe the control plane".
//   - Unreported is the restart-safe half of the transition replay. This
//     transport's own memory covers a reconnect; only the database covers a
//     process that died in between.
//   - MarkReported is what makes that answer shrink. Nothing else marks this
//     transport's rows (amendment A28), so a delivered update is recorded here
//     or it is replayed on every reconnect for the life of the machine.
//   - SetLastAckedSeq and LastAckedSeq persist and re-read the log resume
//     point, which is what makes finding F6 survive a restart rather than
//     merely a reconnect.
//
// The last three carry a broker key and this package always passes
// [store.BrokerRemote]. That key is amendment A28 and it is the reason this
// transport can be run beside the local one at all: the two words
// "acknowledged" and "persisted" used to share one column, which the local
// broker set the instant a transition landed on disk.
type Store interface {
	Create(ctx context.Context, r store.Run) error
	Get(ctx context.Context, runID string) (store.Run, error)
	// HoldToken keeps a workflow job's GitHub token in memory (A52).
	HoldToken(runID, token string)
	Enqueue(ctx context.Context, runID string, priority int) error
	SetExpiry(ctx context.Context, runID string, at time.Time) error

	NonTerminal(ctx context.Context) ([]store.Run, error)
	Unreported(ctx context.Context, brk string) ([]store.Run, error)
	MarkReported(ctx context.Context, brk, runID string, state protocol.RunState) error
	SetLastAckedSeq(ctx context.Context, brk, runID string, seq uint64) error
	LastAckedSeq(ctx context.Context, brk, runID string) (uint64, error)
}

// The remote broker is built against *store.DB, not against store.Store, and
// this line is where that stops being a footnote and becomes a compile error if
// the two drift.
var _ Store = (*store.DB)(nil)
