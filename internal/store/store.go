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
//
// Two amendments so far:
//
//   - A27 added [Store.Approve] and the two approval fields on [Run]. INV-5
//     required a local approval the interface gave no way to record, so the half
//     of the invariant that says "and then it runs" could not be implemented at
//     all — see [Store.Approve].
//   - A28 gave reporting state a transport dimension. [Store.Unreported],
//     [Store.MarkReported] and [Store.SetLastAckedSeq] each take a broker key,
//     [Store.LastAckedSeq] was added so a transport can read its own resume
//     point back, and [Run] lost ReportedState and LastAckedSeq because they
//     stopped being properties of a run. See [BrokerLocal].
package store

import (
	"context"
	"encoding/json"
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

	// Number is this run's position in the machine's own sequence, assigned by
	// [Store.Create] and monotonic across retention (amendment A36). It is the
	// agent's LATEM_BUILD_NUMBER, so a build in TestFlight names the run that
	// produced it.
	//
	// Assigned, not supplied: Create refuses a Run that arrives with one set,
	// because a caller choosing its own would be choosing a build number, and
	// two callers choosing the same one is the defect this exists to prevent.
	// A Run read back from the store carries it; the value a caller passed to
	// Create does not.
	Number int64

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

	// ApprovedAt and ApprovedSHA are what a local `latem approve` recorded
	// (amendment A27). ApprovedSHA is the commit the operator said yes to, not
	// the ref: a ref can point at a branch a contributor controls, so an
	// approval that named only the ref would be defeated by a force-push
	// between the approval and the build. Gate 8 admits an out-of-policy run
	// only while the freshly resolved commit still equals this one.
	ApprovedAt  *time.Time
	ApprovedSHA string

	// Workflow is set for a workflow run's job (A52), nil otherwise.
	Workflow *WorkflowRun
}

// WorkflowRunOf is the part of a run request [WorkflowRun] keeps, or nil for
// a run that is not a workflow run's job. The token is not part of it.
func WorkflowRunOf(req protocol.RunRequest) *WorkflowRun {
	if req.Workflow == "" {
		return nil
	}
	w := &WorkflowRun{
		Workflow:      req.Workflow,
		SHA:           req.SHA,
		Needs:         req.Needs,
		WorkflowRunID: req.WorkflowRunID,
		RunNumber:     req.RunNumber,
		RunAttempt:    req.RunAttempt,
		Matrix:        req.Matrix,
	}
	if req.Event != nil {
		w.Event = *req.Event
	}
	return w
}

// WorkflowRun is what a workflow run's job carries beyond a run (A52), kept
// with the run so an agent that restarts with it queued still knows what to
// build. Not the job's token: that is held in memory only — see
// [DB.HoldToken].
type WorkflowRun struct {
	Workflow      string                         `json:"workflow"`
	SHA           string                         `json:"sha"`
	Event         protocol.Event                 `json:"event"`
	Needs         map[string]protocol.NeedResult `json:"needs,omitempty"`
	WorkflowRunID string                         `json:"workflow_run_id"`
	RunNumber     int64                          `json:"run_number,omitempty"`
	RunAttempt    int64                          `json:"run_attempt,omitempty"`
	// Matrix is a matrix leg's values (A55).
	Matrix json.RawMessage `json:"matrix,omitempty"`
}

