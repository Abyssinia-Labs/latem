package local

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// sink is the local [broker.LogSink].
//
// # What it does not do
//
// It does not buffer, spool, retry, or transmit. Finding F6 replaced an
// io.WriteCloser with this interface because a remote broker has to survive a
// partition without duplicating the output that covers it — and a local broker
// has no partition to survive. The frames are already on this machine: the
// emitter fans every one out to logstream.FileSink, which writes
// ~/.latem/logs/<run-id>.log, and `latem logs -f` tails that file. A second
// copy inside the broker would be a second answer to "what did this build
// print", which is worse than no answer.
//
// # So Acked is the whole implementation
//
// [broker.LogSink.Acked] is the agent's resume point: it persists the value and
// resumes from Acked()+1. Locally, "durably accepted" means written to the
// runs table, so each batch advances last_acked_seq and Acked returns what the
// database took. That is not a shortcut around the interface — it is the
// interface answered honestly for a transport where the far side is the same
// disk. Returning the highest sequence *seen* instead, without the write,
// would claim durability the restart path does not have.
type sink struct {
	st    Store
	runID string

	mu     sync.Mutex
	acked  uint64
	closed bool
}

var _ broker.LogSink = (*sink)(nil)

// newSink starts a sink at the run's persisted resume point, so a sink opened
// after a restart does not claim an ack of zero for a run whose log the store
// already remembers.
func newSink(st Store, runID string, acked uint64) *sink {
	return &sink{st: st, runID: runID, acked: acked}
}

// Write records that a batch of frames is durable.
//
// Frames belonging to another run are refused. The interface hands out one
// sink per run and sequences are per-run, so a frame from elsewhere would
// advance the wrong resume point — silently, and in the direction that loses
// log lines rather than duplicating them.
func (s *sink) Write(ctx context.Context, frames []protocol.LogFrame) error {
	if len(frames) == 0 {
		return nil
	}

	var highest uint64
	for _, f := range frames {
		if f.RunID != s.runID {
			return fmt.Errorf("local: sink for run %s was handed a frame for run %s", s.runID, f.RunID)
		}
		if f.Seq > highest {
			highest = f.Seq
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("local: write to a closed log sink")
	}
	if highest <= s.acked {
		// A replayed batch. The store's write is already monotonic, so this is
		// only avoiding a pointless UPDATE, not preventing a rewind.
		return nil
	}

	if err := s.st.SetLastAckedSeq(ctx, s.runID, highest); err != nil {
		return err
	}
	s.acked = highest
	return nil
}

// Acked reports the highest sequence the store has taken for this run.
func (s *sink) Acked() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// Close makes further writes an error. It is idempotent: shutdown paths close
// things twice and a second Close reporting a failure would mask the first
// error that mattered.
func (s *sink) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
