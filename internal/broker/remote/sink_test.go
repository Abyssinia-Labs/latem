package remote

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestSink_BufferBoundDropsAndSaysSo is §7.4's 50 MB rule.
//
// The property that matters is not that frames are dropped — anything with a
// bound drops — but that the gap is *visible*. Sequence numbers are assigned
// upstream by the emitter (§16) and keep incrementing whether or not this
// package can keep up, so the control plane sees the stream jump; the marker
// frame is what turns that jump from "the agent is broken" into "14,000 lines
// went missing here and the full log is on the machine".
//
// A buffer that silently swallowed the overflow would pass every other test in
// this file.
func TestSink_BufferBoundDropsAndSaysSo(t *testing.T) {
	const runID = "r_flood"
	// A small bound and large lines make the arithmetic obvious: each frame is
	// ~1 KB of line plus overhead, so 8 KB of buffer holds a handful.
	const bound = 8 << 10

	h := newHarness(t, func(o *harnessOptions) { o.maxBuf = bound })
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)

	// The partition comes first, so nothing can drain while the buffer fills.
	h.CP.Partition()
	h.CP.Await(t, "the broker to notice the partition", func() bool { return !h.Connected() })

	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	line := strings.Repeat("x", 1024)
	const total = uint64(200)
	writeFrames(t, sink, runID, 1, total, line)

	s := h.sinkFor(runID)
	if s == nil {
		t.Fatal("the broker lost track of the sink")
	}
	s.mu.Lock()
	buffered, bufBytes, dropped, dropAt := len(s.buf), s.bufBytes, s.dropped, s.dropAt
	s.mu.Unlock()

	if bufBytes > bound {
		t.Errorf("the buffer holds %d bytes, past its %d byte bound — the whole point of the bound is that a partition cannot grow it", bufBytes, bound)
	}
	if dropped == 0 {
		t.Fatalf("nothing was dropped after %d frames into an %d byte buffer; the bound is not being applied", total, bound)
	}
	if uint64(buffered)+dropped != total {
		t.Errorf("%d frames buffered plus %d dropped is not the %d written; some went nowhere and were not counted",
			buffered, dropped, total)
	}
	if dropAt != uint64(buffered)+1 {
		t.Errorf("the first dropped sequence is recorded as %d; %d frames were kept, so it should be %d",
			dropAt, buffered, buffered+1)
	}

	// Heal, and the marker must appear in the stream at the sequence it stands
	// in for — the frames around it contiguous, the hole explicit.
	h.CP.Heal()
	h.CP.Await(t, "the buffered frames to reach the control plane", func() bool {
		return len(h.CP.Frames(runID)) >= buffered
	})
	// The marker is the last thing the flush writes, so give it a moment to
	// arrive before concluding it never will. Its absence is the failure this
	// test exists for, and it deserves a message rather than a timeout.
	h.CP.Await(t, "the drop marker", func() bool {
		for _, f := range h.CP.Frames(runID) {
			if f.Stream == protocol.StreamAgent {
				return true
			}
		}
		return h.CP.Duplicates(runID) > 0 // any further flush would have carried it
	})

	got := h.CP.Frames(runID)
	var marker *protocol.LogFrame
	for i := range got {
		if got[i].Stream == protocol.StreamAgent {
			marker = &got[i]
		}
	}
	if marker == nil {
		t.Fatal("no marker frame reached the control plane; the gap is silent, which is the failure RUNNER_SPEC §7.4 names")
	}
	if marker.Seq != dropAt {
		t.Errorf("the marker is at seq %d, want %d — it should stand exactly where the first dropped line was", marker.Seq, dropAt)
	}
	if !strings.Contains(marker.Line, "dropped") {
		t.Errorf("the marker does not say lines were dropped: %q", marker.Line)
	}
	// The count has to be in it. "Some lines are missing" is not actionable;
	// "14,000 lines are missing" tells an operator whether to go and read the
	// file on the machine.
	if !strings.Contains(marker.Line, itoa(dropped)) {
		t.Errorf("the marker does not say how many lines were lost (%d): %q", dropped, marker.Line)
	}

	// Sequence numbers must still be monotonic across the gap.
	var last uint64
	for _, f := range got {
		if f.Seq <= last {
			t.Fatalf("sequence went backwards at %d after %d; the gap must be visible, not confusing", f.Seq, last)
		}
		last = f.Seq
	}
}

