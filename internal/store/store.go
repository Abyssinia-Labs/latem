// Package store is the agent's local source of truth.
//
// This file is owned by WS-00 and frozen. The sqlite implementation is WS-05.
//
// Local truth outranks the broker's view. Transitions are persisted here
// BEFORE they are reported, so a broker that is unreachable, slow, or lying
// never loses what actually happened on this machine (RUNNER_SPEC §11).
//
// Of the interfaces frozen in WS-00 this one is the most likely to need
// amendment — it has the widest surface and the least prior art in the spec.
// Amending it is cheap and expected; diverging from it quietly is not.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")

// ErrNotClaimable means a conditional claim did not apply because the run had
// already left the queued state. It is an ordinary race outcome.
var ErrNotClaimable = errors.New("store: run not claimable")

// Run is one row of the runs table (RUNNER_SPEC §22).
type Run struct {
	ID   string
	Repo string
	Job  string

	// Ref is as supplied, after INV-3 validation. RefNormalized is the
	// fully-qualified name policy was actually matched against — keeping both
	// makes an audit answerable ("requested as main, matched refs/heads/main")
	// without re-deriving it from a mirror that has since moved on.
	Ref           string
	RefNormalized string
	SHA           string

	Runner    string
	State     protocol.RunState
	ExitCode  *int
	ErrorCode protocol.ErrorCode
	ErrorMsg  string // redacted before it reaches here

	OriginKind   string
	OriginActor  string
	CancelReason string // redacted before it reaches here
	Inputs       map[string]string

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	// ExpiresAt is the approval_ttl or queue_ttl deadline. Set on entering a
	// waiting state, cleared on leaving it.
	ExpiresAt *time.Time

	// ReportedState is the last state a broker acknowledged. Replaying every
	// run where ReportedState != State is what makes reporting recoverable.
	ReportedState protocol.RunState
	// LastAckedSeq is the log resume point (finding F6).
	LastAckedSeq uint64
}

// Filter selects runs for listing.
type Filter struct {
	Repo   string
	States []protocol.RunState
	Limit  int
}

// Store persists runs, the local queue, artifacts, and cache metadata.
type Store interface {
	// Migrate applies embedded migrations. Called once at startup, before
	// anything else touches the database.
	Migrate(ctx context.Context) error

	Create(ctx context.Context, r Run) error
	Get(ctx context.Context, runID string) (Run, error)
	List(ctx context.Context, f Filter) ([]Run, error)

	// Transition persists a state change and any fields that accompany it. It
	// MUST reject a transition the state machine does not allow, so an
	// ordering bug surfaces here rather than as a nonsensical run history.
	Transition(ctx context.Context, runID string, to protocol.RunState, u protocol.Update) error

	// Enqueue adds a run to the local queue.
	Enqueue(ctx context.Context, runID string, priority int) error

	// ClaimNext atomically claims the highest-priority claimable run,
	// returning [ErrNotFound] when there is nothing to claim. Atomicity is the
	// whole contract: this is a conditional UPDATE relying on sqlite's write
	// serialization, not a read followed by a write.
	//
	// "Claimable" means unpinned, or pinned to one of the given runner names.
	// It deliberately does NOT mean "whose job can run on these runners",
	// which is what draft 02's comment claimed and which is unanswerable here:
	// a job's runs_on lives in jobs.yaml at the resolved commit, and INV-5
	// says that file is not read until after checkout — which happens after
	// the claim. Label matching is gate 13, in internal/registry, and a run
	// that fails it is rejected with E_NO_RUNNER having already been claimed
	// (amendment A12).
	ClaimNext(ctx context.Context, runners []string) (Run, error)

	// Claim atomically claims one specific run, returning [ErrNotClaimable] if
	// it has already left the queue.
	Claim(ctx context.Context, runID string) error

	// SweepExpired moves waiting runs past their deadline to rejected, with
	// E_APPROVAL_EXPIRED or E_QUEUE_EXPIRED, and returns what it swept. Run
	// from the daily ticker and at startup — an agent that was off for a week
	// comes back to a queue full of runs nobody wants (RUNNER_SPEC §11).
	SweepExpired(ctx context.Context, now time.Time) ([]Run, error)

	// Unreported returns runs whose ReportedState lags State, for replay after
	// a broker reconnect.
	Unreported(ctx context.Context) ([]Run, error)
	MarkReported(ctx context.Context, runID string, state protocol.RunState) error

	// SetLastAckedSeq persists the log resume point so it survives a restart
	// and not merely a reconnect.
	SetLastAckedSeq(ctx context.Context, runID string, seq uint64) error

	RecordArtifacts(ctx context.Context, runID string, refs []protocol.ArtifactRef) error
	Artifacts(ctx context.Context, runID string) ([]protocol.ArtifactRef, error)

	// NonTerminal returns runs left mid-flight by a crash, for the startup
	// reconciliation at RUNNER_SPEC §6 step 5.
	NonTerminal(ctx context.Context) ([]Run, error)

	Close() error
}

// NotImplemented satisfies [Store] by refusing every call.
type NotImplemented struct{}

var _ Store = NotImplemented{}

func unsupported(method string) error {
	return fmt.Errorf("store: %s: %w", method, errors.ErrUnsupported)
}

func (NotImplemented) Migrate(context.Context) error     { return unsupported("Migrate") }
func (NotImplemented) Create(context.Context, Run) error { return unsupported("Create") }

func (NotImplemented) Get(context.Context, string) (Run, error) {
	return Run{}, unsupported("Get")
}

func (NotImplemented) List(context.Context, Filter) ([]Run, error) {
	return nil, unsupported("List")
}

func (NotImplemented) Transition(context.Context, string, protocol.RunState, protocol.Update) error {
	return unsupported("Transition")
}

func (NotImplemented) Enqueue(context.Context, string, int) error { return unsupported("Enqueue") }

func (NotImplemented) ClaimNext(context.Context, []string) (Run, error) {
	return Run{}, unsupported("ClaimNext")
}

func (NotImplemented) Claim(context.Context, string) error { return unsupported("Claim") }

func (NotImplemented) SweepExpired(context.Context, time.Time) ([]Run, error) {
	return nil, unsupported("SweepExpired")
}

func (NotImplemented) Unreported(context.Context) ([]Run, error) {
	return nil, unsupported("Unreported")
}

func (NotImplemented) MarkReported(context.Context, string, protocol.RunState) error {
	return unsupported("MarkReported")
}

func (NotImplemented) SetLastAckedSeq(context.Context, string, uint64) error {
	return unsupported("SetLastAckedSeq")
}

func (NotImplemented) RecordArtifacts(context.Context, string, []protocol.ArtifactRef) error {
	return unsupported("RecordArtifacts")
}

func (NotImplemented) Artifacts(context.Context, string) ([]protocol.ArtifactRef, error) {
	return nil, unsupported("Artifacts")
}

func (NotImplemented) NonTerminal(context.Context) ([]Run, error) {
	return nil, unsupported("NonTerminal")
}

func (NotImplemented) Close() error { return nil }
