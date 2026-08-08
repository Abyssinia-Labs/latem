package agent

import (
	"context"
	"fmt"
	"time"
)

// preflightLoop is §20's rolling check: every five minutes, per executor.
//
// The interval is the point. A runner that fails is marked unavailable and
// stops accepting work; recovery is automatic when the next check passes. That
// turns "the Docker daemon restarted" from a build that fails forty minutes in
// with a message about a socket into a runner that is briefly dark and then is
// not — and it is the difference between a machine an operator trusts and one
// they check on.
func (a *Agent) preflightLoop(ctx context.Context) {
	t := time.NewTicker(a.preflightEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.preflightAll(ctx)
			// A runner that just came back has work waiting for it, and the
			// next doorbell may be hours away.
			a.ring()
		}
	}
}

// preflightAll checks every runner and records the result.
func (a *Agent) preflightAll(ctx context.Context) {
	for _, r := range a.runners {
		a.preflightOne(ctx, r)
	}
	a.recordRunners()
}

// preflightOne is one runner's availability answer.
//
// Two questions, in this order, and the order is not arbitrary: an executor
// that cannot run anything makes the disk floor moot, while a disk below the
// floor makes a perfectly healthy executor the wrong place to start a build.
// §14.4 is explicit that the floor refuses new runs rather than failing them
// halfway through, and preflight is the only place that refusal costs nothing.
func (a *Agent) preflightOne(ctx context.Context, r *runnerState) {
	if !r.usable() {
		// No executor at all: the reason was fixed at construction and no
		// probe will change it.
		return
	}

	pctx, cancel := context.WithTimeout(ctx, a.preflightTimeout)
	defer cancel()

	err := r.ex.Preflight(pctx)
	if err == nil {
		err = a.checkFloor(pctx, r)
	}

	ok := err == nil
	reason := ""
	if !ok {
		reason = err.Error()
	}
	if r.setAvailable(ok, reason) {
		if ok {
			a.log.Info("runner available", "runner", r.name, "executor", r.executor)
		} else {
			a.log.Warn("runner unavailable", "runner", r.name, "executor", r.executor, "reason", reason)
		}
	}
}

// checkFloor is §14.4's per-runner disk floor, asked before a run rather than
// during one.
//
// A cache manager that cannot answer drops the check rather than failing it,
// for the same reason gate 5 does: this exists to guard against a full disk,
// not against an unconfigured one, and gate 17 asks again with a chosen runner
// before anything is executed.
func (a *Agent) checkFloor(ctx context.Context, r *runnerState) error {
	if a.caches == nil {
		return nil
	}
	free, err := a.caches.FreeGB(ctx)
	if err != nil {
		a.log.Debug("could not measure free disk; skipping the floor check",
			"runner", r.name, "err", err)
		return nil
	}
	floor := a.caches.MinFreeGB(r.name)
	if free < floor {
		return fmt.Errorf("%.1f GB free on %s, runner %q needs %.1f GB (RUNNER_SPEC §14.4)",
			free, a.cfg.Paths.Work, r.name, floor)
	}
	return nil
}

// availableRunners counts the runners that would accept work right now. The
// pool consults it before claiming: taking a run off the queue onto a machine
// where nothing can execute it would move it from queued — where it waits for
// the daemon to come back — to failed.
func (a *Agent) availableRunners() int {
	n := 0
	for _, r := range a.runners {
		if r.usable() && r.isAvailable() {
			n++
		}
	}
	return n
}

// runnerNames lists the slots for store.ClaimNext.
//
// Every configured runner is named, available or not, because the names are a
// pinning filter and not a capability claim: ClaimNext takes runs that are
// unpinned or pinned to one of these (amendment A12), and withholding an
// unavailable runner's name would strand a run gate 16 had already pinned to
// it rather than leaving it queued for the daemon's return.
func (a *Agent) runnerNames() []string {
	out := make([]string, 0, len(a.runners))
	for _, r := range a.runners {
		out = append(out, r.name)
	}
	return out
}