// The broker keys of the broker_state table (amendment A28).
//
// Each transport passes its own constant to [Store.MarkReported],
// [Store.SetLastAckedSeq], [Store.LastAckedSeq] and [Store.Unreported], and
// nothing else ever chooses one: "what has this transport acknowledged" is a
// question only that transport can answer, so it is the only thing that writes
// the answer down.
//
// They live here rather than in the broker packages because the table is this
// package's, and because the two constants sitting side by side is what makes
// the shape of the key obvious to whoever adds a third.
const (
	BrokerLocal  = "local"
	BrokerRemote = "remote"
)

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

	// HoldToken and Token keep a workflow job's GitHub token in memory, never
	// on disk, until the run is terminal (A52).
	HoldToken(runID, token string)
	Token(runID string) string
	// HoldFetchToken and FetchToken do the same for its fetches' token (A72).
	HoldFetchToken(runID, token string)
	FetchToken(runID string) string

	Create(ctx context.Context, r Run) error
	Get(ctx context.Context, runID string) (Run, error)
	List(ctx context.Context, f Filter) ([]Run, error)

	// Transition persists a state change and any fields that accompany it. It
	// MUST reject a transition the state machine does not allow, so an
	// ordering bug surfaces here rather than as a nonsensical run history.
	//
	// Entering awaiting_approval clears any approval on the row. Parking a run
	// means a human has to look at it again, and an approval that survived the
	// park would be an approval of a commit nobody is being shown.
	Transition(ctx context.Context, runID string, to protocol.RunState, u protocol.Update) error

	// Approve records a local approval and moves the run awaiting_approval →
	// queued, atomically, returning the row as it now stands (amendment A27).
	//
	// It is one method rather than the Get/Transition/Enqueue sequence it
	// replaces because the three steps must not be separable. The commit being
	// pinned is read and written inside the same transaction, so no force-push
	// can land between "which commit is this?" and "that commit is approved" —
	// which is the exact race INV-5's approval exists to close.
	//
	// The run must be in awaiting_approval and must already have a resolved
	// sha: approval names a commit, and a row with no commit has nothing to
	// pin. Approving a run that is already queued is a no-op success, because
	// approving twice is something a person does.
	//
	// approvedAt is stamped on the row; expiresAt is the queue_ttl deadline
	// that replaces the approval_ttl one — an approved run still carrying its
	// approval deadline would be swept as an expired queue entry minutes after
	// a human said yes to it. Both are passed in rather than derived here
	// because the clock and the configured TTL belong to the caller.
	//
	// It returns [ErrNotFound], [ErrNotAwaitingApproval], or [ErrNoCommit].
	Approve(ctx context.Context, runID string, approvedAt, expiresAt time.Time) (Run, error)

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

	// Unreported returns the runs whose state the named broker has not
	// acknowledged, for replay after a reconnect.
	//
	// The broker key is amendment A28 and it is the whole point of the method.
	// Draft 02 asked one column "has this been reported?", which two transports
	// answer differently: the local one marks a transition reported the moment
	// it is persisted, so a remote transport sharing that column would find
	// nothing to replay and a run that finished during an outage would never be
	// sent at all.
	Unreported(ctx context.Context, brk string) ([]Run, error)

	// MarkReported records that one broker acknowledged one state.
	MarkReported(ctx context.Context, brk, runID string, state protocol.RunState) error

	// SetLastAckedSeq persists one broker's log resume point so it survives a
	// restart and not merely a reconnect (finding F6). The write is monotonic.
	SetLastAckedSeq(ctx context.Context, brk, runID string, seq uint64) error

	// LastAckedSeq reads a broker's resume point back, returning 0 for a run
	// that transport has never streamed.
	//
	// Added by A28 alongside the key. While the resume point was a column of
	// runs, every caller already had it on the [Run] it was holding; keyed by
	// transport it needs a read of its own, and without one a transport
	// reopening a sink after a restart would begin at zero and re-send a
	// build's whole log.
	LastAckedSeq(ctx context.Context, brk, runID string) (uint64, error)

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

// HoldToken keeps nothing, and Token has nothing: there is no store to hold
// a token for a run in.
func (NotImplemented) HoldToken(string, string)      {}
func (NotImplemented) Token(string) string           { return "" }
func (NotImplemented) HoldFetchToken(string, string) {}
func (NotImplemented) FetchToken(string) string      { return "" }

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

func (NotImplemented) Approve(context.Context, string, time.Time, time.Time) (Run, error) {
	return Run{}, unsupported("Approve")
}

func (NotImplemented) Enqueue(context.Context, string, int) error { return unsupported("Enqueue") }

func (NotImplemented) ClaimNext(context.Context, []string) (Run, error) {
	return Run{}, unsupported("ClaimNext")
}

func (NotImplemented) Claim(context.Context, string) error { return unsupported("Claim") }

func (NotImplemented) SweepExpired(context.Context, time.Time) ([]Run, error) {
	return nil, unsupported("SweepExpired")
}

func (NotImplemented) Unreported(context.Context, string) ([]Run, error) {
	return nil, unsupported("Unreported")
}

func (NotImplemented) MarkReported(context.Context, string, string, protocol.RunState) error {
	return unsupported("MarkReported")
}

func (NotImplemented) SetLastAckedSeq(context.Context, string, string, uint64) error {
	return unsupported("SetLastAckedSeq")
}

func (NotImplemented) LastAckedSeq(context.Context, string, string) (uint64, error) {
	return 0, unsupported("LastAckedSeq")
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
