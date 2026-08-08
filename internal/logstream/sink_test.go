package logstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func frames(lines ...string) []protocol.LogFrame {
	out := make([]protocol.LogFrame, len(lines))
	for i, l := range lines {
		out[i] = protocol.LogFrame{RunID: "run_1", Seq: uint64(i + 1), Line: l}
	}
	return out
}

func TestFileSink(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileSink(dir, "run_1")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Write(frames("one", "two")); err != nil {
		t.Fatal(err)
	}
	// Flushed per batch, so `tail -f` works during a long step rather than
	// only after it finishes.
	body, err := os.ReadFile(filepath.Join(dir, "run_1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one\ntwo\n" {
		t.Errorf("before close: %q", body)
	}

	if err := s.Write(frames("three")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	body, _ = os.ReadFile(filepath.Join(dir, "run_1.log"))
	if string(body) != "one\ntwo\nthree\n" {
		t.Errorf("after close: %q", body)
	}
}

// The log directory holds build output from a machine that also holds signing
// certificates. It is not world-readable.
func TestFileSink_Permissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	s, err := NewFileSink(dir, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "run_1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}

func TestRingSink_SnapshotWrapsAround(t *testing.T) {
	r := NewRingSink(3)

	if err := r.Write(frames("a", "b")); err != nil {
		t.Fatal(err)
	}
	if got := lines(r.Snapshot()); strings.Join(got, ",") != "a,b" {
		t.Errorf("partial ring = %v", got)
	}

	if err := r.Write(frames("c", "d", "e")); err != nil {
		t.Fatal(err)
	}
	if got := lines(r.Snapshot()); strings.Join(got, ",") != "c,d,e" {
		t.Errorf("wrapped ring = %v, want the three most recent", got)
	}
}

func TestRingSink_Subscribe(t *testing.T) {
	r := NewRingSink(10)
	ch, stop := r.Subscribe(4)
	defer stop()

	if err := r.Write(frames("hello")); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-ch:
		if f.Line != "hello" {
			t.Errorf("got %q", f.Line)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}

	stop()
	if _, open := <-ch; open {
		t.Error("channel should be closed after stop")
	}
}

// Someone running `latem logs -f` over a slow link must never be able to stall
// a build by reading slowly.
func TestRingSink_SlowSubscriberDoesNotBlock(t *testing.T) {
	r := NewRingSink(10)
	_, stop := r.Subscribe(1) // capacity 1, never drained
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_ = r.Write(frames("flood"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow subscriber blocked the pipeline")
	}
}

func TestBatchSink_FlushesOnCount(t *testing.T) {
	inner := &captureSink{}
	b := NewBatchSink(inner, 3, 0) // no ticker: deterministic

	for i := 0; i < 2; i++ {
		if err := b.Write(frames("x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(inner.lines()); got != 0 {
		t.Errorf("forwarded %d frames before reaching the batch size", got)
	}

	if err := b.Write(frames("x")); err != nil {
		t.Fatal(err)
	}
	if got := len(inner.lines()); got != 3 {
		t.Errorf("forwarded %d frames, want 3", got)
	}
}

func TestBatchSink_FlushesOnClose(t *testing.T) {
	inner := &captureSink{}
	b := NewBatchSink(inner, 100, 0)

	if err := b.Write(frames("straggler")); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if got := inner.lines(); len(got) != 1 || got[0] != "straggler" {
		t.Errorf("buffered frames were lost on close: %v", got)
	}
	if !inner.closed {
		t.Error("inner sink was not closed")
	}
}

func TestBatchSink_TickerFlushes(t *testing.T) {
	inner := &captureSink{}
	b := NewBatchSink(inner, 1000, 20*time.Millisecond)
	defer b.Close()

	if err := b.Write(frames("waiting")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if len(inner.lines()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("ticker never flushed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func lines(fs []protocol.LogFrame) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Line
	}
	return out
}
