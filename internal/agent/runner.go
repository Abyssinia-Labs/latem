package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runnerState is one execution slot's live view: what it is, whether it can
// take work, and how much it is doing.
//
// It exists because [protocol.RunnerInfo] is a snapshot and a runner is not.
// Availability changes every five minutes (§20), the active count changes
// whenever a run starts or ends, and the heartbeat has to render both without
// holding a lock across a broker call.
type runnerState struct {
	cfg      *config.Runner
	ex       executor.Executor
	closer   io.Closer
	name     string
	executor string
	labels   []string
	limit    int

	mu        sync.Mutex
	available bool
	reason    string
	active    int
}

func newRunnerState(c *config.Runner, ex executor.Executor, buildErr error) *runnerState {
	limit := c.Concurrency
	if limit <= 0 {
		limit = config.DefaultConcurrency
	}
	r := &runnerState{
		cfg:      c,
		ex:       ex,
		name:     c.Name,
		executor: string(c.Executor),
		labels:   append([]string(nil), c.Labels...),
		limit:    limit,
	}
	if buildErr != nil {
		// A runner whose executor could not even be constructed is dark for
		// the life of the process, and saying so is worth more than refusing
		// to start: a Mac whose Docker socket is missing should still run
		// its Xcode jobs, and should still explain the dark runner (§20).
		r.reason = buildErr.Error()
	}
	if ex != nil {
		r.closer, _ = ex.(io.Closer)
	}
	return r
}

// usable reports whether this runner has an executor at all. A runner that
// does not is never offered to the orchestrator, so a job whose runs_on
// matches it is refused with E_NO_RUNNER rather than failing at Prepare.
func (r *runnerState) usable() bool { return r.ex != nil }

// setAvailable records a preflight result and reports whether it changed, so
// the caller logs a transition rather than the same line every five minutes.
func (r *runnerState) setAvailable(ok bool, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.available != ok || (!ok && r.reason != reason)
	r.available, r.reason = ok, reason
	if ok {
		r.reason = ""
	}
	return changed
}

func (r *runnerState) isAvailable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.available
}

func (r *runnerState) unavailableReason() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

func (r *runnerState) addActive(n int) {
	r.mu.Lock()
	r.active += n
	if r.active < 0 {
		r.active = 0
	}
	r.mu.Unlock()
}

func (r *runnerState) info() protocol.RunnerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return protocol.RunnerInfo{
		Name:        r.name,
		Executor:    r.executor,
		Labels:      append([]string(nil), r.labels...),
		Concurrency: r.limit,
		Active:      r.active,
		Available:   r.available,
		Unavailable: r.reason,
	}
}

// ErrRunnerUnavailable is returned by a gated executor whose runner failed
// preflight. It maps to E_EXECUTOR, which §21 marks retryable — correctly, in
// that a Docker daemon that came back is a machine condition that resolves
// itself.
var ErrRunnerUnavailable = errors.New("agent: runner is unavailable")

// slots enforces per-runner concurrency, and it lives here rather than in the
// pool because of where the decision is actually made.
//
// A worker cannot know which runner a queued run needs. Amendment A12 says why:
// a job's runs_on lives in jobs.yaml at the resolved commit, which is not on
// disk until after the claim, so store.ClaimNext deliberately does not match
// labels and gate 13 picks the runner deep inside Execute. By the time the
// answer exists the run is already claimed and already checked out, and the
// only remaining seam is [executor.Executor.Prepare].
//
// So the limit is taken there. A run whose runner is full waits — the same
// shape as gate 18's cache lock, which also blocks during preparing under the
// job's own wall clock — and gives up after [Options.SlotWait] rather than
// hanging. The alternative, letting the pool run more work on a runner than
// its concurrency allows, is not available on a Tart runner at all: two guest
// VMs is Apple's licence ceiling, not a tuning parameter (§13.2).
type slots struct {
	wait time.Duration

	mu    sync.Mutex
	free  map[string]int
	held  map[string]string // run id → runner name
	waits map[string]chan struct{}
}

