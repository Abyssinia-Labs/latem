package local

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// defaultOriginKind is what an origin with no kind becomes.
//
// origin_kind is NOT NULL in the §22 schema, and the only thing that reaches
// this socket is a local command line. Refusing a run because an audit-only
// display field was blank would be a worse answer than recording the truth,
// which is that it came from the CLI.
const defaultOriginKind = "cli"

// enqueueRun writes a run to the queue.
//
// This is the one implementation of "a run request becomes a queued run", and
// it has two callers on purpose: the socket handler, when an agent is up, and
// [Client.Submit], when there is no agent to hand it to (RUNNER_SPEC §7.3).
// Both paths therefore leave sqlite in exactly the same state, which is what
// makes the agent-down case a genuine deferral rather than a second, thinner
// kind of run that behaves differently when it finally executes.
//
// It is idempotent for a run that is already queued: a client that wrote the
// row itself and then found an agent after all, or that retried a notification,
// re-enqueues rather than failing. A run that already exists and has reached a
// terminal state is refused — re-queueing a finished run under its old id
// would make the run history lie about which build produced which artifact.
func enqueueRun(ctx context.Context, st Store, req protocol.RunRequest, priority int, expires time.Time) error {
	if err := protocol.ValidateID(req.ID); err != nil {
		return err
	}
	switch {
	case req.Repo == "":
		return errors.New("local: run request has no repo")
	case req.Job == "":
		return errors.New("local: run request has no job")
	case req.Ref == "":
		return errors.New("local: run request has no ref")
	}

	origin := req.Origin
	if origin.Kind == "" {
		origin.Kind = defaultOriginKind
	}

	// CreatedAt is left zero so the store stamps it from this machine's clock.
	// Origin.RequestedAt is a timestamp from wherever the request came from and
	// RUNNER_SPEC §7.1 is explicit that it must not order anything.
	run := store.Run{
		ID:          req.ID,
		Repo:        req.Repo,
		Job:         req.Job,
		Ref:         req.Ref,
		State:       protocol.StateQueued,
		OriginKind:  origin.Kind,
		OriginActor: origin.Actor,
		Inputs:      req.Inputs,
	}

	switch err := st.Create(ctx, run); {
	case err == nil:
	case errors.Is(err, store.ErrExists):
		existing, getErr := st.Get(ctx, req.ID)
		if getErr != nil {
			return getErr
		}
		if existing.State.IsTerminal() {
			return fmt.Errorf("local: run %s already finished as %s", req.ID, existing.State)
		}
	default:
		return err
	}

	if err := st.Enqueue(ctx, req.ID, priority); err != nil {
		return err
	}

	// §22: expires_at is set when a run enters queued. Only the caller knows
	// the configured queue_ttl, which is why it travels in rather than being
	// derived here — the same reason amendment A8 put ExpiresAt on the update
	// instead of inferring it.
	if !expires.IsZero() {
		if err := st.SetExpiry(ctx, req.ID, expires); err != nil {
			return err
		}
	}
	return nil
}
