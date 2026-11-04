package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// defaultOriginKind is what an inbound run with no origin kind becomes.
//
// origin_kind is NOT NULL in the §22 schema and Origin is audit-only and
// untrusted (§7.1), so a blank one is recorded rather than refused: rejecting a
// build because a display field was empty would be the wrong trade, and
// "manual" is the honest description of a run a control plane sent without
// saying why.
const defaultOriginKind = "manual"

// enqueueRun makes an inbound run durable before it is acknowledged.
//
// This is the same act internal/broker/local performs on its socket, for the
// same reason: §7.3 makes sqlite the truth and the transport a notification, so
// a run that has been accepted survives the agent restarting one second later.
// The control plane is told "accepted" only once that is true.
//
// It is idempotent by run id, which is what makes the server's post-resume
// replay safe (§7.4): a run already queued is re-queued rather than refused,
// and a run that has already reached a terminal state is refused rather than
// resurrected — re-queueing a finished run under its old id would make the run
// history lie about which build produced which artifact.
//
// # Duplication, recorded rather than hidden
//
// internal/broker/local has this function, unexported, and this is a second
// copy of it. The shared version has nowhere to live: internal/broker holds the
// frozen interface and nothing else (finding F8), and neither transport may
// import the other. §27 already records the same shape of debt between the
// docker and native executors. The difference in behaviour is deliberate and
// small — the default origin kind, because a run arriving here did not come
// from a command line — and it is the whole difference.
func enqueueRun(ctx context.Context, st Store, req protocol.RunRequest, priority int, expires time.Time) error {
	if err := protocol.ValidateID(req.ID); err != nil {
		return err
	}
	switch {
	case req.Repo == "":
		return errors.New("remote: run request has no repo")
	case req.Job == "":
		return errors.New("remote: run request has no job")
	case req.Ref == "":
		return errors.New("remote: run request has no ref")
	}

	origin := req.Origin
	if origin.Kind == "" {
		origin.Kind = defaultOriginKind
	}

	// CreatedAt is left zero so the store stamps it from this machine's clock.
	// Origin.RequestedAt came from another machine and §7.1 is explicit that it
	// must not order, expire, or decide anything.
	run := store.Run{
		ID:          req.ID,
		Repo:        req.Repo,
		Job:         req.Job,
		Ref:         req.Ref,
		State:       protocol.StateQueued,
		OriginKind:  origin.Kind,
		OriginActor: origin.Actor,
		Inputs:      req.Inputs,
		Workflow:    store.WorkflowRunOf(req),
	}

	switch err := st.Create(ctx, run); {
	case err == nil:
	case errors.Is(err, store.ErrExists):
		existing, getErr := st.Get(ctx, req.ID)
		if getErr != nil {
			return getErr
		}
		if existing.State.IsTerminal() {
			return fmt.Errorf("remote: run %s already finished as %s", req.ID, existing.State)
		}
		if existing.State != protocol.StateQueued {
			// Sent again for its tokens (A75): a run a runner has already
			// claimed takes them and stays where it is. Queueing it again
			// would put a claimed run back in the queue.
			st.HoldToken(req.ID, req.Token)
			st.HoldFetchToken(req.ID, req.FetchToken)
			return nil
		}
	default:
		return err
	}

	// The token is held before the run is queued, so no runner can claim it
	// first and start without one. A replayed request may carry a fresh one.
	st.HoldToken(req.ID, req.Token)
	st.HoldFetchToken(req.ID, req.FetchToken)

	if err := st.Enqueue(ctx, req.ID, priority); err != nil {
		return err
	}

	// §22: expires_at is set when a run enters queued. The TTL is this machine's
	// configuration and travels in rather than arriving on the wire, because
	// INV-2 keeps configuration off the inbound path.
	if !expires.IsZero() {
		if err := st.SetExpiry(ctx, req.ID, expires); err != nil {
			return err
		}
	}
	return nil
}