func newSlots(runners []*runnerState, wait time.Duration) *slots {
	s := &slots{
		wait:  wait,
		free:  make(map[string]int, len(runners)),
		held:  map[string]string{},
		waits: make(map[string]chan struct{}, len(runners)),
	}
	for _, r := range runners {
		s.free[r.name] = r.limit
		s.waits[r.name] = make(chan struct{}, 1)
	}
	return s
}

// acquire takes a slot on runner for runID, waiting up to the configured
// budget. It is idempotent per run: a run already holding a slot on this
// runner takes no second one.
func (s *slots) acquire(ctx context.Context, runner, runID string) error {
	deadline := time.NewTimer(s.wait)
	defer deadline.Stop()

	for {
		s.mu.Lock()
		if s.held[runID] == runner {
			s.mu.Unlock()
			return nil
		}
		if n, ok := s.free[runner]; !ok {
			s.mu.Unlock()
			return fmt.Errorf("agent: no such runner %q", runner)
		} else if n > 0 {
			s.free[runner] = n - 1
			s.held[runID] = runner
			s.mu.Unlock()
			return nil
		}
		ch := s.waits[runner]
		s.mu.Unlock()

		select {
		case <-ch:
			// A slot was released; loop and try to take it. Losing the race to
			// another waiter is fine — the next release wakes this one again.
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("agent: waited %s for a slot on runner %q (concurrency %d)",
				s.wait, runner, s.capacity(runner))
		}
	}
}

// release returns the slot held by runID, if any. It is idempotent, and it is
// called from two places on purpose: the environment's Close, which is where
// the resource actually goes away, and the pool once Execute returns, which is
// the only place guaranteed to run even if an executor forgets.
func (s *slots) release(runID string) {
	s.mu.Lock()
	runner, ok := s.held[runID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.held, runID)
	s.free[runner]++
	ch := s.waits[runner]
	s.mu.Unlock()

	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *slots) capacity(runner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.free[runner]
}

// gatedExecutor wraps a runner's executor with the two things §20 requires of
// every runner and no executor implements for itself: never accept a run onto
// a runner that cannot execute it, and never exceed the runner's concurrency.
//
// Everything else is passed straight through. In particular Reap and Preflight
// are not gated, because reconciliation and recovery both have to work
// precisely when the runner is unavailable.
type gatedExecutor struct {
	inner executor.Executor
	state *runnerState
	slots *slots
}

var _ executor.Executor = (*gatedExecutor)(nil)

func (g *gatedExecutor) Name() string { return g.inner.Name() }

func (g *gatedExecutor) Preflight(ctx context.Context) error { return g.inner.Preflight(ctx) }

func (g *gatedExecutor) Reap(ctx context.Context, live func(string) bool) ([]executor.Reaped, error) {
	return g.inner.Reap(ctx, live)
}

func (g *gatedExecutor) Prepare(ctx context.Context, spec executor.Spec) (executor.Env, error) {
	if !g.state.isAvailable() {
		return nil, fmt.Errorf("%w: %s: %s", ErrRunnerUnavailable, g.state.name, g.state.unavailableReason())
	}
	if err := g.slots.acquire(ctx, g.state.name, spec.RunID); err != nil {
		return nil, err
	}

	env, err := g.inner.Prepare(ctx, spec)
	if err != nil {
		g.slots.release(spec.RunID)
		return nil, err
	}
	g.state.addActive(1)
	return &gatedEnv{Env: env, runID: spec.RunID, state: g.state, slots: g.slots}, nil
}

// gatedEnv releases the slot when the environment goes away.
type gatedEnv struct {
	executor.Env
	runID string
	state *runnerState
	slots *slots
	once  sync.Once
}

func (e *gatedEnv) Close(ctx context.Context) error {
	err := e.Env.Close(ctx)
	e.once.Do(func() {
		e.state.addActive(-1)
		e.slots.release(e.runID)
	})
	return err
}
