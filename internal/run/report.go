package run

import (
	"context"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// defaultReportAttempts and defaultReportBackoff bound one report's retry.
	// RUNNER_SPEC §11: "Report is retried with backoff; the run does not block
	// on it." Giving up after a handful of tries is not data loss — the store
	// still holds the transition and ReportedState still lags it, so
	// store.Unreported replays the whole thing on the next reconnect.
	defaultReportAttempts = 5
	defaultReportBackoff  = 250 * time.Millisecond

	// reportQueueDepth is per run. A run has fewer than ten transitions in its
	// whole life, so this cannot fill in practice; the overflow path exists
	// because "cannot happen" is not a reason to corrupt ordering.
	reportQueueDepth = 32

	// reportTimeout bounds one attempt, so a broker that accepts a connection
	// and then says nothing cannot hold a reporting goroutine for the life of
	// the agent.
	reportTimeout = 30 * time.Second
)

// reporter publishes state transitions to the broker without ever blocking the
// run that produced them.
//
// One goroutine per live run, fed by that run's own queue, which is what keeps
// reports for a single run in order: a control plane that sees `collecting`
// after `succeeded` shows a finished build as still working. Runs do not need
// ordering between each other, and giving them a shared worker would make one
// unreachable run's backoff delay every other run's reports.
type reporter struct {
	brk      broker.Broker
	st       Store
	attempts int
	backoff  time.Duration

	mu     sync.Mutex
	queues map[string]chan protocol.Update
	closed bool
	wg     sync.WaitGroup
}

func newReporter(brk broker.Broker, st Store, attempts int, backoff time.Duration) *reporter {
	return &reporter{
		brk:      brk,
		st:       st,
		attempts: orDefaultInt(attempts, defaultReportAttempts),
		backoff:  orDefault(backoff, defaultReportBackoff),
		queues:   map[string]chan protocol.Update{},
	}
}

// enqueue schedules one update for publication. It never blocks and never
// returns an error: a broker that will not take a transition is a normal
// condition, and the run has already written the only copy that matters.
func (r *reporter) enqueue(runID string, u protocol.Update) {
	if r.brk == nil {
		return
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	q, ok := r.queues[runID]
	if !ok {
		q = make(chan protocol.Update, reportQueueDepth)
		r.queues[runID] = q
		r.wg.Add(1)
		go r.serve(runID, q)
	}
	r.mu.Unlock()

	select {
	case q <- u:
	default:
		// Full. Dropping is the honest answer: the transition is durable in
		// sqlite and ReportedState still lags it, so store.Unreported picks it
		// up. Forcing it through out of band would reorder the run's history
		// on the far side, which is worse than replaying it later in order.
	}
}

// serve publishes one run's updates in order until the queue is closed.
func (r *reporter) serve(runID string, q <-chan protocol.Update) {
	defer r.wg.Done()
	for u := range q {
		if r.publish(runID, u) {
			ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
			_ = r.st.MarkReported(ctx, runID, u.State)
			cancel()
		}
	}
}

// publish makes up to attempts tries with exponential backoff.
func (r *reporter) publish(runID string, u protocol.Update) bool {
	delay := r.backoff
	for attempt := 0; attempt < r.attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		err := r.brk.Report(ctx, runID, u)
		cancel()
		if err == nil {
			return true
		}
	}
	return false
}

// forget closes a finished run's queue. The goroutine drains what is left and
// exits, so a report enqueued microseconds before the run ended is still sent.
func (r *reporter) forget(runID string) {
	r.mu.Lock()
	q, ok := r.queues[runID]
	if ok {
		delete(r.queues, runID)
	}
	r.mu.Unlock()
	if ok {
		close(q)
	}
}

// close drains every queue and waits. It is idempotent.
func (r *reporter) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	qs := make([]chan protocol.Update, 0, len(r.queues))
	for id, q := range r.queues {
		qs = append(qs, q)
		delete(r.queues, id)
	}
	r.mu.Unlock()

	for _, q := range qs {
		close(q)
	}
	r.wg.Wait()
}
