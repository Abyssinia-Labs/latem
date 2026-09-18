package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// pool is RUNNER_SPEC §6 step 6: the workers that take runs off the queue and
// drive them through internal/run.
//
// # Why it polls as well as listens
//
// [local.Broker.Commands] does not replay the backlog, and that is deliberate
// rather than a gap: sqlite is the truth and the socket is a doorbell. Three
// ordinary things produce a claimable run with no notification behind it —
// a run enqueued while the agent was down (§7.3), an approval (`latem
// approve`), and a run gate 16 sent back to the queue because its concurrency
// group was busy. A pool that only reacted to Commands would sit idle beside a
// full queue in all three cases, which is the single most expensive mistake
// available in this file.
//
// So every worker waits on three things: the doorbell, a short ticker, and its
// own shutdown. The doorbell makes a locally submitted run start immediately;
// the ticker makes every other path work at all.
//
// # Why the workers are not per runner
//
// A worker cannot know which runner a queued run needs. Amendment A12 is
// explicit: a job's runs_on lives in jobs.yaml at the resolved commit, which is
// not on disk until after the claim, so store.ClaimNext matches pinning and not
// labels, and gate 13 chooses the runner deep inside Execute. The pool is
// therefore sized to the sum of the runners' concurrencies and the per-runner
// limit is enforced where the answer exists — see [slots].
type pool struct {
	a       *Agent
	workers int

	mu     sync.Mutex
	active map[string]time.Time

	accepting bool
	finished  chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// startPool launches the workers and returns the pool.
func (a *Agent) startPool(ctx context.Context) *pool {
	workers := 0
	for _, r := range a.runners {
		if r.usable() {
			workers += r.limit
		}
	}
	if workers < 1 {
		workers = 1
	}
	if a.opts.Once {
		// --once drives one run from [Agent.once] and nothing else. Leaving
		// the workers running would let them drain the rest of the queue
		// behind the operator's back, which is the opposite of what a flag
		// called "execute a single queued run and exit" promises.
		workers = 0
	}

	p := &pool{
		a:         a,
		workers:   workers,
		active:    map[string]time.Time{},
		accepting: true,
		finished:  make(chan struct{}),
	}
	for range workers {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.work(ctx)
		}()
	}
	a.log.Info("runner pool started", "workers", workers)
	return p
}

// work is one worker's life: wait for a reason to look, then drain the queue
// until it is empty or the worker is no longer wanted.
func (p *pool) work(ctx context.Context) {
	t := time.NewTicker(p.a.claimEvery)
	defer t.Stop()

	for {
		p.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.a.wake:
			// One doorbell wakes one worker. That is enough: a worker that
			// finds more than one claimable run drains them all, and a worker
			// that finds none costs a single query.
		case <-t.C:
		}
	}
}

// drain claims and executes until there is nothing claimable.
func (p *pool) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil || !p.accepts() {
			return
		}
		r, err := p.a.claimOne(ctx)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return
		case err != nil:
			if ctx.Err() == nil {
				p.a.log.Error("claiming from the queue", "err", err)
			}
			return
		}
		p.execute(ctx, r)
	}
}

// claimOne takes the next claimable run, or reports [store.ErrNotFound].
//
// It refuses to claim at all when no runner can execute anything. §20 says
// never accept a run onto a runner that cannot execute it, and the honest
// reading on a machine where every runner is dark is to leave the work queued:
// a claimed run whose Prepare then fails is a failed build, while a queued one
// simply starts when Docker comes back.
func (a *Agent) claimOne(ctx context.Context) (store.Run, error) {
	if a.availableRunners() == 0 {
		return store.Run{}, store.ErrNotFound
	}
	return a.db.ClaimNext(ctx, a.runnerNames())
}

