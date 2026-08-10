package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// maxBatchBytes bounds one outbound log message.
//
// It is well under [MaxFrameBytes] because a reconnect flush can have fifty
// megabytes to move, and sending it as one message would mean a single
// acknowledgement for the whole outage: a link that died halfway would have
// nothing to show for the transfer. Chunking makes the resume point advance
// while the flush is still running.
const maxBatchBytes = 256 << 10

// frameOverhead approximates the JSON around a log line — the field names, the
// quoting, the RFC 3339 timestamp — for the purpose of the buffer bound. It
// does not have to be exact; it has to be an over-estimate, so that "50 MB of
// buffer" is never more than 50 MB of memory.
const frameOverhead = 128

// sink is the remote [broker.LogSink]: one run's output, sequenced, buffered
// across a partition, and acknowledged by the far side.
//
// # What it holds, and until when
//
// It holds every frame the control plane has not acknowledged. Not every frame
// that has not been *sent* — every frame that has not been *acknowledged*. That
// distinction is finding F6: a batch written into a socket that then dies was
// probably not received, so it stays here and is offered again after the
// reconnect, and the server's `(run_id, seq)` dedupe (§7.2) settles the case
// where it did arrive. A sink that dropped frames at the point of writing would
// lose exactly the output covering the outage, which is the part anybody cares
// about.
//
// # The bound, and why the gap is loud
//
// The buffer is capped at 50 MB per run (§7.4). Beyond that, arriving frames
// are dropped and counted, and the next flush emits one marker frame saying how
// many were lost. Sequence numbers are assigned upstream, by the emitter (§16),
// so they keep incrementing across the gap whether or not this package is
// keeping up — the server sees 1…1000, a marker, then 15001, and the hole is
// visible rather than silent. The complete log is still on the machine, in
// ~/.latem/logs/<run-id>.log, which is why dropping is an acceptable answer at
// all.
//
// The marker takes the sequence number of the *first* frame it stands in for.
// That keeps the stream contiguous at the point the resume frame told the
// server to continue from, and it means the marker is deduped like anything
// else if a flush is retried.
//
// # A deliberate deviation: the buffer is in memory, not on disk
//
// §7.4 says "log frames buffer to disk while disconnected". This buffers in
// memory, bounded identically, and the deviation is recorded here rather than
// quietly taken.
//
// The reason is that the frames are already on disk. §16 fans every one of them
// to logstream.FileSink, which writes ~/.latem/logs/<run-id>.log, so a disk
// spool here would be a second copy of a file this machine already has — and it
// would want that copy on the same filesystem the build is filling, on a
// machine whose disk floors (§13, §14) exist because running out of space
// mid-build is the failure mode that costs the most. Spending 50 MB of somebody
// else's disk to avoid 50 MB of this process's memory, on a box provisioned for
// a 90 GB Xcode image, is the wrong trade.
//
// What that costs is narrow and worth stating: an agent that *restarts* mid
// partition loses the unacknowledged buffer, where a disk spool would not. It
// does not lose the log — that is still in the run's own file — and the run
// itself is reconciled to interrupted at the next start (§6 step 5), so the
// stream it belonged to is over either way. A spool would hook in at
// [sink.Write] and [sink.flush] if that trade is ever judged differently.
//
// # Why the newest frames are dropped rather than the oldest
//
// A failing build's interesting output is at the end, so discarding the head of
// the buffer is tempting. It is wrong here: the resume contract is "ingestion
// resumes at last_acked_seq + 1", and a buffer that discarded its head would
// make every reconnect begin with a hole in exactly the place the server was
// told to continue from. The tail is recoverable — it is in the log file, and
// the run's terminal state and error message travel on a different frame
// entirely. Contiguity from the resume point is not.
type sink struct {
	b     *Broker
	runID string

	// flushMu serializes [sink.flush]. It is not mu and it cannot be mu: mu
	// guards the buffer and is deliberately dropped before anything touches the
	// network, or a slow socket would hold up the emitter that is filling it.
	//
	// Without it the frames arrive out of order. [sink.nextBatch] is atomic, so
	// two concurrent flushes carve the buffer into two correct, disjoint batches
	// — and then race to write them, so the socket can carry 121…150 before
	// 41…120. Every frame arrives and none is duplicated, and a server that
	// orders by seq is unharmed; a server that appends in arrival order shows the
	// build's output shuffled, and the region it shuffles is the one the
	// reconnect just recovered. The two flushers are not hypothetical: the
	// reconnect calls [Broker.flushSinks] on the connect goroutine while the run
	// it is recovering is still writing into the same sink.
	flushMu sync.Mutex

	mu  sync.Mutex
	buf []protocol.LogFrame
	// bufBytes is the approximate memory held by buf, bounded by b.maxBuffer.
	bufBytes int64
	// acked is the highest sequence the control plane has taken.
	acked uint64
	// sent is the highest sequence written on the current session. It is reset
	// to acked whenever the session ends, which is what re-offers an unanswered
	// batch after a reconnect.
	sent uint64
	// dropped counts frames discarded since the last marker; dropAt is the
	// sequence of the first of them.
	dropped uint64
	dropAt  uint64

	closed bool
	// decided and ship are the [LogsOnFailure] verdict: whether the run's
	// outcome is known, and whether the buffer is to be shipped at all.
	decided bool
	ship    bool
}

