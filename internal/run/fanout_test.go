package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"io"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The orchestrator held exactly one broker until amendment A28, and could not
// hold more: §22 gave the machine one reported_state and one last_acked_seq, so
// a second transport reading either would have been told what the first had
// written. These tests are the fan-out that became possible, and each one is
// written against the way it would go wrong rather than the way it goes right.

// TestFanOut_EveryBrokerGetsEveryTransition.
//
// A machine that accepted a run from a control plane and reported it only
// locally would leave that run showing as queued forever, which is the reason
// the agent did not consume remote commands at all before this change.
func TestFanOut_EveryBrokerGetsEveryTransition(t *testing.T) {
	h := newHarness(t)
	second := &fakeBroker{name: "second"}
	h.opts.Brokers = []broker.Broker{h.broker, second}
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	_ = o.Close()

	first := h.broker.states("run1")
	other := second.states("run1")
	if len(first) == 0 {
		t.Fatal("the first broker was told nothing at all")
	}
	if !slices.Equal(first, other) {
		t.Errorf("the two brokers were told different stories:\n first  = %v\n second = %v",
			fmtStates(first), fmtStates(other))
	}
	if !slices.Contains(other, protocol.StateSucceeded) {
		t.Error("the second broker never learned the run finished")
	}
}

// TestFanOut_EveryBrokerGetsEveryLogFrame.
//
// Each transport gets its own [broker.LogSink] and its own resume point, which
// is what makes them independent. What must not differ is the frames: they are
// sequenced once, before fan-out (§16), so two sinks disagreeing about the
// stream would mean the numbering had stopped being machine-wide.
func TestFanOut_EveryBrokerGetsEveryLogFrame(t *testing.T) {
	h := newHarness(t)
	second := &fakeBroker{name: "second"}
	h.opts.Brokers = []broker.Broker{h.broker, second}
	o := h.build()

	h.run(o, h.request("test"))
	_ = o.Close()

	firstSeqs := seqsOf(h.broker.lines())
	otherSeqs := seqsOf(second.lines())
	if len(firstSeqs) == 0 {
		t.Fatal("no frames reached the first broker")
	}
	if !slices.Equal(firstSeqs, otherSeqs) {
		t.Errorf("the two brokers received different frames: %d vs %d, first divergence at %v",
			len(firstSeqs), len(otherSeqs), firstDiff(firstSeqs, otherSeqs))
	}
}

// TestFanOut_AFailingBrokerDoesNotFailTheRun is the product promise stated as a
// test: the machine keeps building when the hosted layer is unreachable.
//
// This broker fails in all three ways at once — it will not open a log sink, it
// refuses every report, and it errors on close. None of that may reach the run,
// and none of it may reach the other transport.
func TestFanOut_AFailingBrokerDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t)
	broken := &fakeBroker{name: "broken", logsErr: errors.New("the control plane is unreachable"), failNext: 1 << 30}
	h.opts.Brokers = []broker.Broker{h.broker, broken}
	h.opts.ReportAttempts = 1
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s: %s); an unreachable transport must cost a build nothing",
			res.State, res.ErrorCode, res.ErrorMsg)
	}
	_ = o.Close()

	if len(broken.updates()) != 0 {
		t.Error("the broken broker recorded a report it rejected")
	}
	if !slices.Contains(h.broker.states("run1"), protocol.StateSucceeded) {
		t.Error("the working transport lost the run's outcome to the broken one")
	}
	if len(h.broker.lines()) == 0 {
		t.Error("the working transport received no log frames")
	}
	if !strings.Contains(h.logContents("run1"), "broker log sink unavailable") {
		t.Error("the degradation was not narrated into the run's own log")
	}
}

// TestFanOut_OneBrokerCannotStallAnother is why the reporter runs a goroutine
// per (broker, run) rather than publishing to both from one.
//
// The remote transport retries five times with doubling backoff against a
// control plane that may be mid-partition. Publishing to both from one goroutine
// would put that delay in front of the local one — the transport `latem run
// --wait` and the whole local product depend on — and the symptom would be a
// machine that feels slow exactly when the network is bad.
//
// The stall here is unbounded rather than long, so this is a deadlock test and
// not a timing test: with a shared goroutine the working broker receives
// nothing, ever, and the test times out rather than flaking.
func TestFanOut_OneBrokerCannotStallAnother(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	h := newHarness(t)
	stalled := &fakeBroker{name: "stalled", block: release}
	h.opts.Brokers = []broker.Broker{stalled, h.broker}
	o := h.build()

	go func() { _, _ = o.Execute(context.Background(), h.admitAndClaim(o, h.request("test"))) }()

	waitFor(t, "the working broker to be told the run finished while the other is stuck", func() bool {
		return slices.Contains(h.broker.states("run1"), protocol.StateSucceeded)
	})
	if len(stalled.updates()) != 0 {
		t.Error("the stalled broker recorded a report it never returned from")
	}
}

// TestFanOut_NoBrokersIsLocalOnlyOperation. §7.3 has the agent doing useful work
// with no control plane at all, and a nil entry in the list — which is what a
// caller passes for a transport that is configured off — must not be a nil call.
func TestFanOut_NoBrokersIsLocalOnlyOperation(t *testing.T) {
	h := newHarness(t)
	h.brokerless = true
	h.opts.Brokers = []broker.Broker{nil, nil}
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s (%s)", res.State, res.ErrorMsg)
	}
	if got := mustGet(t, h.db, "run1").State; got != protocol.StateSucceeded {
		t.Errorf("stored state = %s; local truth does not depend on a broker", got)
	}
}

// TestAlwaysRedact_ReachesEveryRunsRedactor is §16's machine-token half at this
// layer. The value is registered before the pipeline exists, which is the
// ordering INV-4 turns on: registered before the first step, and before the
// value is used for anything else.
func TestAlwaysRedact_ReachesEveryRunsRedactor(t *testing.T) {
	const token = "tok-machine-42"

	h := newHarness(t)
	h.opts.AlwaysRedact = [][]byte{[]byte(token)}
	h.env.exec = func(_ context.Context, _ executor.Step, stdout, _ io.Writer) (int, error) {
		_, _ = io.WriteString(stdout, "authorization: bearer "+token+"\n")
		return 0, nil
	}
	o := h.build()

	h.run(o, h.request("test"))

	if strings.Contains(h.logContents("run1"), token) {
		t.Errorf("a value passed in AlwaysRedact appeared verbatim in the run's log")
	}
	if !strings.Contains(h.logContents("run1"), logstream.Marker) {
		t.Errorf("nothing was redacted at all; the line was lost rather than scrubbed:\n%s", h.logContents("run1"))
	}
}

func seqsOf(frames []protocol.LogFrame) []uint64 {
	out := make([]uint64, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Seq)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func firstDiff(a, b []uint64) any {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i]
		}
	}
	return "the shorter stream simply stops"
}
