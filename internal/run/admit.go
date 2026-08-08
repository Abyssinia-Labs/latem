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
// Three outcomes, all of which create a row:
//
//   - queued, with a queue_ttl deadline;
//   - awaiting_approval, with an approval_ttl deadline, when the ref is
//     outside allowed_refs and the repo permits approval (INV-5). A broker
//     cannot grant this; only `latem approve` can;
//   - rejected, terminal, carrying the §21 code the gate produced.
//
// Only a queued run is enqueued. The row exists in every case because a
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
	_, repoCfg, _ := o.repoPolicy(req.Repo)

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

	switch {
	case decision.OK():
		r.ExpiresAt = ptr(now.Add(o.queueTTL()))
	case decision.State == protocol.StateAwaitingApproval:
		r.State = protocol.StateAwaitingApproval
		r.ErrorCode = decision.Code
		r.ErrorMsg = truncate(decision.Msg, MaxErrorMsgBytes)
		r.ExpiresAt = ptr(now.Add(o.approvalTTL(repoCfg)))
	default:
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