var _ broker.LogSink = (*sink)(nil)

// newSink starts a sink at the run's persisted resume point, so a sink opened
// after a restart does not claim an ack of zero for a run whose log the control
// plane already holds.
func newSink(b *Broker, runID string, acked uint64) *sink {
	s := &sink{b: b, runID: runID, acked: acked, sent: acked}
	if b.mode == LogsAlways {
		// Nothing to decide: frames ship as they are produced.
		s.decided, s.ship = true, true
	}
	return s
}

// Write accepts a batch of frames.
//
// Frames belonging to another run are refused, exactly as the local sink
// refuses them: sequences are per-run, so a stray frame would advance the wrong
// resume point — silently, and in the direction that loses log lines.
func (s *sink) Write(ctx context.Context, frames []protocol.LogFrame) error {
	if len(frames) == 0 {
		return nil
	}
	for _, f := range frames {
		if f.RunID != s.runID {
			return fmt.Errorf("remote: sink for run %s was handed a frame for run %s", s.runID, f.RunID)
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("remote: write to a closed log sink")
	}
	for _, f := range frames {
		if f.Seq <= s.acked {
			// Already durable on the far side. Re-buffering it would be the
			// duplication finding F6 exists to prevent.
			continue
		}
		size := frameBytes(f)
		if s.bufBytes+size > s.b.maxBuffer {
			if s.dropped == 0 {
				s.dropAt = f.Seq
			}
			s.dropped++
			continue
		}
		s.buf = append(s.buf, f)
		s.bufBytes += size
	}
	s.mu.Unlock()

	if s.shippable() {
		s.flush(ctx)
	}
	return nil
}

// Acked reports the highest sequence the control plane has durably accepted.
//
// It is deliberately not "the highest sequence sent". The agent persists this
// and resumes from Acked()+1, so claiming an ack the far side never gave would
// make the next reconnect skip precisely the frames that did not make it.
func (s *sink) Acked() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// Close ends the run's stream.
//
// It does not discard what has not been acknowledged. A partition that spans
// the end of a build is the ordinary case — the last thing a failing run does
// is print why — and a sink that dropped its buffer at Close would lose it. The
// broker keeps the sink registered until the buffer drains or the run's outcome
// says it never will; see [Broker.resolveSink].
//
// Idempotent: shutdown paths close things twice.
func (s *sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.shippable() {
		s.flush(ctx)
	}
	s.retireIfDone()
	return nil
}

// decide records the run's outcome, which is what [LogsOnFailure] turns on.
//
// A failing terminal state ships the buffer; a succeeded one discards it. §7.4
// puts the mode there for verbose Gradle output, where the successful builds
// are the ones nobody reads and the bandwidth is real.
//
// "Failing" is every terminal state except succeeded. cancelled and timed_out
// are included on purpose: somebody stopped or outlasted that build and the
// first thing they will ask is what it was doing.
func (s *sink) decide(ctx context.Context, state protocol.RunState) {
	if !state.IsTerminal() {
		return
	}
	s.mu.Lock()
	if s.decided {
		s.mu.Unlock()
		s.retireIfDone()
		return
	}
	s.decided = true
	s.ship = state != protocol.StateSucceeded
	ship := s.ship
	if !ship {
		s.buf = nil
		s.bufBytes = 0
		s.dropped = 0
	}
	s.mu.Unlock()

	if ship {
		s.flush(ctx)
	}
	s.retireIfDone()
}

// shippable reports whether this sink may put frames on the wire yet.
func (s *sink) shippable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decided && s.ship
}

// flush offers everything unsent to the control plane, in order, in chunks.
//
// It stops at the first write that fails. Nothing is discarded when it does:
// the frames stay buffered, `sent` rewinds to the resume point, and the next
// connection offers them again.
//
// It returns immediately when there is no connection, and that early exit is
// load-bearing rather than an optimisation. [sink.nextBatch] materialises the
// drop marker and zeroes the counter behind it, so calling it during a
// partition — which every Write would do — would mint one marker per batch
// written while the link was down, instead of the single marker §7.4 asks for
// per gap.
func (s *sink) flush(ctx context.Context) {
	if !s.shippable() || !s.b.Connected() {
		return
	}
	// Ordered, not merely correct. See [sink.flushMu].
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	for {
		batch, high, ok := s.nextBatch()
		if !ok {
			return
		}
		if err := s.sendBatch(ctx, batch, high); err != nil {
			s.rewind()
			return
		}
	}
}

