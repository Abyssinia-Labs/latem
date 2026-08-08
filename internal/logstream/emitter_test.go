package logstream

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// captureSink records everything it receives.
type captureSink struct {
	mu     sync.Mutex
	frames []protocol.LogFrame
	err    error
	closed bool
}

func (c *captureSink) Write(frames []protocol.LogFrame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, frames...)
	return c.err
}

func (c *captureSink) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *captureSink) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.frames))
	for i, f := range c.frames {
		out[i] = f.Line
	}
	return out
}

func (c *captureSink) snapshot() []protocol.LogFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.LogFrame(nil), c.frames...)
}

// Sequence numbers are what make resumption after a partition non-duplicating.
// They must be per-run, monotonic, and gapless.
func TestEmitter_SequenceIsMonotonic(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	for i := 0; i < 10; i++ {
		e.Emit(protocol.StreamStdout, "line")
	}

	frames := sink.snapshot()
	if len(frames) != 10 {
		t.Fatalf("got %d frames", len(frames))
	}
	for i, f := range frames {
		if f.Seq != uint64(i+1) {
			t.Errorf("frame %d has seq %d, want %d", i, f.Seq, i+1)
		}
		if f.RunID != "run_1" {
			t.Errorf("frame %d has run id %q", i, f.RunID)
		}
		if f.At.IsZero() {
			t.Errorf("frame %d has no timestamp", i)
		}
	}
}

// Ordering matters more than throughput: a log whose lines arrive out of order
// is worse than a slightly slower one. Run under -race.
func TestEmitter_ConcurrentEmitKeepsSequenceGapless(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				e.Emit(protocol.StreamStdout, "concurrent")
			}
		}()
	}
	wg.Wait()

	frames := sink.snapshot()
	seen := make(map[uint64]bool, len(frames))
	for _, f := range frames {
		if seen[f.Seq] {
			t.Fatalf("duplicate sequence %d", f.Seq)
		}
		seen[f.Seq] = true
	}
	for i := uint64(1); i <= uint64(len(frames)); i++ {
		if !seen[i] {
			t.Fatalf("gap at sequence %d", i)
		}
	}
}

// A frame that exists has already been redacted. This is the property that
// makes it impossible to add a sink that bypasses the redactor (INV-4).
func TestEmitter_FramesReachSinksAlreadyRedacted(t *testing.T) {
	sink := &captureSink{}
	red := NewRedactor()
	red.Register([]byte("top-secret-token-value"))
	e := NewEmitter("run_1", red, sink)

	e.Emit(protocol.StreamStdout, "using top-secret-token-value now")

	for _, line := range sink.lines() {
		if strings.Contains(line, "top-secret-token-value") {
			t.Fatalf("unredacted secret reached a sink: %s", line)
		}
	}
}

// The private-key guard. Registration covers declared secrets and patterns
// cover recognisable one-line credentials; neither covers a build that prints
// a key file it found. Without the guard the header would be redacted and
// every body line emitted in the clear — a leak that looks handled.
func TestEmitter_PrivateKeyGuard(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink) // nothing registered

	for _, line := range strings.Split(pemFixture, "\n") {
		e.Emit(protocol.StreamStdout, line)
	}
	e.Emit(protocol.StreamStdout, "BUILD SUCCESSFUL")

	joined := strings.Join(sink.lines(), "\n")
	if strings.Contains(joined, "MIIEvQIBADANBgkqhkiG") {
		t.Fatalf("PEM body escaped the guard:\n%s", joined)
	}
	// Output after the block closes is unaffected.
	if !strings.Contains(joined, "BUILD SUCCESSFUL") {
		t.Error("guard did not disengage after END")
	}
	// And it says so, rather than silently blanking output.
	if !strings.Contains(joined, "private key block detected") {
		t.Error("guard engaged without a diagnostic note")
	}
}

