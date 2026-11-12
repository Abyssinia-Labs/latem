package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// RunIDPrefix begins every run id this machine generates.
//
// It is short, lower case, and carries no meaning beyond "this is a run",
// because the id becomes work/<id>, logs/<id>.log, artifacts/<id>/, and the
// container name latem-run-<id>. Anything encoded in it — a date, a repo, a
// job — would be a second source of truth for something the run row already
// answers, and would widen the character class INV-3 deliberately narrows.
const RunIDPrefix = "r_"

// NewRunID mints an id satisfying [protocol.ValidateID].
//
// Random rather than sequential. A sequential id would need a counter that
// survives a restart and is shared with every other `latem run` on the machine,
// and the only thing it would buy is guessability — which for a value that
// names a directory and a container is a cost rather than a feature.
func NewRunID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: generating a run id: %w", err)
	}
	id := RunIDPrefix + hex.EncodeToString(b[:])
	if err := protocol.ValidateID(id); err != nil {
		return "", fmt.Errorf("agent: generated an invalid run id %q: %w", id, err)
	}
	return id, nil
}

// ApprovalStore is what [Approve] needs. It is narrower than [store.Store]
// because approval is one write, and because the caller is `latem approve` — a
// short-lived CLI process that opens the same sqlite file the agent holds in
// WAL mode, and should be able to prove from the type what it is allowed to do
// to it.
//
// It was three methods (Get, Transition, Enqueue) until amendment A27 made the
// store do all of it in one transaction. The narrowing is the point: a CLI that
// could read a commit and then write it back as approved would have a window
// between the two, and that window is the one a force-push exploits.
//
// Since A89 `latem approve` shows a person the commit and has them type it
// before it approves, so the CLI does read the commit first. The window is
// closed the other way round: the store is told which commit the person typed,
// and pins that one or nothing, in the same transaction.
type ApprovalStore interface {
	ApproveCommit(ctx context.Context, runID, sha string, approvedAt, expiresAt time.Time) (store.Run, error)
}

// ErrNotAwaitingApproval means the run was not parked for a human.
//
// It aliases the store's sentinel rather than wrapping it, so a caller that
// knows only this package and a caller that knows only the store both match the
// same error.
var ErrNotAwaitingApproval = store.ErrNotAwaitingApproval

// ErrNoCommit means the run has no resolved commit for the approval to pin.
var ErrNoCommit = store.ErrNoCommit

// ErrCommitChanged means the run is held at another commit than the one the
// approval names, and nothing was approved (A89).
var ErrCommitChanged = store.ErrCommitChanged

// Approve records a local approval and moves the run back into the queue
// (§11, INV-5).
//
// # Why this is a store write and not a command
//
// Approval is a local action only. INV-2 closes the inbound command set at run
// and cancel, and a third kind is a security review rather than a feature — so
// there is deliberately no wire verb a broker could send, and therefore none
// `latem approve` can send either. The CLI reaches the same sqlite file the
// agent has open in WAL mode and performs the transition itself; the agent's
// pool finds the run on its next poll, which is the same path a run enqueued
// while the agent was down takes (§7.3).
//
// # What is being approved
//
// A commit, not a ref (amendment A27). The run row carries the commit gate 9
// resolved before gate 8 parked it, and approval pins exactly that one: gate 8
// admits the run on the next attempt only while the ref still resolves to it.
// Approving the ref instead would mean approving whatever the branch points at
// by the time a runner picks the run up, and INV-5 exists precisely because
// that is a value a contributor controls.
//
// The run lands in queued rather than claimed: §11 has no awaiting_approval →
// claimed edge, and approving a run says it may be built, not that it must be
// built next.
//
// Approving a run that is already queued succeeds. Approving twice is
// something a person does, and the second one should not read as an error.
//
// sha is the commit the person approving was shown and typed (A89). A run held
// at any other is [ErrCommitChanged] and stays held: the commit on the row can
// change only if the run was approved by someone else, built, and held again
// with its ref moved, and nobody has looked at that one.
//
// It returns the run as it now stands, so the caller can say which commit it
// just approved.
func Approve(
	ctx context.Context, st ApprovalStore, runID, sha string, queueTTL time.Duration, now time.Time,
) (store.Run, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return store.Run{}, fmt.Errorf("agent: approve: %w", err)
	}
	if queueTTL <= 0 {
		queueTTL = 72 * time.Hour
	}

	// The queue deadline replaces the approval one rather than being left
	// alone (amendment A8): an approved run still carrying its approval_ttl
	// would be swept as an expired queue entry by the very next ticker,
	// minutes after a human said yes to it.
	r, err := st.ApproveCommit(ctx, runID, sha, now, now.Add(queueTTL))
	if err != nil {
		return store.Run{}, fmt.Errorf("agent: approve %s: %w", runID, err)
	}
	return r, nil
}
