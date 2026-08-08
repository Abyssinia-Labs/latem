package run

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// errCancelRequested is the cause a run's context carries when a cancel
// command reached it, as opposed to the job wall clock expiring or the agent
// shutting down. [context.Cause] is what separates the three at the point they
// all look identical — a step returning context.Canceled.
var errCancelRequested = errors.New("run: cancel requested")

// handle is the live state of one executing run, and the only thing
// [Orchestrator.Cancel] can reach.
type handle struct {
	runID  string
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	phase     protocol.RunState
	requested bool
	reason    string
	actor     string
	ring      *logstream.RingSink
}

// setPhase records which §11 state the run is in, because the effect of a
// cancel depends on it.
func (h *handle) setPhase(s protocol.RunState) {
	h.mu.Lock()
	h.phase = s
	h.mu.Unlock()
}

// request applies a cancel and reports whether it took effect.
//
// RUNNER_SPEC §11, per state: immediate in queued, awaiting_approval and
// claimed, because nothing has executed; propagated to the executor in
// preparing and running; and ignored in collecting, because artifacts and logs
// from a stopped build are the entire reason collecting always runs. Stopping
// it would throw away exactly what the person who pressed Stop wants to read.
func (h *handle) request(reason, actor string) bool {
	h.mu.Lock()
	if h.phase == protocol.StateCollecting {
		h.mu.Unlock()
		return false
	}
	first := !h.requested
	h.requested = true
	if first {
		h.reason, h.actor = reason, actor
	}
	h.mu.Unlock()

	if first {
		h.cancel(errCancelRequested)
	}
	return true
}

// cancelled reports whether a cancel was requested, and with what reason.
func (h *handle) cancelled() (bool, string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requested, h.reason, h.actor
}

// enterCollecting closes the door on cancellation and reports what got through.
//
// The two halves are one operation because there is a window between them
// otherwise: a cancel landing after the check and before the phase change would
// cancel a context nothing is listening to any more, and the run would report
// as though nobody had pressed Stop. Under one lock, a cancel is either
// honoured or ignored, and the caller is told which.
func (h *handle) enterCollecting() (bool, string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.phase = protocol.StateCollecting
	return h.requested, h.reason, h.actor
}

// attachRing publishes the run's in-memory tail, which is what backs
// `latem logs -f` for a run already in flight (§16).
func (h *handle) attachRing(r *logstream.RingSink) {
	h.mu.Lock()
	h.ring = r
	h.mu.Unlock()
}

func (h *handle) ringSink() *logstream.RingSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ring
}

// begin registers a run as executing and derives its cancellable context.
func (o *Orchestrator) begin(ctx context.Context, runID string) (context.Context, *handle, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.live[runID]; ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrAlreadyExecuting, runID)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	h := &handle{runID: runID, cancel: cancel, phase: protocol.StateClaimed}
	o.live[runID] = h
	return runCtx, h, nil
}

// end deregisters a run. The context is released here rather than by the
// caller so that a return path which forgot to cancel cannot leak it.
func (o *Orchestrator) end(h *handle) {
	o.mu.Lock()
	delete(o.live, h.runID)
	o.mu.Unlock()
	h.cancel(context.Canceled)
}

