package run

import (
	"context"
	"fmt"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Admit runs gates 1-5 of RUNNER_SPEC §10 and files the run.
//
// These are the gates decidable with no network, no filesystem, and no
// process: the repo allowlist, the ref regex, input shape, the conservative
// ref-policy pre-check, and the weakest disk floor on the machine. A hostile
// request costs a map lookup, a regexp, and one row.
//
// Two outcomes, both of which create a row:
//
//   - queued, with a queue_ttl deadline;
//   - rejected, terminal, carrying the §21 code the gate produced.
//
// It no longer parks anything (amendment A27). Gate 4's approval branch was the
// third outcome, and it created a run in awaiting_approval that carried no
// commit — because nothing has been fetched at this point and the ref has not
// been resolved. `latem approve` binds an approval to a commit, so such a run
// could never be approved at all: the invariant's second half was unreachable
// from its first. A ref outside allowed_refs is now queued when the repo
// permits approval, and gate 8 parks it once gate 9 knows what it would build.
// Where no approval is possible, gate 4 still rejects for free.
//
// Only a queued run is enqueued. The row exists in both cases because a
// rejection nobody can see is a support question, and because §22's history is
// what makes an audit answerable.
func (o *Orchestrator) Admit(ctx context.Context, req protocol.RunRequest, priority int) (store.Run, error) {
	if err := protocol.ValidateID(req.ID); err != nil {
		// Defence in depth: protocol.Command.Validate has already done this at
		// the trust boundary, and every path below builds a filesystem name
		// out of this value (amendment A6).
		return store.Run{}, fmt.Errorf("run: admit: %w", err)
	}

	decision := registry.PreAdmit(req, o.policy(ctx))

	now := o.now()
	r := store.Run{
		ID:          req.ID,
		Repo:        req.Repo,
		Job:         req.Job,
		Ref:         req.Ref,
		State:       protocol.StateQueued,
		OriginKind:  req.Origin.Kind,
		OriginActor: req.Origin.Actor,
		Inputs:      req.Inputs,
		CreatedAt:   now,
	}

	if decision.OK() {
		r.ExpiresAt = ptr(now.Add(o.queueTTL()))
	} else {
		// Every decision gates 1-5 can now return is terminal, so this branch
		// stamps finished_at unconditionally. [registry.PreAdmit] documents
		// that it never returns awaiting_approval.
		r.State = decision.State
		r.ErrorCode = decision.Code
		r.ErrorMsg = truncate(decision.Msg, MaxErrorMsgBytes)
		r.FinishedAt = ptr(now)
	}

	if err := o.store.Create(ctx, r); err != nil {
		return store.Run{}, fmt.Errorf("run: admit %s: %w", req.ID, err)
	}
	if r.State == protocol.StateQueued {
		if err := o.store.Enqueue(ctx, r.ID, priority); err != nil {
			return store.Run{}, fmt.Errorf("run: admit %s: enqueue: %w", req.ID, err)
		}
	}

	o.reporter.enqueue(r.ID, protocol.Update{
		State:      r.State,
		ErrorCode:  r.ErrorCode,
		ErrorMsg:   r.ErrorMsg,
		FinishedAt: r.FinishedAt,
		ExpiresAt:  r.ExpiresAt,
	})
	if r.State.IsTerminal() {
		o.reporter.forget(r.ID)
	}
	return r, nil
}

// requestOf reconstructs the request a run was created from.
//
// The row is the source of truth rather than whatever the caller is still
// holding: an agent that restarted mid-queue has the row and nothing else, and
// two code paths differing in where the job name came from is how INV-2 gets
// eroded one convenience at a time.
func requestOf(r store.Run) protocol.RunRequest {
	return protocol.RunRequest{
		ID:     r.ID,
		Repo:   r.Repo,
		Job:    r.Job,
		Ref:    r.Ref,
		Inputs: r.Inputs,
		Origin: protocol.Origin{Kind: r.OriginKind, Actor: r.OriginActor},
	}
}

func ptr[T any](v T) *T { return &v }
