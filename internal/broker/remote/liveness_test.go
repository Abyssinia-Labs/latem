package remote

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The tests of A98: a connection whose far side has gone without saying so is
// given up within two ping intervals, and one that is merely quiet is kept.
//
// They run on a real clock with a short interval, because what they test is
// time passing on a real socket: a ping that is or is not answered, a send
// buffer that does or does not fill. The reconnect loop's own wait is still the
// fake clock's, so a dial after a give-up costs nothing.

// testPingEvery is the ping interval these tests use. The silence window is
// twice it. Long enough that a loaded machine running the race detector answers
// a ping well inside it; short enough that the whole file takes seconds.
const testPingEvery = 100 * time.Millisecond

func silenceLine(every time.Duration) string {
	return fmt.Sprintf("no frame from the control plane in %s, reconnecting", 2*every)
}

func writeDeadlineLine(every time.Duration) string {
	return fmt.Sprintf("could not write a frame to the control plane in %s, reconnecting", 2*every)
}

// TestKeepalive_ABlackHoleIsGivenUpAndDialledAgain is LAT-14, as it happened on
// the desktop on 2026-11-13: the control plane accepted, the session worked,
// and then its far end went. Nothing came back — no frame, no pong, no close,
// no reset — and the agent's writes went on succeeding into its own send
// buffer. Before A98 the agent sat on that socket until TCP gave up a quarter
// of an hour later, and this test waited for a second handshake until it timed
// out.
func TestKeepalive_ABlackHoleIsGivenUpAndDialledAgain(t *testing.T) {
	h := newHarness(t, func(o *harnessOptions) { o.pingEvery = testPingEvery })
	h.AwaitConnected(t)
	h.AwaitResumes(t, 1)

	// The connection reads this heartbeat and nothing after it.
	h.CP.BlackHoleNext(0)
	if err := h.Heartbeat(t.Context(), protocol.MachineHealth{MachineID: "mch_test"}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	h.CP.Await(t, "the control plane to go silent", func() bool { return h.CP.Stalls() == 1 })
	silentFrom := time.Now()

	// The writes still succeed: this is the part the kernel hides. A heartbeat
	// into a black hole is a few hundred bytes into a send buffer, and nothing
	// on this side can tell it from one that arrived.
	if err := h.Heartbeat(t.Context(), protocol.MachineHealth{MachineID: "mch_test"}); err != nil {
		t.Fatalf("a heartbeat into the black hole returned %v; the write should have looked like a success", err)
	}

	h.CP.Await(t, "the agent to give the connection up and dial again", func() bool { return h.CP.Handshakes() >= 2 })
	took := time.Since(silentFrom)

	// The last thing heard was a pong at most one interval before the silence,
	// so the deadline lands between one and two intervals into it. Far more
	// than two means the deadline is not a deadline; far less, that one missed
	// pong was enough to end a connection.
	if took > 10*testPingEvery {
		t.Errorf("the black hole was given up %s after it began; the silence window is %s", took, 2*testPingEvery)
	}
	if took < testPingEvery/2 {
		t.Errorf("the black hole was given up %s after it began, before a ping could have gone unanswered", took)
	}

	// Said once, in words that name the deadline, and not narrated a second time
	// as a lost connection with "use of closed network connection" for a reason.
	h.CP.Await(t, "the give-up to be logged", func() bool { return h.Narration.Count(silenceLine(testPingEvery)) == 1 })
	if n := h.Narration.Count("control plane connection lost"); n != 0 {
		t.Errorf("the give-up was also logged as %d lost connection(s); the reason should be said once", n)
	}

	// The ordinary reconnect: the backoff from its first step, since the
	// session had got as far as its resume frame.
	if got := h.Clock.Schedule(); len(got) == 0 || got[0] != DefaultBackoff.Base {
		t.Errorf("the reconnect waited %v; want the backoff's first step, %s", got, DefaultBackoff.Base)
	}

	// And the new connection is a session: resumed, and the health it last had
	// published at once rather than at the next beat.
	h.AwaitConnected(t)
	h.AwaitResumes(t, 2)
	h.CP.Await(t, "the heartbeat to reach the control plane again", func() bool { return len(h.CP.Heartbeats()) >= 2 })
}

// TestKeepalive_AQuietLinkThatAnswersPingsIsKept is the other half: silence
// alone proves nothing. A healthy control plane says nothing to an idle
// machine for hours, since heartbeats are not answered (PROTOCOL.md §4.5), and
// the pongs are all that keep the deadline moving.
func TestKeepalive_AQuietLinkThatAnswersPingsIsKept(t *testing.T) {
	h := newHarness(t, func(o *harnessOptions) { o.pingEvery = testPingEvery })
	h.AwaitConnected(t)

	const intervals = 12
	time.Sleep(intervals * testPingEvery)

	if n := h.CP.Handshakes(); n != 1 {
		t.Errorf("%d handshakes over %d quiet intervals; a link that answers its pings must be kept", n, intervals)
	}
	if !h.Connected() {
		t.Error("a quiet link that answers its pings is not connected")
	}
	if n := h.Narration.Count("reconnecting"); n != 0 {
		t.Errorf("a healthy quiet link was given up %d time(s)", n)
	}
	// About one a interval. Too few and the deadline above was never at risk,
	// so the test proved nothing; too many and the loop is spinning.
	if p := h.CP.Pings(); p < intervals/3 || p > 3*intervals {
		t.Errorf("%d pings answered over %d intervals; want about one an interval", p, intervals)
	}
}

// TestKeepalive_AControlPlaneThatStillSendsIsKeptUntilAFrameCannotBeWritten
// is a far side that stopped reading, and so stopped answering pings, while
// it still sends. Its frames are proof the link carries something, so it is
// kept: any frame extends the deadline, not only a pong, which is what keeps a
// link busy with a log flush from being ended while its own ping waits behind
// megabytes of output. What ends this one is the write deadline: once the
// agent has more to say than the socket's buffers hold, a write blocks, and a
// frame that cannot go out in the silence window ends the connection.
func TestKeepalive_AControlPlaneThatStillSendsIsKeptUntilAFrameCannotBeWritten(t *testing.T) {
	h := newHarness(t, func(o *harnessOptions) {
		o.pingEvery = testPingEvery
		// Room for everything the flood below can write before a write blocks.
		o.maxBuf = 1 << 30
	})
	const runID = "r_flood"
	seedRun(t, h.DB, runID, protocol.StateRunning)
	h.AwaitConnected(t)

	h.CP.BlackHoleNext(testPingEvery / 4)
	if err := h.Heartbeat(t.Context(), protocol.MachineHealth{MachineID: "mch_test"}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	h.CP.Await(t, "the control plane to stop reading", func() bool { return h.CP.Stalls() == 1 })

	// No pong for several silence windows, and the link is kept: the far side
	// is still talking.
	time.Sleep(6 * testPingEvery)
	if n := h.CP.Handshakes(); n != 1 {
		t.Fatalf("%d handshakes while the control plane was still sending; any frame extends the deadline, not only a pong", n)
	}

	// Now the agent says more than any send buffer holds. A megabyte a write,
	// up to a ceiling no loopback socket's buffers come near.
	sink, err := h.Logs(t.Context(), runID)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	line := strings.Repeat("x", 4<<10)
	const perWrite = 256
	var seq uint64 = 1
	for h.Narration.Count(writeDeadlineLine(testPingEvery)) == 0 {
		if seq > 256*perWrite {
			t.Fatalf("wrote %d MB to a control plane that reads nothing and no write blocked", (seq-1)*uint64(len(line))>>20)
		}
		writeFrames(t, sink, runID, seq, seq+perWrite-1, line)
		seq += perWrite
	}

	h.CP.Await(t, "the agent to dial again", func() bool { return h.CP.Handshakes() >= 2 })
	if n := h.Narration.Count(silenceLine(testPingEvery)); n != 0 {
		t.Errorf("the connection was given up for silence; its control plane was sending throughout, and the write deadline should have ended it")
	}
	if n := h.Narration.Count(writeDeadlineLine(testPingEvery)); n != 1 {
		t.Errorf("the write deadline was said %d times; want once", n)
	}
}
