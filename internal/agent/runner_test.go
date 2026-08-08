package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
)

func testRunnerState(name string, concurrency int) *runnerState {
	r := newRunnerState(&config.Runner{
		Name:        name,
		Executor:    config.ExecutorDocker,
		Concurrency: concurrency,
	}, nil, nil)
	r.setAvailable(true, "")
	return r
}

// A slot limit that is not enforced is not a limit, and on a Tart runner it is
// Apple's licence ceiling rather than a tuning parameter: two guest VMs per
// host, full stop (§13.2). This pins that a third acquire waits.
func TestSlotsEnforceTheRunnerConcurrencyLimit(t *testing.T) {
	s := newSlots([]*runnerState{testRunnerState("tart", 2)}, 50*time.Millisecond)
	ctx := context.Background()

	for _, id := range []string{"r_a", "r_b"} {
		if err := s.acquire(ctx, "tart", id); err != nil {
			t.Fatalf("acquiring for %s: %v", id, err)
		}
	}

	err := s.acquire(ctx, "tart", "r_c")
	if err == nil {
		t.Fatal("a third run took a slot on a runner with concurrency 2")
	}

	// Releasing one lets the waiter through, so the limit is a queue and not a
	// wall: a run that waits for a slot is normal, and one that fails forever
	// after a busy morning is not.
	s.release("r_a")
	if err := s.acquire(ctx, "tart", "r_c"); err != nil {
		t.Fatalf("after a release, acquiring should succeed: %v", err)
	}
}

// A waiter must be woken by a release rather than time out beside a free slot.
func TestSlotsWakeAWaiterOnRelease(t *testing.T) {
	s := newSlots([]*runnerState{testRunnerState("one", 1)}, 5*time.Second)
	ctx := context.Background()

	if err := s.acquire(ctx, "one", "r_first"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	got := make(chan error, 1)
	go func() {
		defer wg.Done()
		got <- s.acquire(ctx, "one", "r_second")
	}()

	time.Sleep(50 * time.Millisecond)
	s.release("r_first")

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the waiter should have been woken: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a released slot did not wake the waiter")
	}
	wg.Wait()
}

// Acquire is idempotent per run, because the pool releases by run id as well
// as the environment doing it on Close. Double accounting would leak the slot
// on every run.
func TestSlotsAreIdempotentPerRun(t *testing.T) {
	s := newSlots([]*runnerState{testRunnerState("one", 1)}, 50*time.Millisecond)
	ctx := context.Background()

	for range 3 {
		if err := s.acquire(ctx, "one", "r_same"); err != nil {
			t.Fatalf("re-acquiring for the same run should be free: %v", err)
		}
	}
	s.release("r_same")
	s.release("r_same") // the pool's belt to the environment's braces

	if err := s.acquire(ctx, "one", "r_next"); err != nil {
		t.Fatalf("the slot was not returned: %v", err)
	}
}

func TestSlotsRejectAnUnknownRunner(t *testing.T) {
	s := newSlots([]*runnerState{testRunnerState("one", 1)}, time.Second)
	if err := s.acquire(context.Background(), "nope", "r_x"); err == nil {
		t.Error("acquiring on a runner that does not exist should fail loudly")
	}
}

func TestSlotAcquireHonoursContextCancellation(t *testing.T) {
	s := newSlots([]*runnerState{testRunnerState("one", 1)}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.acquire(ctx, "one", "r_first"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.acquire(ctx, "one", "r_second") }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiting for a slot should end with the context: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the run did not release its wait for a slot")
	}
}