// execute drives one run to a terminal state and records what happened.
func (p *pool) execute(ctx context.Context, r store.Run) {
	started := p.a.now()
	p.begin(r)
	defer p.end(r.ID)

	p.a.log.Info("run claimed", "run_id", r.ID, "repo", r.Repo, "job", r.Job, "ref", r.Ref)

	res, err := p.a.orch.Execute(ctx, r)
	if err != nil {
		// Execute returns an error only when nothing ran at all: a run that is
		// not claimed, one already executing here, a store that refused the
		// first transition. Everything a build can do to itself, including
		// failing, comes back as a Result.
		p.a.log.Error("run did not start", "run_id", r.ID, "err", err)
		return
	}

	switch {
	case res.Requeued:
		p.a.log.Info("run returned to the queue", "run_id", r.ID, "runner", res.Runner)
		// Something else holds its concurrency group. Ringing the doorbell now
		// would spin; the ticker picks it up when the group is free.
	case res.State.IsTerminal():
		p.a.log.Info("run finished",
			"run_id", r.ID, "state", res.State, "runner", res.Runner,
			"error_code", res.ErrorCode, "took", p.a.now().Sub(started).Round(time.Second))
	default:
		p.a.log.Info("run parked", "run_id", r.ID, "state", res.State)
	}
	p.a.recordResult(res, p.a.now().Sub(started))
}

// begin registers a run as this pool's, and remembers its repo so the cache
// pruner can tell a busy mirror from an idle one.
func (p *pool) begin(r store.Run) {
	p.mu.Lock()
	p.active[r.ID] = p.a.now()
	p.mu.Unlock()
	p.a.noteActive(r.ID, r.Repo)
}

// end releases everything the run held.
//
// The slot is released here as well as in the environment's Close, and the
// duplication is the point: Close is where the resource actually goes away,
// and this is the only place guaranteed to run whatever an executor does. Both
// are idempotent.
func (p *pool) end(runID string) {
	p.a.slots.release(runID)
	p.a.forgetActive(runID)

	p.mu.Lock()
	delete(p.active, runID)
	empty := len(p.active) == 0 && !p.accepting
	p.mu.Unlock()

	if empty {
		p.closeOnce.Do(func() { close(p.finished) })
	}
}

// accepts reports whether the pool may still claim.
func (p *pool) accepts() bool {
	// Two flags rather than one, and both are checked here because this is the
	// only place a run is taken on. draining is final; paused is self-update
	// holding the door for a few minutes (§20).
	if p.a.draining.Load() || p.a.paused.Load() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepting
}

// stopAccepting closes the queue side of the pool. In-flight runs continue —
// that is what shutdown_grace is for.
func (p *pool) stopAccepting() {
	p.mu.Lock()
	p.accepting = false
	empty := len(p.active) == 0
	p.mu.Unlock()
	if empty {
		p.closeOnce.Do(func() { close(p.finished) })
	}
}

// done closes once the pool has stopped accepting and every in-flight run has
// finished.
func (p *pool) done() <-chan struct{} { return p.finished }

func (p *pool) idle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active) == 0
}

func (p *pool) activeIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.active))
	for id := range p.active {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// cancelAll stops every in-flight run through the ordinary cancellation path,
// which is what §6 means by "send the run's process group SIGTERM, wait 30s,
// SIGKILL": internal/run owns that sequence and the executor performs it.
//
// Going through Cancel rather than cancelling the work context wholesale is
// deliberate. Cancel drives the run through collecting to cancelled, so a
// build stopped by a shutdown still yields its artifacts and its logs, and the
// run reads cancelled rather than being resurrected as interrupted by the next
// startup for a shutdown that was entirely orderly.
func (p *pool) cancelAll(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range p.activeIDs() {
		if err := p.a.orch.Cancel(ctx, protocol.CancelRequest{
			ID:     id,
			Actor:  "agent",
			Reason: reason,
		}); err != nil {
			p.a.log.Error("cancelling on shutdown", "run_id", id, "err", err)
		}
	}
}

func (p *pool) wait() { p.wg.Wait() }