// TestSink_OnFailureShipsOnlyWhatFailed is §7.4's remote_logs mode.
//
// on_failure exists for verbose Gradle output, and it is worth exactly nothing
// if a succeeded run's buffer is shipped anyway. Both halves are asserted,
// because getting only one right is the easy mistake: buffering and then always
// flushing looks identical to always-mode from a distance.
func TestSink_OnFailureShipsOnlyWhatFailed(t *testing.T) {
	tests := map[string]struct {
		state    protocol.RunState
		wantShip bool
		why      string
	}{
		"succeeded ships nothing": {
			protocol.StateSucceeded, false,
			"a green build's log is the one nobody reads, and shipping it is the bandwidth on_failure exists to save",
		},
		"failed ships everything": {
			protocol.StateFailed, true,
			"the failing build's output is the entire reason anyone configured a control plane",
		},
		"timed_out ships everything": {
			protocol.StateTimedOut, true,
			"a build that outlasted its wall clock is one somebody has to diagnose",
		},
		"cancelled ships everything": {
			protocol.StateCancelled, true,
			"somebody pressed Stop and the first question is what it was doing",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const runID = "r_onfailure"
			h := newHarness(t, func(o *harnessOptions) { o.logMode = LogsOnFailure })
			seedRun(t, h.DB, runID, protocol.StateRunning)
			h.AwaitConnected(t)
			h.AwaitResumes(t, 1)

			sink, err := h.Logs(t.Context(), runID)
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			writeFrames(t, sink, runID, 1, 20, "gradle")

			// Nothing may be on the wire yet, whatever the connection is doing.
			if n := len(h.CP.Frames(runID)); n != 0 {
				t.Fatalf("on_failure shipped %d frames while the run was still going", n)
			}
			if got := sink.Acked(); got != 0 {
				t.Errorf("Acked = %d before anything was sent", got)
			}

			finished := time.Now().UTC()
			if err := h.Report(t.Context(), runID, protocol.Update{State: tc.state, FinishedAt: &finished}); err != nil {
				t.Fatalf("Report: %v", err)
			}
			_ = sink.Close(t.Context())

			if tc.wantShip {
				h.CP.Await(t, "the buffered log to be shipped", func() bool { return len(h.CP.Frames(runID)) >= 20 })
				assertContiguous(t, h.CP.Frames(runID), seqRange(1, 20), runID)
			} else {
				// There is no event to wait for when the correct behaviour is
				// silence, so this gives the wrong behaviour a fair chance to
				// happen before concluding it did not.
				deadline := time.After(300 * time.Millisecond)
				for {
					if n := len(h.CP.Frames(runID)); n != 0 {
						t.Fatalf("%d frames were shipped for a %s run; %s", n, tc.state, tc.why)
					}
					select {
					case <-deadline:
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
		})
	}
}

// TestSink_RefusesFramesForAnotherRun mirrors the local sink's rule.
//
// Sequences are per run, so a stray frame would advance the wrong resume point
// — silently, and in the direction that loses log lines rather than duplicating
// them.
func TestSink_RefusesFramesForAnotherRun(t *testing.T) {
	h := newHarness(t, nil)
	seedRun(t, h.DB, "r_mine", protocol.StateRunning)
	h.AwaitConnected(t)

	sink, err := h.Logs(t.Context(), "r_mine")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	err = sink.Write(t.Context(), []protocol.LogFrame{{RunID: "r_theirs", Seq: 1, Line: "x"}})
	errorContains(t, err, "r_theirs")
}

// TestSink_AckedIsWhatTheFarSideTookAndNotWhatWasSent is the distinction
// finding F6 turns on.
//
// The agent persists Acked() and resumes from Acked()+1. A sink that reported
// what it had written would make the next reconnect skip precisely the frames
// that did not make it — which is worse than duplicating them, because a
// duplicate is visible and a hole is not.
func TestSink_AckedIsWhatTheFarSideTookAndNotWhatWasSent(t *testing.T) {
	const runID = "r_acked"
	h := newHarness(t, nil)
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)

	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	h.CP.WithholdLogAcks(true)
	writeFrames(t, sink, runID, 1, 10, "unacknowledged")
	h.CP.Await(t, "the frames to arrive", func() bool { return len(h.CP.Frames(runID)) >= 10 })

	if got := sink.Acked(); got != 0 {
		t.Fatalf("Acked = %d with every acknowledgement withheld; the frames were written, not accepted", got)
	}

	h.CP.WithholdLogAcks(false)
	writeFrames(t, sink, runID, 11, 12, "acknowledged")
	h.CP.Await(t, "the resume point to advance", func() bool { return sink.Acked() >= 12 })

	// And the resume point is durable, not merely remembered: an agent that
	// restarts has to pick up where the far side actually is.
	//
	// Awaited rather than read the instant Acked() moves. [sink.ack] releases
	// the lock and *then* calls persist, so the counter is briefly ahead of the
	// row by design — the alternative is a database write on the hot path under
	// a mutex, and the skew is in the safe direction because resuming from a
	// stale point re-sends frames rather than skipping them. Reading the row at
	// the moment the counter moves therefore measures that window instead of
	// the durability this asserts, which is why it passed on a workstation and
	// failed on a loaded CI runner.
	h.CP.Await(t, "the resume point to reach the store, without which a restart would not resume where the far side actually is", func() bool {
		seq, err := h.DB.LastAckedSeq(t.Context(), store.BrokerRemote, runID)
		if err != nil {
			t.Fatalf("LastAckedSeq: %v", err)
		}
		return seq >= 12
	})
	// Under this transport's key, and not the local one's. A sink that wrote to
	// the shared column would make a local broker on the same machine resume
	// from what a network happened to acknowledge (amendment A28).
	if local, lerr := h.DB.LastAckedSeq(t.Context(), store.BrokerLocal, runID); lerr != nil || local != 0 {
		t.Errorf("last_acked_seq(local) = %d, %v; want 0 — this transport wrote to a key it does not own", local, lerr)
	}
}

// TestSink_OneSinkPerRun guards against two ack streams racing one resume
// point. The lower of the two would win at the next reconnect, losing exactly
// the frames covering the outage.
func TestSink_OneSinkPerRun(t *testing.T) {
	h := newHarness(t, nil)
	seedRun(t, h.DB, "r_one", protocol.StateRunning)
	h.AwaitConnected(t)

	a, err := h.Logs(t.Context(), "r_one")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	b, err := h.Logs(t.Context(), "r_one")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if a != b {
		t.Error("two calls to Logs for one run returned different sinks")
	}
}

// TestSink_RejectsUnknownRunAndBadIDs keeps a traversal id from becoming a
// buffer keyed on a path (INV-3, amendment A6), and keeps a sink for a run that
// does not exist from failing halfway through a build instead of at the start.
func TestSink_RejectsUnknownRunAndBadIDs(t *testing.T) {
	h := newHarness(t, nil)
	h.AwaitConnected(t)

	if _, err := h.Logs(t.Context(), "../../../../tmp/x"); err == nil {
		t.Error("Logs accepted a path traversal as a run id")
	}
	if _, err := h.Logs(t.Context(), "r_never_created"); err == nil {
		t.Error("Logs accepted a run that does not exist; the failure belongs here, not on the first batch of output")
	}
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestSink_ConcurrentFlushesStayInOrder.
//
// nextBatch is atomic, so two flushes carve the buffer into two correct,
// disjoint batches — and then race to write them. Nothing is lost and nothing is
// duplicated, so every count-based assertion passes; what changes is the order
// the frames reach the wire, and a server that appends in arrival order shows
// the build's output shuffled. The region it shuffles is the one a reconnect
// just recovered, which is the region anybody is reading.
//
// The two flushers are not hypothetical, and this is their exact shape:
// [Broker.flushSinks] runs on the connect goroutine while the run it is
// recovering is still writing into the same sink. That is what happens after
// every partition.
func TestSink_ConcurrentFlushesStayInOrder(t *testing.T) {
	const runID = "r_ordered"
	const batches = 40
	const perBatch = uint64(10)
	const total = uint64(batches) * perBatch

	h := newHarness(t, nil)
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)

	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	s := h.sinkFor(runID)

	done := make(chan struct{})
	var wg sync.WaitGroup

	// The reconnect's flusher: it keeps offering whatever is unsent, in parallel
	// with the build still producing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			s.flush(t.Context())
		}
	}()

	for i := range uint64(batches) {
		writeFrames(t, sink, runID, i*perBatch+1, (i+1)*perBatch, "streaming")
	}
	h.CP.Await(t, "every frame to be acknowledged", func() bool { return sink.Acked() >= total })
	close(done)
	wg.Wait()

	got := h.CP.Frames(runID)
	var last uint64
	for i, f := range got {
		if f.Seq <= last {
			t.Fatalf("frame %d arrived as seq %d after seq %d; concurrent flushes put the batches on the wire out of order, so a control plane that appends in arrival order shows the build shuffled",
				i, f.Seq, last)
		}
		last = f.Seq
	}
	if last != total {
		t.Errorf("the control plane's last frame is seq %d, want %d", last, total)
	}
}
