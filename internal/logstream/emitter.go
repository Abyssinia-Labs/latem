package logstream

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// PEM block delimiters, used by the multi-line guard below.
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// Sink receives batches of frames. Every frame reaching a sink has already
// been redacted and sequenced, so a new sink cannot bypass the redactor
// (INV-4) — the type system makes that the only way frames exist.
type Sink interface {
	Write(frames []protocol.LogFrame) error
	Close() error
}

// Emitter is the pipeline: split, redact, sequence, fan out.
//
// One per run. Ordering matters more than throughput here — a log whose lines
// arrive out of order is worse than a slightly slower one — so emission is
// serialised, and sequence assignment and fan-out happen under the same lock.
// The sinks downstream are buffered, so the critical section is short.
type Emitter struct {
	runID string
	red   Redactor
	now   func() time.Time

	mu    sync.Mutex
	seq   uint64
	sinks []Sink
	// inPEM is the multi-line guard. See [Emitter.Emit].
	inPEM bool
	// stopToken, when set, pauses workflow-command processing until a line
	// reading `::<token>::` arrives. See [Emitter.command].
	stopToken string
}

// NewEmitter builds a pipeline for one run.
func NewEmitter(runID string, red Redactor, sinks ...Sink) *Emitter {
	return &Emitter{
		runID: runID,
		red:   red,
		now:   time.Now,
		sinks: sinks,
	}
}

// Emit redacts, sequences, and fans out one line. It returns the frame it
// produced, which is already safe to transmit.
//
// # The private-key guard
//
// Beyond the §16 rules, the emitter tracks whether it is inside a PEM private
// key block and suppresses every line until the block closes.
//
// Registration handles declared secrets, and pattern matching handles
// recognisable one-line credentials. Neither covers an undeclared private key
// that a build prints — `cat key.pem`, a tool dumping its config, a failing
// script echoing a file it just wrote. The §16 pattern list matches the BEGIN
// and END delimiters, which means without this guard the log would show the
// header redacted and every base64 body line in the clear: a leak that looks
// like it was handled.
//
// The guard is deliberately narrow. It keys on "PRIVATE KEY", so certificates
// and public keys pass through untouched. If a block never closes, suppression
// continues to the end of the stream, which is the safe direction to fail.
func (e *Emitter) Emit(stream protocol.Stream, line string) protocol.LogFrame {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The guard inspects the raw line, because the delimiters it keys on are
	// themselves redacted by the always-on patterns.
	entering := !e.inPEM && pemBegin.MatchString(line)
	if entering {
		e.inPEM = true
		e.emitLocked(protocol.StreamAgent,
			"[latem] private key block detected in output — suppressing until it closes")
	}

	var out string
	if e.inPEM {
		// Suppress the whole line, not merely the parts that match something.
		out = Marker + fingerprint(line)
		if pemEnd.MatchString(line) {
			e.inPEM = false
		}
		return e.emitLocked(stream, out)
	}

	// Workflow commands (A42 phase 2). Checked after the PEM guard so a line
	// inside a suppressed key block can never issue one, and gated on Known
	// so that output which merely has the shape — a C++ diagnostic naming
	// std::vector — stays output.
	if e.resumesCommands(line) {
		e.stopToken = ""
		return e.emitLocked(protocol.StreamAgent, "[latem] workflow commands resumed")
	}
	if e.stopToken == "" {
		if cmd, ok := ghcmd.ParseLine(line); ok && cmd.Known() {
			return e.command(stream, cmd)
		}
	}

	return e.emitLocked(stream, e.red.Redact(line))
}

// EmitTruncated is [Emitter.Emit] with a note about bytes dropped to the line
// cap. The note is appended after redaction so it cannot be mangled by it.
func (e *Emitter) EmitTruncated(stream protocol.Stream, line string, dropped int) protocol.LogFrame {
	frame := e.Emit(stream, line)
	if dropped <= 0 {
		return frame
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emitLocked(protocol.StreamAgent,
		fmt.Sprintf("[latem] previous line truncated, %d bytes dropped", dropped))
}

// emitLocked assigns the next sequence number and fans out. Callers hold e.mu.
func (e *Emitter) emitLocked(stream protocol.Stream, line string) protocol.LogFrame {
	e.seq++
	frame := protocol.LogFrame{
		RunID:  e.runID,
		Seq:    e.seq,
		Stream: stream,
		At:     e.now(),
		Line:   line,
	}
	batch := []protocol.LogFrame{frame}
	for _, s := range e.sinks {
		// A failing sink must not stop the others, and must not fail the build.
		// Losing log lines is bad; losing a 90-minute iOS build because a disk
		// filled while writing its log is worse.
		_ = s.Write(batch)
	}
	return frame
}

// Note emits an agent-stream line. Used for step boundaries and teardown.
func (e *Emitter) Note(format string, args ...any) protocol.LogFrame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emitLocked(protocol.StreamAgent, "[latem] "+fmt.Sprintf(format, args...))
}

// Seq reports the highest sequence assigned so far.
func (e *Emitter) Seq() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

// Consume reads r to EOF, emitting each line on the given stream. This is the
// entry point an executor wires a step's stdout and stderr into.
func (e *Emitter) Consume(ctx context.Context, stream protocol.Stream, r io.Reader) error {
	return SplitLines(r, MaxLineBytes, func(line string, dropped int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.EmitTruncated(stream, line, dropped)
		return nil
	})
}

// Close closes every sink, returning the first error.
func (e *Emitter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var first error
	for _, s := range e.sinks {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
