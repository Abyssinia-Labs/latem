package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
// because approval touches exactly three statements, and because the caller is
// `latem approve` — a short-lived CLI process that opens the same sqlite file
// the agent holds in WAL mode, and should be able to prove from the type what
// it is allowed to do to it.
type ApprovalStore interface {
	Get(ctx context.Context, runID string) (store.Run, error)
	Transition(ctx context.Context, runID string, to protocol.RunState, u protocol.Update) error
	Enqueue(ctx context.Context, runID string, priority int) error
}

// ErrNotAwaitingApproval means the run was not parked for a human.
var ErrNotAwaitingApproval = errors.New("agent: run is not awaiting approval")

// Approve moves an awaiting_approval run back into the queue (§11, INV-5).
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
// The run lands in queued rather than claimed: §11 has no awaiting_approval →
// claimed edge, and approving a run says it may be built, not that it must be
// built next.
//
// Approving a run that is already queued succeeds. Approving twice is
// something a person does, and the second one should not read as an error.
func Approve(ctx context.Context, st ApprovalStore, runID string, queueTTL time.Duration, now time.Time) error {
	if err := protocol.ValidateID(runID); err != nil {
		return fmt.Errorf("agent: approve: %w", err)
	}
	r, err := st.Get(ctx, runID)
	if err != nil {
		return fmt.Errorf("agent: approve %s: %w", runID, err)
	}
	switch r.State {
	case protocol.StateQueued:
		return nil
	case protocol.StateAwaitingApproval:
	default:
		return fmt.Errorf("agent: approve %s: run is %s: %w", runID, r.State, ErrNotAwaitingApproval)
	}

	if queueTTL <= 0 {
		queueTTL = 72 * time.Hour
	}
	expires := now.Add(queueTTL)

	// The new deadline travels with the transition (amendment A8). It has to
	// be replaced rather than left alone: an approved run still carrying its
	// approval_ttl would be swept as an expired queue entry by the very next
	// ticker, minutes after a human said yes to it.
	if err := st.Transition(ctx, runID, protocol.StateQueued, protocol.Update{
		State:     protocol.StateQueued,
		ExpiresAt: &expires,
	}); err != nil {
		return fmt.Errorf("agent: approve %s: %w", runID, err)
	}
	if err := st.Enqueue(ctx, runID, 0); err != nil {
		return fmt.Errorf("agent: approve %s: enqueue: %w", runID, err)
	}
	return nil
}
