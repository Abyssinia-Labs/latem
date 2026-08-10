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
	// still holds the transition and that broker's reported_state still lags
	// it, so store.Unreported replays the whole thing on the next reconnect.
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

// reporter publishes state transitions to every broker without ever blocking
// the run that produced them.
//
// One goroutine per (broker, live run), fed by its own queue. Two things follow
// from that shape and both are the point:
//
//   - Reports for a single run reach a single broker in order. A control plane
//     that sees `collecting` after `succeeded` shows a finished build as still
//     working. Runs do not need ordering between each other, and a shared worker
//     would make one unreachable run's backoff delay every other run's reports.
//   - Brokers cannot delay each other. The remote transport retries five times
//     with doubling backoff against a control plane that may be in the middle of
//     a ten-minute partition; publishing to both from one goroutine would put
//     that delay in front of the local transport, which is the one `latem run
//     --wait` and the whole local product depend on. A hosted layer's outage
//     must cost this machine nothing, and "nothing" includes latency.
//
// Marking a transition reported is deliberately not done here. It is a claim
// about one transport's far side, and only that transport knows when it is true
// — so each writes its own broker_state row (amendment A28). This type would
// have to guess a key, and it is not the only caller of Broker.Report: the
// agent's reconciliation and TTL sweep report outside it entirely.
type reporter struct {
	brks     []broker.Broker
	attempts int
	backoff  time.Duration

	mu     sync.Mutex
	queues map[queueKey]chan protocol.Update
	closed bool
	wg     sync.WaitGroup
}

// queueKey identifies one run's stream of updates to one broker.
type queueKey struct {
	broker int
	runID  string
}

func newReporter(brks []broker.Broker, attempts int, backoff time.Duration) *reporter {
	return &reporter{
		brks:     brks,
		attempts: orDefaultInt(attempts, defaultReportAttempts),
		backoff:  orDefault(backoff, defaultReportBackoff),
		queues:   map[queueKey]chan protocol.Update{},
	}
}

// enqueue schedules one update for publication on every broker. It never blocks
// and never returns an error: a broker that will not take a transition is a
// normal condition, and the run has already written the only copy that matters.
func (r *reporter) enqueue(runID string, u protocol.Update) {
	for i := range r.brks {
		r.enqueueTo(i, runID, u)
	}
}

func (r *reporter) enqueueTo(brk int, runID string, u protocol.Update) {
	key := queueKey{broker: brk, runID: runID}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	q, ok := r.queues[key]
	if !ok {
		q = make(chan protocol.Update, reportQueueDepth)
		r.queues[key] = q
		r.wg.Add(1)
		go r.serve(r.brks[brk], runID, q)
	}
	r.mu.Unlock()

	select {
	case q <- u:
	default:
		// Full. Dropping is the honest answer: the transition is durable in
		// sqlite and this broker's reported_state still lags it, so
		// store.Unreported picks it up. Forcing it through out of band would
		// reorder the run's history on the far side, which is worse than
		// replaying it later in order.
	}
}

// serve publishes one run's updates to one broker, in order, until the queue is
// closed.
func (r *reporter) serve(brk broker.Broker, runID string, q <-chan protocol.Update) {
	defer r.wg.Done()
	for u := range q {
		r.publish(brk, runID, u)
	}
}

// publish makes up to attempts tries with exponential backoff.
func (r *reporter) publish(brk broker.Broker, runID string, u protocol.Update) bool {
	delay := r.backoff
	for attempt := 0; attempt < r.attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		err := brk.Report(ctx, runID, u)
		cancel()
		if err == nil {
			return true
		}
	}
	return false
}

// forget closes a finished run's queues, one per broker. Each goroutine drains
// what is left and exits, so a report enqueued microseconds before the run ended
// is still sent.
func (r *reporter) forget(runID string) {
	r.mu.Lock()
	qs := make([]chan protocol.Update, 0, len(r.brks))
	for key, q := range r.queues {
		if key.runID == runID {
			qs = append(qs, q)
			delete(r.queues, key)
		}
	}
	r.mu.Unlock()

	for _, q := range qs {
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
	for key, q := range r.queues {
		qs = append(qs, q)
		delete(r.queues, key)
	}
	r.mu.Unlock()

	for _, q := range qs {
		close(q)
	}
	r.wg.Wait()
}