// §20: never accept a run onto a runner that cannot execute it.
//
// The refusal has to happen at Prepare rather than at the claim, because a
// worker cannot know which runner a queued run needs until gate 13 has read
// jobs.yaml off the checkout (amendment A12). Prepare is the last place the
// answer exists and the first place it is known.
func TestGatedExecutorRefusesAnUnavailableRunner(t *testing.T) {
	fx := newFakeExecutor(t, "docker")
	state := testRunnerState("docker-1", 1)
	state.ex = fx
	g := &gatedExecutor{inner: fx, state: state, slots: newSlots([]*runnerState{state}, time.Second)}

	state.setAvailable(false, "docker daemon unreachable")
	_, err := g.Prepare(context.Background(), executor.Spec{RunID: "r_x"})
	if !errors.Is(err, ErrRunnerUnavailable) {
		t.Fatalf("Prepare on an unavailable runner = %v, want ErrRunnerUnavailable", err)
	}
	if !strings.Contains(err.Error(), "docker daemon unreachable") {
		t.Errorf("the refusal must carry the preflight reason: %v", err)
	}

	// Recovery is automatic; §20 is explicit that a runner comes back on its
	// own when the next check passes.
	state.setAvailable(true, "")
	env, err := g.Prepare(context.Background(), executor.Spec{RunID: "r_x"})
	if err != nil {
		t.Fatalf("Prepare after recovery: %v", err)
	}
	if err := env.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A failed Prepare must not keep the slot, or one broken run costs the runner
// a permanent unit of concurrency.
func TestGatedExecutorReleasesTheSlotWhenPrepareFails(t *testing.T) {
	fx := newFakeExecutor(t, "docker")
	fx.prepareErr = errBoom
	state := testRunnerState("docker-1", 1)
	state.ex = fx
	s := newSlots([]*runnerState{state}, 50*time.Millisecond)
	g := &gatedExecutor{inner: fx, state: state, slots: s}

	if _, err := g.Prepare(context.Background(), executor.Spec{RunID: "r_fail"}); err == nil {
		t.Fatal("Prepare should have failed")
	}
	if err := s.acquire(context.Background(), "docker-1", "r_next"); err != nil {
		t.Errorf("the slot was not returned after a failed Prepare: %v", err)
	}
}

// Closing the environment returns the slot and the active count, so the
// heartbeat's view of the machine does not drift up over a day of building.
func TestGatedEnvReleasesOnceOnClose(t *testing.T) {
	fx := newFakeExecutor(t, "docker")
	state := testRunnerState("docker-1", 1)
	state.ex = fx
	s := newSlots([]*runnerState{state}, 50*time.Millisecond)
	g := &gatedExecutor{inner: fx, state: state, slots: s}

	env, err := g.Prepare(context.Background(), executor.Spec{RunID: "r_x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := state.info().Active; got != 1 {
		t.Errorf("Active = %d while a run holds the environment, want 1", got)
	}
	for range 3 {
		if err := env.Close(context.Background()); err != nil {
			t.Fatalf("Close must be idempotent: %v", err)
		}
	}
	if got := state.info().Active; got != 0 {
		t.Errorf("Active = %d after Close, want 0", got)
	}
	if err := s.acquire(context.Background(), "docker-1", "r_next"); err != nil {
		t.Errorf("the slot was not returned on Close: %v", err)
	}
}

// Reap and Preflight are deliberately not gated: reconciliation and recovery
// both have to work precisely when the runner is unavailable, which is the one
// state where a gate would be tempting and wrong.
func TestGatedExecutorDoesNotGateReapOrPreflight(t *testing.T) {
	fx := newFakeExecutor(t, "docker")
	state := testRunnerState("docker-1", 1)
	state.ex = fx
	state.setAvailable(false, "down")
	g := &gatedExecutor{inner: fx, state: state, slots: newSlots([]*runnerState{state}, time.Second)}

	if err := g.Preflight(context.Background()); err != nil {
		t.Errorf("Preflight on an unavailable runner should still run: %v", err)
	}
	fx.leak(t, "r_orphan")
	reaped, err := g.Reap(context.Background(), func(string) bool { return false })
	if err != nil {
		t.Fatalf("Reap on an unavailable runner: %v", err)
	}
	if len(reaped) != 1 {
		t.Errorf("Reap returned %d, want the orphan; a dark runner still has litter", len(reaped))
	}
	if g.Name() != "docker" {
		t.Errorf("Name = %q, want the inner executor's", g.Name())
	}
}
