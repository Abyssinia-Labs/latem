package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func newLocal(t *testing.T, root string, timeout time.Duration, opts ...func(*Options)) *Local {
	t.Helper()
	o := Options{
		Root:        root,
		LockTimeout: timeout,
		Runners: map[string]RunnerConfig{
			"linux":  {Executor: ExecutorNative},
			"docker": {Executor: ExecutorDocker},
		},
	}
	for _, f := range opts {
		f(&o)
	}
	l, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The acceptance criterion for WS-08: two concurrent runs on one runner cannot
// corrupt a shared cache. Two runs writing one ~/.gradle produce intermittent
// build failures nobody attributes to caching, so the exclusion has to be
// real, not documented.
func TestAcquire_SerialisesConcurrentRunsOnOneCache(t *testing.T) {
	l := newLocal(t, t.TempDir(), 10*time.Second)
	ctx := context.Background()

	var (
		mu      sync.Mutex
		inside  int
		highest int
		wg      sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Acquire(ctx, "linux", []string{"gradle"}); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			if inside > highest {
				highest = inside
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inside--
			mu.Unlock()
			if err := l.Release(ctx, "linux", []string{"gradle"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if highest != 1 {
		t.Errorf("%d runs held one cache at once; the flock is not excluding", highest)
	}
}

// The in-process gate is not enough on its own: `latem cache prune` run by
// hand, or a second agent started by accident, is a different process. flock
// binds to the open file description, so two managers over one root exclude
// each other exactly as two agents would.
func TestAcquire_ExcludesAnotherProcessHoldingTheSameCache(t *testing.T) {
	root := t.TempDir()
	first := newLocal(t, root, 10*time.Second)
	second := newLocal(t, root, 150*time.Millisecond)
	ctx := context.Background()

	if _, err := first.Acquire(ctx, "linux", []string{"gradle"}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := second.Acquire(ctx, "linux", []string{"gradle"})
	if err == nil {
		t.Fatal("a second holder acquired a cache the first still has")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v; the timeout did not bound the wait", elapsed)
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Errorf("err = %v, want ErrLockTimeout", err)
	}
	if CodeOf(err) != protocol.ErrCache {
		t.Errorf("CodeOf(err) = %q, want %s", CodeOf(err), protocol.ErrCache)
	}

	// And the lock is genuinely released, not merely dropped from a map.
	if err := first.Release(ctx, "linux", []string{"gradle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Acquire(ctx, "linux", []string{"gradle"}); err != nil {
		t.Fatalf("the cache stayed locked after Release: %v", err)
	}
}

// RUNNER_SPEC §14.3: the ten-minute timeout exists so a contended cache fails
// the run instead of hanging it. A run that stops and says which cache it was
// waiting for is diagnosable; one that sits in `preparing` forever is not.
func TestAcquire_TimesOutRatherThanHanging(t *testing.T) {
	root := t.TempDir()
	holder := newLocal(t, root, time.Second)
	waiter := newLocal(t, root, 50*time.Millisecond)
	ctx := context.Background()

	if _, err := holder.Acquire(ctx, "linux", []string{"pnpm"}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := waiter.Acquire(ctx, "linux", []string{"pnpm"})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Errorf("err = %v, want ErrLockTimeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire hung well past its timeout")
	}
}

func TestAcquire_HonoursContextCancellation(t *testing.T) {
	root := t.TempDir()
	holder := newLocal(t, root, time.Minute)
	waiter := newLocal(t, root, time.Minute)

	if _, err := holder.Acquire(context.Background(), "linux", []string{"pnpm"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := waiter.Acquire(ctx, "linux", []string{"pnpm"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's error — a cancelled run should not wait ten minutes", err)
	}
}

// Names are locked in sorted order so two runs asking for {gradle, pnpm} and
// {pnpm, gradle} cannot deadlock. Without it both would wait the full timeout
// and both would fail, and only on a busy machine.
func TestAcquire_LocksInSortedOrder(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)

	mounts, err := l.Acquire(context.Background(), "linux", []string{"pnpm", "gradle", "cocoapods"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cocoapods", "gradle", "pnpm"}
	for i, m := range mounts {
		if m.Name != want[i] {
			t.Fatalf("mounts = %+v, want them locked in sorted order %v", mounts, want)
		}
	}
}

func TestAcquire_DeduplicatesNames(t *testing.T) {
	l := newLocal(t, t.TempDir(), 200*time.Millisecond)

	mounts, err := l.Acquire(context.Background(), "linux", []string{"pnpm", "pnpm"})
	if err != nil {
		t.Fatalf("a repeated cache name deadlocked against itself: %v", err)
	}
	if len(mounts) != 1 {
		t.Errorf("mounts = %+v, want one", mounts)
	}
}

// A partial acquisition is a leak: the caches taken before the failure would
// stay locked for the life of the agent, and every later run would time out on
// them.
func TestAcquire_UnwindsAfterAPartialFailure(t *testing.T) {
	root := t.TempDir()
	holder := newLocal(t, root, time.Second)
	runner := newLocal(t, root, 50*time.Millisecond)
	ctx := context.Background()

	// Hold the second cache in sorted order, so the first is taken and then
	// has to be given back.
	if _, err := holder.Acquire(ctx, "linux", []string{"zebra"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Acquire(ctx, "linux", []string{"alpha", "zebra"}); err == nil {
		t.Fatal("Acquire succeeded while zebra was held")
	}

	// alpha must be free again for anyone, including another process.
	third := newLocal(t, root, 200*time.Millisecond)
	if _, err := third.Acquire(ctx, "linux", []string{"alpha"}); err != nil {
		t.Errorf("alpha stayed locked after a partial acquisition failed: %v", err)
	}
}

func TestRelease_IsIdempotentAndSafeAfterFailure(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	ctx := context.Background()

	// Never acquired.
	if err := l.Release(ctx, "linux", []string{"pnpm"}); err != nil {
		t.Errorf("releasing a cache that was never acquired: %v", err)
	}
	if _, err := l.Acquire(ctx, "linux", []string{"pnpm"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx, "linux", []string{"pnpm"}); err != nil {
		t.Fatal(err)
	}
	// Twice: a run releasing on its happy path and again from a defer.
	if err := l.Release(ctx, "linux", []string{"pnpm"}); err != nil {
		t.Errorf("second Release: %v", err)
	}
	// Invalid names cannot have been locked, so they are not an error either.
	if err := l.Release(ctx, "../evil", []string{"..", "PNPM"}); err != nil {
		t.Errorf("Release rejected names nothing could have locked: %v", err)
	}
}

func TestAcquire_RejectsUnsafeNames(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	ctx := context.Background()

	tests := []struct {
		name   string
		runner string
		cache  string
	}{
		{"parent directory as a cache", "linux", ".."},
		{"parent directory as a runner", "..", "pnpm"},
		{"separator in the cache name", "linux", "a/b"},
		{"separator in the runner name", "a/b", "pnpm"},
		{"absolute path", "linux", "/etc/passwd"},
		{"leading dot", "linux", ".hidden"},
		{"leading dash reads as a flag", "linux", "-rf"},
		{"uppercase is not a legal volume name", "linux", "Gradle"},
		{"empty cache name", "linux", ""},
		{"empty runner name", "", "pnpm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := l.Acquire(ctx, tc.runner, []string{tc.cache}); err == nil {
				t.Fatalf("Acquire(%q, %q) was accepted; these become volume names and paths",
					tc.runner, tc.cache)
			} else if CodeOf(err) != protocol.ErrCache {
				t.Errorf("CodeOf(err) = %q, want %s", CodeOf(err), protocol.ErrCache)
			}
		})
	}
}

func TestAcquire_NoNamesIsANoOp(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	mounts, err := l.Acquire(context.Background(), "linux", nil)
	if err != nil || mounts != nil {
		t.Errorf("Acquire(nil) = %v, %v", mounts, err)
	}
}