// nextBatch takes the next chunk of unsent frames, appending the drop marker
// first if one is owed.
func (s *sink) nextBatch() ([]protocol.LogFrame, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dropped > 0 {
		// The marker is allowed past the buffer bound. It is one short line, and
		// it is the one line worth keeping when the rest had to go: a gap the
		// server can see is a different thing from a gap it cannot.
		s.buf = append(s.buf, s.marker())
		s.dropped = 0
		s.dropAt = 0
	}

	var (
		batch []protocol.LogFrame
		bytes int64
		high  = s.sent
	)
	for _, f := range s.buf {
		if f.Seq <= s.sent {
			continue
		}
		size := frameBytes(f)
		if len(batch) > 0 && bytes+size > maxBatchBytes {
			break
		}
		batch = append(batch, f)
		bytes += size
		high = f.Seq
	}
	if len(batch) == 0 {
		return nil, 0, false
	}
	s.sent = high
	return batch, high, true
}

// marker is the frame that records a gap. Caller holds s.mu.
func (s *sink) marker() protocol.LogFrame {
	return protocol.LogFrame{
		RunID:  s.runID,
		Seq:    s.dropAt,
		Stream: protocol.StreamAgent,
		At:     s.b.now(),
		Line: fmt.Sprintf(
			"[latem] %d log lines were dropped while the control plane was unreachable "+
				"(per-run buffer limit %d MB); the complete log is on the machine that ran the build",
			s.dropped, s.b.maxBuffer>>20),
	}
}

// sendBatch writes one chunk and registers the waiter that will advance the
// resume point when the far side answers.
//
// The waiter is registered before the write, because an acknowledgement can
// arrive on the reader goroutine before this one returns.
func (s *sink) sendBatch(ctx context.Context, batch []protocol.LogFrame, high uint64) error {
	id, _, err := s.b.register(&waiter{runID: s.runID, highSeq: high})
	if err != nil {
		return err
	}
	if err := s.b.send(ctx, frame{Kind: kindLogs, ID: id, Logs: batch}); err != nil {
		s.b.forget(id)
		return err
	}
	return nil
}

// rewind re-offers everything the far side has not acknowledged.
func (s *sink) rewind() {
	s.mu.Lock()
	s.sent = s.acked
	s.mu.Unlock()
}

// ack advances the resume point.
//
// A refusal is treated as acceptance, with a warning. That is not politeness:
// the alternative is re-offering the same batch on every reconnect for the life
// of the run, and a control plane that will not take a log frame has said
// everything it is going to say about it. The lines are still in the run's log
// file on this machine.
func (s *sink) ack(high uint64, a protocol.Ack) {
	if !a.OK {
		s.b.log.Warn("the control plane refused a log batch",
			"run_id", s.runID, "through_seq", high, "err", ackReason(a))
	}

	s.mu.Lock()
	if high > s.acked {
		s.acked = high
		kept := s.buf[:0]
		var bytes int64
		for _, f := range s.buf {
			if f.Seq > s.acked {
				kept = append(kept, f)
				bytes += frameBytes(f)
			}
		}
		s.buf = kept
		s.bufBytes = bytes
	}
	if s.sent < s.acked {
		s.sent = s.acked
	}
	acked := s.acked
	s.mu.Unlock()

	s.persist(acked)
	s.retireIfDone()
}

// persist writes the resume point to the store so it survives a restart and not
// merely a reconnect (finding F6).
//
// It is written under [store.BrokerRemote] and reads back only from there
// (amendment A28). Before the key existed this was the same column the local
// broker advanced on every batch it wrote to disk, so the value a restarted
// agent read back was "everything this machine has ever emitted" rather than
// "everything the control plane took" — and the difference is exactly the
// frames covering an outage.
//
// store.SetLastAckedSeq is monotonic within a transport, so this cannot rewind
// anything.
func (s *sink) persist(seq uint64) {
	if seq == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.b.st.SetLastAckedSeq(ctx, store.BrokerRemote, s.runID, seq); err != nil {
		s.b.log.Debug("could not persist the log resume point", "run_id", s.runID, "seq", seq, "err", err)
	}
}

// retireIfDone unregisters a sink that owes the control plane nothing.
//
// A closed sink with an empty buffer is finished. A closed sink with frames
// still in it is not, whatever the run's state — those frames are the reason
// the reconnect flush exists.
func (s *sink) retireIfDone() {
	s.mu.Lock()
	done := s.closed && (len(s.buf) == 0 && s.dropped == 0 || s.decided && !s.ship)
	s.mu.Unlock()
	if done {
		s.b.dropSink(s.runID)
	}
}

// resetSession re-offers everything unacknowledged. Called when a connection
// ends, so the next one starts from the resume point rather than from wherever
// the last write happened to reach.
func (s *sink) resetSession() { s.rewind() }

// frameBytes over-estimates one frame's memory cost.
func frameBytes(f protocol.LogFrame) int64 {
	return int64(len(f.Line) + len(f.RunID) + len(f.Stream) + frameOverhead)
}
