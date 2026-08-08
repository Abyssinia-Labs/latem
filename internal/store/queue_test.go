package store

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func enqueue(t *testing.T, d *DB, id string, priority int) {
	t.Helper()

	mustCreate(t, d, testRun(id, protocol.StateQueued))
	if err := d.Enqueue(t.Context(), id, priority); err != nil {
		t.Fatalf("Enqueue %s: %v", id, err)
	}
}

func TestClaimNext_PriorityThenArrival(t *testing.T) {
	d := newTestDB(t)

	// Enqueued in an order that differs from the claim order in both dimensions.
	enqueue(t, d, "low-first", 0)
	enqueue(t, d, "high", 10)
	enqueue(t, d, "low-second", 0)

	want := []string{"high", "low-first", "low-second"}
	for _, id := range want {
		got, err := d.ClaimNext(t.Context(), nil)
		if err != nil {
			t.Fatalf("ClaimNext: %v", err)
		}
		if got.ID != id {
			t.Fatalf("ClaimNext = %s, want %s (priority first, then arrival)", got.ID, id)
		}
		if got.State != protocol.StateClaimed {
			t.Errorf("%s came back as %q, want claimed", got.ID, got.State)
		}
	}

	if _, err := d.ClaimNext(t.Context(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClaimNext on an empty queue = %v, want ErrNotFound", err)
	}
}

// TestClaimNext_ClearsDeadlineAndDequeues: a claimed run has left the queue in
// both senses, and keeping either trace would have the TTL sweep reject a build
// that is already running.
func TestClaimNext_ClearsDeadlineAndDequeues(t *testing.T) {
	d := newTestDB(t)
	deadline := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	r := testRun("r", protocol.StateQueued)
	r.ExpiresAt = &deadline
	mustCreate(t, d, r)
	if err := d.Enqueue(t.Context(), "r", 0); err != nil {
		t.Fatal(err)
	}

	got, err := d.ClaimNext(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt != nil {
		t.Errorf("claimed run kept expires_at = %v", got.ExpiresAt)
	}

	// Re-read rather than trusting the RETURNING row: the claim is one
	// UPDATE ... RETURNING whose result set is closed after the first row, and
	// what matters is that the write landed on disk, not what was echoed back.
	stored, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != protocol.StateClaimed || stored.ExpiresAt != nil {
		t.Errorf("stored run = %q with expires_at %v, want claimed with none", stored.State, stored.ExpiresAt)
	}

	depth, err := d.QueueDepth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if depth != 0 {
		t.Errorf("queue depth = %d after claiming the only run", depth)
	}
}

// TestClaimNext_RunnerFilter documents what the filter can and cannot do. It
// matches runs.runner, which is NULL until gate 13 pins a run to a runner —
// after the claim — so an unpinned run is claimable by anyone.
func TestClaimNext_RunnerFilter(t *testing.T) {
	d := newTestDB(t)

	pin := func(id, runner string) {
		t.Helper()
		mustCreate(t, d, testRun(id, protocol.StateQueued))
		if err := d.SetRunner(t.Context(), id, runner); err != nil {
			t.Fatal(err)
		}
		if err := d.Enqueue(t.Context(), id, 0); err != nil {
			t.Fatal(err)
		}
	}
	pin("mac-only", "mac-tart")
	pin("linux-only", "docker")
	enqueue(t, d, "unpinned", 0)

	got, err := d.ClaimNext(t.Context(), []string{"docker"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "linux-only" && got.ID != "unpinned" {
		t.Fatalf("a docker runner claimed %s", got.ID)
	}

	// Only the mac run and whichever of the two above is left remain; a runner
	// name nobody is pinned to still gets the unpinned work.
	if _, err := d.ClaimNext(t.Context(), []string{"nonexistent-runner"}); err != nil {
		if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}

	// A mac run is never handed to a docker-only agent.
	for {
		got, err := d.ClaimNext(t.Context(), []string{"docker"})
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.ID == "mac-only" {
			t.Fatal("a run pinned to mac-tart was claimed by a docker runner")
		}
	}
}

func TestClaim_Specific(t *testing.T) {
	d := newTestDB(t)
	enqueue(t, d, "r", 0)

	if err := d.Claim(t.Context(), "r"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	got, err := d.Get(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != protocol.StateClaimed {
		t.Errorf("state = %q, want claimed", got.State)
	}

	// Claiming twice is the ordinary lost race, not a fault.
	if err := d.Claim(t.Context(), "r"); !errors.Is(err, ErrNotClaimable) {
		t.Errorf("second Claim = %v, want ErrNotClaimable", err)
	}
	// A bad id is a different thing and must be distinguishable from it.
	if err := d.Claim(t.Context(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim of an unknown run = %v, want ErrNotFound", err)
	}
}

func TestClaim_NotQueued(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("waiting", protocol.StateAwaitingApproval))

	// An unapproved run is not claimable, however it got into the queue.
	if err := d.Enqueue(t.Context(), "waiting", 0); err != nil {
		t.Fatal(err)
	}
	if err := d.Claim(t.Context(), "waiting"); !errors.Is(err, ErrNotClaimable) {
		t.Errorf("Claim of an awaiting_approval run = %v, want ErrNotClaimable", err)
	}
	if _, err := d.ClaimNext(t.Context(), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClaimNext with only an unapproved run queued = %v, want ErrNotFound", err)
	}
}

// TestClaim_ConcurrentExactlyOneWinner is the failure this whole design
// prevents: two runner goroutines executing one run. The claim is a conditional
// UPDATE serialized by sqlite's single writer, so exactly one contender may see
// a row change.
//
// The test discriminates. Replacing Claim's conditional UPDATE with a SELECT
// followed by an UPDATE — the implementation that reads identically and is
// wrong — produces six winners out of these sixteen contenders.
func TestClaim_ConcurrentExactlyOneWinner(t *testing.T) {
	d := newTestDB(t)
	enqueue(t, d, "contended", 0)

	const contenders = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		other   []error
	)
	start := make(chan struct{})

	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			err := d.Claim(t.Context(), "contended")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, ErrNotClaimable):
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d goroutines claimed the same run; exactly one must win", winners)
	}
	if len(other) > 0 {
		t.Fatalf("contenders failed with something other than ErrNotClaimable: %v", other)
	}
}

// TestClaimNext_ConcurrentNoDoubleClaim is the same property for the queue as a
// whole: every run is handed to exactly one caller, and no run is skipped.
func TestClaimNext_ConcurrentNoDoubleClaim(t *testing.T) {
	d := newTestDB(t)

	const (
		runs    = 24
		workers = 8
	)
	for i := range runs {
		enqueue(t, d, "run-"+strconv.Itoa(i), i%3)
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims = map[string]int{}
		fails  []error
	)
	start := make(chan struct{})

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for {
				r, err := d.ClaimNext(t.Context(), nil)
				if errors.Is(err, ErrNotFound) {
					return
				}
				mu.Lock()
				if err != nil {
					fails = append(fails, err)
					mu.Unlock()
					return
				}
				claims[r.ID]++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(fails) > 0 {
		t.Fatalf("ClaimNext failed: %v", fails)
	}
	if len(claims) != runs {
		t.Errorf("%d distinct runs claimed, want %d — the queue skipped work", len(claims), runs)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("run %s was claimed %d times; two agents would execute it", id, n)
		}
	}

	// Every run must have moved out of queued exactly once, too.
	remaining, err := d.List(t.Context(), Filter{
		States: []protocol.RunState{protocol.StateQueued},
		Limit:  runs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d runs are still queued after every claim succeeded", len(remaining))
	}
}

func TestEnqueue_Rejects(t *testing.T) {
	d := newTestDB(t)

	if err := d.Enqueue(t.Context(), "ghost", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Enqueue of an unknown run = %v, want ErrNotFound", err)
	}

	// A terminal run in the queue is a row ClaimNext can never take, which
	// looks like work that is never done.
	mustCreate(t, d, testRun("done", protocol.StateSucceeded))
	if err := d.Enqueue(t.Context(), "done", 0); err == nil {
		t.Error("Enqueue accepted a finished run")
	}
}

// TestEnqueue_RepriorityKeepsPlaceInLine: a re-enqueue changes priority and
// keeps enqueued_at, so a retry loop cannot starve everything behind it by
// repeatedly sending itself to the back and then to the front.
func TestEnqueue_RepriorityKeepsPlaceInLine(t *testing.T) {
	d := newTestDB(t)

	enqueue(t, d, "first", 0)
	enqueue(t, d, "second", 0)

	var before int64
	err := d.db.QueryRowContext(t.Context(), `SELECT enqueued_at FROM queue WHERE run_id = ?`, "first").Scan(&before)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.Enqueue(t.Context(), "first", 5); err != nil {
		t.Fatal(err)
	}

	var after int64
	var priority int
	err = d.db.QueryRowContext(t.Context(),
		`SELECT enqueued_at, priority FROM queue WHERE run_id = ?`, "first").Scan(&after, &priority)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("enqueued_at moved on re-enqueue (%d -> %d)", before, after)
	}
	if priority != 5 {
		t.Errorf("priority = %d after re-enqueue, want 5", priority)
	}
}

func TestQueueDepthAndDequeue(t *testing.T) {
	d := newTestDB(t)
	enqueue(t, d, "a", 0)
	enqueue(t, d, "b", 0)

	depth, err := d.QueueDepth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if depth != 2 {
		t.Fatalf("queue depth = %d, want 2", depth)
	}

	if err := d.Dequeue(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	// Dequeuing leaves the run's state alone; only its place in line is gone.
	got, err := d.Get(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != protocol.StateQueued {
		t.Errorf("state = %q after Dequeue, want it unchanged", got.State)
	}
	if depth, err = d.QueueDepth(t.Context()); err != nil || depth != 1 {
		t.Errorf("queue depth = %d, %v; want 1", depth, err)
	}

	// Removing something that is not there is the outcome the caller asked for.
	if err := d.Dequeue(t.Context(), "ghost"); err != nil {
		t.Errorf("Dequeue of an unknown run = %v, want nil", err)
	}
}
