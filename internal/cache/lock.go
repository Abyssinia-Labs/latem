package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// lockPoll is how often a waiter retries a non-blocking flock. Cache locks are
// held for the length of a build, so the retry rate is irrelevant to
// throughput and a coarse interval keeps a waiting run off the CPU.
const lockPoll = 50 * time.Millisecond

// lockSet holds the per-cache locks of one agent process.
//
// Two layers, because they defend against different things. flock excludes
// other processes — a second agent, or an operator running `latem cache prune`
// by hand — and survives a crash, since the kernel drops it when the process
// dies. The in-process gate excludes goroutines within this agent, which is
// the concurrency > 1 case RUNNER_SPEC §14.3 is actually about, and makes the
// timeout deterministic instead of dependent on flock's retry loop.
type lockSet struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
	held  map[string]*os.File
}

func newLockSet() *lockSet {
	return &lockSet{gates: map[string]chan struct{}{}, held: map[string]*os.File{}}
}

func (s *lockSet) gate(path string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[path]
	if !ok {
		g = make(chan struct{}, 1)
		s.gates[path] = g
	}
	return g
}

// acquire takes the lock at path, waiting at most timeout.
//
// It returns [ErrLockTimeout] rather than blocking forever. A run that stops
// after ten minutes and says which cache it was waiting for is diagnosable; a
// run that sits in `preparing` until someone notices is not.
func (s *lockSet) acquire(ctx context.Context, path string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	gate := s.gate(path)
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%w: %s (%s)", ErrLockTimeout, path, timeout)
	}

	f, err := s.flock(ctx, path, timer)
	if err != nil {
		<-gate
		return err
	}

	s.mu.Lock()
	s.held[path] = f
	s.mu.Unlock()
	return nil
}

// flock opens the lock file and polls for the kernel lock until timer fires.
func (s *lockSet) flock(ctx context.Context, path string, timer *time.Timer) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cache: creating lock directory for %s: %w", path, err)
	}
	// #nosec G304 -- path is built by paths.lockPath from a runner and cache
	// name already checked against namePattern, which admits no separator and
	// no dot-leading name. Nothing here originates outside the machine.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cache: opening lock %s: %w", path, err)
	}

	for {
		ok, err := tryFlock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cache: locking %s: %w", path, err)
		}
		if ok {
			return f, nil
		}

		poll := time.NewTimer(lockPoll)
		select {
		case <-poll.C:
		case <-ctx.Done():
			poll.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
			poll.Stop()
			_ = f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, path)
		}
	}
}

// release drops the lock at path. Releasing a lock this process does not hold
// is a no-op, so [Manager.Release] is safe after a failed Acquire and safe to
// call twice — which is what a deferred release in a run that also releases on
// its happy path will do.
func (s *lockSet) release(path string) error {
	s.mu.Lock()
	f, ok := s.held[path]
	if ok {
		delete(s.held, path)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}

	err := unflock(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// Free the gate last: a waiter that wakes before the flock is dropped
	// would spin through its poll interval for nothing.
	<-s.gate(path)

	if err != nil {
		return fmt.Errorf("cache: releasing lock %s: %w", path, err)
	}
	return nil
}

// isHeld reports whether this process holds the lock at path. Prune uses it to
// leave a running build's cache alone.
func (s *lockSet) isHeld(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.held[path]
	return ok
}

// busy reports whether anyone — this process or another — holds the lock at
// path, without waiting for it.
//
// Prune consults this before evicting. Deleting the cache a build is halfway
// through reading produces exactly the intermittent, unattributable failure
// §14.3 exists to prevent, and doing it from the daily ticker would make it
// look like a flaky build rather than a bug in the pruner.
func (s *lockSet) busy(path string) bool {
	if s.isHeld(path) {
		return true
	}
	// #nosec G304 -- see flock: path comes from paths.lockPath over validated
	// names.
	f, err := os.OpenFile(path, os.O_RDONLY, 0o600)
	if err != nil {
		// No lock file means the cache has never been acquired, so nothing can
		// be holding it.
		return false
	}
	defer func() { _ = f.Close() }()

	ok, err := tryFlock(f)
	if err != nil {
		return true // cannot tell; the safe answer is "leave it alone"
	}
	if ok {
		_ = unflock(f)
		return false
	}
	return true
}