// Deliberately narrow: certificates and public keys are not secrets and must
// pass through, or the guard would blank out ordinary TLS-related output.
func TestEmitter_GuardIgnoresCertificates(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	for _, line := range []string{
		"-----BEGIN CERTIFICATE-----",
		"MIIBkTCB+wIJAJ2s3lhVQ0000MA0GCSqGSIb3DQEBCwUAMBQxEjAQBgNVBAMMCWxv",
		"-----END CERTIFICATE-----",
	} {
		e.Emit(protocol.StreamStdout, line)
	}

	joined := strings.Join(sink.lines(), "\n")
	if !strings.Contains(joined, "MIIBkTCB+wIJAJ2s3lhVQ0000MA0G") {
		t.Errorf("certificate body was suppressed:\n%s", joined)
	}
}

// An unterminated block suppresses to the end of the stream. That is the safe
// direction to fail.
func TestEmitter_GuardFailsClosed(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	e.Emit(protocol.StreamStdout, "-----BEGIN PRIVATE KEY-----")
	e.Emit(protocol.StreamStdout, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj")
	e.Emit(protocol.StreamStdout, "still inside the block")

	joined := strings.Join(sink.lines(), "\n")
	if strings.Contains(joined, "still inside the block") {
		t.Errorf("guard released without an END delimiter:\n%s", joined)
	}
}

func TestEmitter_TruncationNote(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	e.EmitTruncated(protocol.StreamStdout, "kept", 1234)

	lines := sink.lines()
	if len(lines) != 2 {
		t.Fatalf("got %d frames, want the line plus a note", len(lines))
	}
	if !strings.Contains(lines[1], "1234 bytes dropped") {
		t.Errorf("note = %q", lines[1])
	}
	if sink.snapshot()[1].Stream != protocol.StreamAgent {
		t.Error("the note should be on the agent stream")
	}
}

// Losing log lines is bad; losing a 90-minute iOS build because a disk filled
// while writing its log is worse.
func TestEmitter_FailingSinkDoesNotStopTheOthers(t *testing.T) {
	bad := &captureSink{err: errSentinel}
	good := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), bad, good)

	e.Emit(protocol.StreamStdout, "one")
	e.Emit(protocol.StreamStdout, "two")

	if len(good.lines()) != 2 {
		t.Errorf("healthy sink got %d frames", len(good.lines()))
	}
}

func TestEmitter_Consume(t *testing.T) {
	sink := &captureSink{}
	red := NewRedactor()
	red.Register([]byte("swordfish-password"))
	e := NewEmitter("run_1", red, sink)

	input := "starting\npassword=swordfish-password\ndone\n"
	if err := e.Consume(context.Background(), protocol.StreamStdout, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}

	lines := sink.lines()
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %q", len(lines), lines)
	}
	if strings.Contains(strings.Join(lines, "\n"), "swordfish") {
		t.Error("secret survived the pipeline")
	}
	if lines[0] != "starting" || lines[2] != "done" {
		t.Errorf("surrounding lines damaged: %q", lines)
	}
}

func TestEmitter_ConsumeStopsOnCancelledContext(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := e.Consume(ctx, protocol.StreamStdout, strings.NewReader("a\nb\nc\n"))
	if err == nil {
		t.Error("expected cancellation to surface")
	}
}

func TestEmitter_NoteAndClose(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)

	e.Note("step %d of %d", 1, 3)
	frames := sink.snapshot()
	if len(frames) != 1 || frames[0].Stream != protocol.StreamAgent {
		t.Fatalf("got %+v", frames)
	}
	if frames[0].Line != "[latem] step 1 of 3" {
		t.Errorf("line = %q", frames[0].Line)
	}
	if e.Seq() != 1 {
		t.Errorf("seq = %d", e.Seq())
	}

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if !sink.closed {
		t.Error("sink was not closed")
	}
}

func TestEmitter_TimestampsAreInjectable(t *testing.T) {
	sink := &captureSink{}
	e := NewEmitter("run_1", NewRedactor(), sink)
	fixed := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return fixed }

	e.Emit(protocol.StreamStdout, "x")
	if got := sink.snapshot()[0].At; !got.Equal(fixed) {
		t.Errorf("At = %v, want %v", got, fixed)
	}
}
