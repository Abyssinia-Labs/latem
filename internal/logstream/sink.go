package logstream

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// FileSink writes a run's log to ~/.latem/logs/<run-id>.log.
//
// Plain text, one line per frame, no structure. The file is the human artifact
// — the thing you `tail -f` on a machine you own — and every metadata field is
// available in the frame stream for anything that needs it. Encoding it as
// JSONL would make the common case worse to serve a case the frames already
// cover.
type FileSink struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// NewFileSink creates or truncates the log for a run.
//
// The run id is validated here as well as at the trust boundary
// ([protocol.ValidateID]). That is deliberate duplication: run ids arrive from
// a control plane, this function turns one into a filesystem path, and a
// single validation point that every other package quietly trusts is the shape
// of the next incident. The cost is one regexp per run.
func NewFileSink(dir, runID string) (*FileSink, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return nil, fmt.Errorf("logstream: refusing to open a log for an invalid run id: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logstream: creating %s: %w", dir, err)
	}

	path := filepath.Join(dir, runID+".log")

	// Belt and braces: whatever the id was, the result must sit directly in
	// dir. This catches a future id policy that permits something clever.
	if filepath.Dir(path) != filepath.Clean(dir) {
		return nil, fmt.Errorf("logstream: run id %q escapes the log directory", runID)
	}

	// #nosec G304 -- path is filepath.Join(dir, <validated id>+".log"); the id
	// is checked against protocol.ValidateID above and the result is asserted
	// to remain directly inside dir.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logstream: opening %s: %w", path, err)
	}
	return &FileSink{f: f, w: bufio.NewWriterSize(f, 32*1024)}, nil
}

func (s *FileSink) Write(frames []protocol.LogFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fr := range frames {
		if _, err := s.w.WriteString(fr.Line); err != nil {
			return err
		}
		if err := s.w.WriteByte('\n'); err != nil {
			return err
		}
	}
	// Flushing per batch keeps `tail -f` responsive during a long step. The
	// writer is still buffered within a batch, so this is one syscall per
	// batch rather than one per line.
	return s.w.Flush()
}

func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}

// RingSink keeps the most recent frames in memory and broadcasts to
// subscribers, which is what backs `latem logs -f`.
type RingSink struct {
	mu   sync.Mutex
	buf  []protocol.LogFrame
	next int
	full bool
	subs map[int]chan protocol.LogFrame
	id   int
}

// DefaultRingSize is the number of lines a late subscriber can catch up on
// (RUNNER_SPEC §16).
const DefaultRingSize = 1000

// NewRingSink returns a ring holding size frames.
func NewRingSink(size int) *RingSink {
	if size <= 0 {
		size = DefaultRingSize
	}
	return &RingSink{
		buf:  make([]protocol.LogFrame, size),
		subs: map[int]chan protocol.LogFrame{},
	}
}

func (s *RingSink) Write(frames []protocol.LogFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fr := range frames {
		s.buf[s.next] = fr
		s.next = (s.next + 1) % len(s.buf)
		if s.next == 0 {
			s.full = true
		}
		for _, ch := range s.subs {
			// Non-blocking. A subscriber that cannot keep up loses lines; it
			// must never be able to stall a build by reading slowly, and
			// someone running `latem logs -f` over a slow link is exactly the
			// case that would.
			select {
			case ch <- fr:
			default:
			}
		}
	}
	return nil
}

// Snapshot returns the buffered frames oldest-first.
func (s *RingSink) Snapshot() []protocol.LogFrame {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.full {
		out := make([]protocol.LogFrame, s.next)
		copy(out, s.buf[:s.next])
		return out
	}
	out := make([]protocol.LogFrame, 0, len(s.buf))
	out = append(out, s.buf[s.next:]...)
	return append(out, s.buf[:s.next]...)
}

// Subscribe returns a channel of subsequent frames and a function to stop.
func (s *RingSink) Subscribe(buffer int) (<-chan protocol.LogFrame, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.id++
	id := s.id
	ch := make(chan protocol.LogFrame, buffer)
	s.subs[id] = ch

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

func (s *RingSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	return nil
}

// Batching defaults from RUNNER_SPEC §16.
const (
	DefaultBatchFrames = 100
	DefaultBatchDelay  = 250 * time.Millisecond
)

// BatchSink buffers frames and forwards them in batches, which is what keeps a
// remote broker from receiving one WebSocket message per log line.
type BatchSink struct {
	inner  Sink
	max    int
	mu     sync.Mutex
	buf    []protocol.LogFrame
	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewBatchSink wraps inner. A delay of zero disables the background flush, so
// tests can drive batching deterministically with [BatchSink.Flush].
func NewBatchSink(inner Sink, maxFrames int, delay time.Duration) *BatchSink {
	if maxFrames <= 0 {
		maxFrames = DefaultBatchFrames
	}
	b := &BatchSink{inner: inner, max: maxFrames, done: make(chan struct{})}

	if delay > 0 {
		b.ticker = time.NewTicker(delay)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for {
				select {
				case <-b.ticker.C:
					_ = b.Flush()
				case <-b.done:
					return
				}
			}
		}()
	}
	return b
}

func (b *BatchSink) Write(frames []protocol.LogFrame) error {
	b.mu.Lock()
	b.buf = append(b.buf, frames...)
	ready := len(b.buf) >= b.max
	b.mu.Unlock()

	if ready {
		return b.Flush()
	}
	return nil
}

// Flush forwards whatever is buffered.
func (b *BatchSink) Flush() error {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	batch := b.buf
	b.buf = nil
	b.mu.Unlock()

	return b.inner.Write(batch)
}

func (b *BatchSink) Close() error {
	if b.ticker != nil {
		b.ticker.Stop()
		close(b.done)
		b.wg.Wait()
	}
	err := b.Flush()
	if cerr := b.inner.Close(); err == nil {
		err = cerr
	}
	return err
}
