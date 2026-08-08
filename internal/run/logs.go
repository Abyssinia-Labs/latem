package run

import (
	"context"
	"fmt"
	"io"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// brokerLogSink is the part of [broker.LogSink] this package holds on to after
// the emitter has been given ownership of the writing side.
type brokerLogSink interface {
	Acked() uint64
	Close(ctx context.Context) error
}

// openLogs builds this run's pipeline: split, redact, sequence, fan out (§16).
//
// The redactor is per run, because the values it holds are this run's secrets.
// It is created before anything else so that [secrets.ResolveAll] has something
// to register into, which is the ordering INV-4 turns on: registered before the
// first step, and before the value is used for anything else.
func (x *execution) openLogs(ctx context.Context) error {
	x.red = logstream.NewRedactor()

	file, err := logstream.NewFileSink(x.o.logDir, x.run.ID)
	if err != nil {
		return err
	}
	sinks := []logstream.Sink{file}

	ring := logstream.NewRingSink(x.o.ringSize)
	x.h.attachRing(ring)
	sinks = append(sinks, ring)

	if x.o.brk != nil {
		ls, lerr := x.o.brk.Logs(ctx, x.run.ID)
		switch {
		case lerr != nil:
			// A control plane that will not take logs is not a reason to
			// refuse to build. The file sink still has everything, and §7.2's
			// resume point means the far side can catch up later.
			x.emitLater("broker log sink unavailable, continuing locally: %v", lerr)
		default:
			x.logSink = ls
			sinks = append(sinks, logstream.NewBatchSink(
				&brokerSink{ctx: context.WithoutCancel(ctx), inner: ls},
				logstream.DefaultBatchFrames, logstream.DefaultBatchDelay))
		}
	}

	x.emitter = logstream.NewEmitter(x.run.ID, x.red, sinks...)
	x.flushDeferred()
	return nil
}

// emitLater and flushDeferred hold the handful of lines produced while the
// emitter is still being built, so a diagnostic about the log pipeline is not
// the one thing that never reaches the log.
func (x *execution) emitLater(format string, args ...any) {
	x.deferred = append(x.deferred, fmt.Sprintf(format, args...))
}

func (x *execution) flushDeferred() {
	for _, line := range x.deferred {
		x.emitter.Note("%s", line)
	}
	x.deferred = nil
}

// closeLogs flushes every sink and records the log resume point.
//
// LastAckedSeq is what makes resumption after a partition non-duplicating
// (finding F6). Persisting it here rather than only holding it in the sink is
// the difference between surviving a reconnect and surviving a restart.
func (x *execution) closeLogs(ctx context.Context) {
	if x.emitter == nil {
		return
	}
	_ = x.emitter.Close()

	if x.logSink == nil {
		return
	}
	cctx, cancel := x.o.teardownContext(ctx)
	defer cancel()
	if seq := x.logSink.Acked(); seq > 0 {
		_ = x.o.store.SetLastAckedSeq(cctx, x.run.ID, seq)
	}
}

// note writes one agent-stream line: step boundaries, gate results, teardown.
func (x *execution) note(format string, args ...any) {
	if x.emitter == nil {
		return
	}
	x.emitter.Note(format, args...)
}

// redact runs a string through this run's redactor.
//
// Everything reaching [protocol.Update.ErrorMsg] goes through here first. The
// store redacts again on the way in, with the agent-wide redactor; that one
// does not hold this run's secrets, so it is not a substitute for this. An
// error message is one of the easiest places for a credential to escape — a
// failing curl printing its own argv, a program echoing its configuration —
// and INV-4 makes no exception for the failure path.
func (x *execution) redact(s string) string {
	if s == "" || x.red == nil {
		return s
	}
	return x.red.Redact(s)
}

// brokerSink adapts [broker.LogSink] to [logstream.Sink].
//
// The two differ only in that the broker's methods take a context and the
// pipeline's do not — deliberately, because a sink is called from the emitter's
// critical section and giving it a cancellable context there would let a
// cancelled run drop the very lines explaining why it was cancelled.
type brokerSink struct {
	ctx   context.Context
	inner broker.LogSink
}

func (s *brokerSink) Write(frames []protocol.LogFrame) error {
	return s.inner.Write(s.ctx, frames)
}

func (s *brokerSink) Close() error { return s.inner.Close(s.ctx) }

// stepStream carries one step's stdout or stderr into the pipeline.
//
// The two are separate because [protocol.LogFrame] has a Stream field and
// draft 02's single writer made it unfillable (amendment A9). An operator
// reading a build wants to know which lines were the error.
type stepStream struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	done chan struct{}
}

// stream starts a consumer for one of a step's output streams.
func (x *execution) stream(s protocol.Stream) *stepStream {
	pr, pw := io.Pipe()
	st := &stepStream{pr: pr, pw: pw, done: make(chan struct{})}
	go func() {
		defer close(st.done)
		// Consume stops emitting — and therefore stops draining — as soon as
		// its context is done, which would block the executor writing into the
		// other end. A cancelled or timed-out step's partial output is exactly
		// the diagnostic §9 promises, so this context is not the run's.
		_ = x.emitter.Consume(context.Background(), s, pr)
	}()
	return st
}

// close ends the stream normally: the writer closes, the consumer reads to EOF.
func (s *stepStream) close() {
	_ = s.pw.Close()
	<-s.done
}

// abandon ends the stream when the executor did not stop within the grace.
//
// Closing the read side first is what makes abandonment safe: the goroutine
// still holding the writer gets ErrClosedPipe on its next write instead of
// blocking forever on a reader nobody is draining.
func (s *stepStream) abandon() {
	_ = s.pr.CloseWithError(errStepAbandoned)
	_ = s.pw.Close()
	<-s.done
}