// Cancel stops a run (RUNNER_SPEC §11, finding F4).
//
// It is idempotent, and a cancel for a run this agent does not know or has
// already finished is a no-op that reports success — a phone tapping Stop
// twice, or tapping it as the build goes green, is not an error and must not
// be rendered as one.
//
// Three cases, in the order they are checked:
//
//   - The run is executing here. Its context is cancelled, which propagates to
//     the executor; the executing goroutine drives it through collecting to
//     cancelled. A cancel arriving during collecting is ignored.
//   - The run is waiting or claimed and nothing has executed. It moves to
//     cancelled directly, here.
//   - Anything else — unknown id, terminal state, malformed id — is a no-op.
//
// Reason crossed the trust boundary as free text. It is redacted and bounded
// before it is stored or reported, exactly like an error message.
func (o *Orchestrator) Cancel(ctx context.Context, req protocol.CancelRequest) error {
	if err := protocol.ValidateID(req.ID); err != nil {
		// An id that cannot name a run names no run. §11's "unknown run is a
		// no-op reporting success" covers it, and refusing here would let a
		// broker learn which ids are well-formed by which cancels error.
		return nil
	}
	req.Normalize()

	o.mu.Lock()
	h := o.live[req.ID]
	o.mu.Unlock()

	if h != nil {
		h.request(req.Reason, req.Actor)
		return nil
	}

	r, err := o.store.Get(ctx, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run: cancel %s: %w", req.ID, err)
	}
	if r.State.IsTerminal() || r.State == protocol.StateCollecting {
		return nil
	}

	finished := o.now()
	u := protocol.Update{
		State:        protocol.StateCancelled,
		FinishedAt:   &finished,
		CancelReason: cancelReason(req),
	}
	if err := o.transition(ctx, req.ID, protocol.StateCancelled, u); err != nil {
		// The run left the state we read between the Get and the Transition —
		// it was claimed, or it finished. Either way the cancel has nothing
		// left to do, and §11 says that is a success.
		if errors.Is(err, store.ErrIllegalTransition) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	o.reporter.forget(req.ID)
	return nil
}

// cancelReason renders a CancelRequest for display, bounded and attributed.
// The redactor is applied by the caller that owns one; the store applies its
// own on the way in, which covers the path with no run-scoped redactor yet.
func cancelReason(req protocol.CancelRequest) string {
	switch {
	case req.Reason != "" && req.Actor != "":
		return truncate(fmt.Sprintf("%s (by %s)", req.Reason, req.Actor), MaxErrorMsgBytes)
	case req.Reason != "":
		return truncate(req.Reason, MaxErrorMsgBytes)
	case req.Actor != "":
		return truncate(fmt.Sprintf("cancelled by %s", req.Actor), MaxErrorMsgBytes)
	default:
		return "cancelled"
	}
}

// Live reports the run ids this orchestrator is currently executing, for the
// heartbeat's capacity view and for reconciliation's "is this run still going"
// question (RUNNER_SPEC §14, §17 both take a live predicate).
func (o *Orchestrator) Live() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.live))
	for id := range o.live {
		out = append(out, id)
	}
	return out
}

// IsLive reports whether one run is executing. It is the predicate
// cache.Options.Live and artifact.PruneOptions.Live want, so a sweep cannot
// delete a workspace or an artifact out from under a build in flight.
func (o *Orchestrator) IsLive(runID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.live[runID]
	return ok
}

// teardownContext detaches from a cancelled context and bounds the work.
//
// Teardown must run on the cancellation path — that is most of what it is for
// — so it cannot inherit a context that cancellation has already closed. It
// still gets a deadline, because a wedged daemon must not become a wedged
// agent.
func (o *Orchestrator) teardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), o.teardownTimeout)
}

// Tail returns the buffered lines of a run in flight, oldest first, and whether
// the run is executing here. It backs `latem logs` for a live run; a finished
// one is read from the file the pipeline wrote (§16).
func (o *Orchestrator) Tail(runID string) ([]protocol.LogFrame, bool) {
	o.mu.Lock()
	h := o.live[runID]
	o.mu.Unlock()
	if h == nil {
		return nil, false
	}
	ring := h.ringSink()
	if ring == nil {
		return nil, false
	}
	return ring.Snapshot(), true
}

// Subscribe streams subsequent lines of a run in flight, for `latem logs -f`.
// The returned function stops the subscription and must be called.
func (o *Orchestrator) Subscribe(runID string, buffer int) (<-chan protocol.LogFrame, func(), bool) {
	o.mu.Lock()
	h := o.live[runID]
	o.mu.Unlock()
	if h == nil {
		return nil, nil, false
	}
	ring := h.ringSink()
	if ring == nil {
		return nil, nil, false
	}
	ch, stop := ring.Subscribe(buffer)
	return ch, stop, true
}
