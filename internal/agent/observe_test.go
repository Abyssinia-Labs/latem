package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The daily mirror gc must skip a mirror a live run is reading.
//
// Mirrors have gc.auto disabled (§12) precisely so nothing collects the commit
// a build is checking out after a force-push; running an explicit gc under one
// would undo that, and the symptom — a build failing on a missing object —
// points nowhere near a ticker that ran at 3am.
func TestMirrorBusyTracksLiveRuns(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	a, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}

	appMirror := cfg.Paths.MirrorDir("app")
	if a.mirrorBusy(appMirror) {
		t.Error("no run is live, so no mirror is busy")
	}

	a.noteActive("r_1", "app")
	if !a.mirrorBusy(appMirror) {
		t.Error("a live run reading app should mark its mirror busy")
	}
	if a.mirrorBusy(cfg.Paths.MirrorDir("other")) {
		t.Error("a live run on app must not make every mirror busy")
	}

	a.forgetActive("r_1")
	if a.mirrorBusy(appMirror) {
		t.Error("the mirror should be free once the run ends")
	}
}

// §14.4's floor refuses new runs rather than failing them halfway through a
// build, and preflight is the only place that refusal costs nothing.
func TestARunnerBelowItsDiskFloorIsUnavailable(t *testing.T) {
	cfg := newConfigDir(t, configOptions{runners: `
[[runner]]
name        = "greedy"
executor    = "native"
labels      = ["test"]
concurrency = 1

  [runner.native]
  min_free_gb = 1000000
`})

	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.availableRunners() != 0 {
		t.Fatal("a runner needing a petabyte should not be available")
	}
	reason := ta.runners[0].unavailableReason()
	if !strings.Contains(reason, "GB free") {
		t.Errorf("the reason should name the numbers: %q", reason)
	}
	if !strings.Contains(reason, "§14.4") {
		t.Errorf("the reason should point at the rule: %q", reason)
	}
}

// An agent with the local broker turned off still runs, and still says that
// nothing can reach it — a machine that silently ignores `latem run` is worse
// than one that refuses.
func TestLocalBrokerCanBeDisabled(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	cfg.Broker.Local.Enabled = false

	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.localBrk != nil {
		t.Error("the socket should not be bound when broker.local is disabled")
	}
	if ta.brokerOrNil() != nil {
		t.Error("with no transport the orchestrator should get a nil broker, which internal/run tolerates")
	}
	if ta.Socket() != cfg.Paths.Socket {
		t.Errorf("Socket() = %q, want the configured path even when unbound", ta.Socket())
	}
	// The run still has to be recorded: local truth outranks any transport.
	db := openStore(t, cfg)
	seedRun(t, db, "r_notrans", protocol.StateRunning)
	if err := ta.report(context.Background(), "r_notrans", protocol.Update{
		State:     protocol.StateInterrupted,
		ErrorCode: protocol.ErrInterrupted,
	}); err != nil {
		t.Fatalf("reporting with no broker: %v", err)
	}
	r, err := db.Get(context.Background(), "r_notrans")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateInterrupted {
		t.Errorf("state = %s; a transition must land in sqlite with or without a broker", r.State)
	}
}

// The metric set is the machine's own account of itself (§20). QueueDepth in
// particular has to fall back to zero, or a gauge nobody clears reports a
// queue that never drains.
func TestMetricsFollowTheQueueAndTheRunners(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	m := ta.Metrics()
	if m == nil {
		t.Fatal("a started agent should expose its metric set")
	}

	db := openStore(t, cfg)
	seedRun(t, db, "r_q1", protocol.StateQueued)
	seedRun(t, db, "r_q2", protocol.StateQueued)
	seedRun(t, db, "r_p1", protocol.StateAwaitingApproval)

	waitFor(t, "queue depth to reflect three parked runs", 10*time.Second, func() bool {
		return m.QueueDepth.Value(string(protocol.StateQueued)) == 2 &&
			m.QueueDepth.Value(string(protocol.StateAwaitingApproval)) == 1
	})

	// Draining the queue must take the gauge back down.
	if err := db.Transition(context.Background(), "r_q1", protocol.StateCancelled, protocol.Update{
		State: protocol.StateCancelled,
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queue depth to fall", 10*time.Second, func() bool {
		return m.QueueDepth.Value(string(protocol.StateQueued)) == 1
	})

	if got := m.RunnerAvailable.Value("test"); got != 1 {
		t.Errorf("runner_available = %v, want 1 for a runner that passed preflight", got)
	}
	if got := m.BrokerUp.Value("local"); got != 1 {
		t.Errorf("broker_up{local} = %v, want 1", got)
	}
	if got := m.BrokerUp.Value("remote"); got != 0 {
		t.Errorf("broker_up{remote} = %v, want 0 — there is no remote transport yet", got)
	}
}

// The daily prune has to be safe to call, log what it did, and not fall over
// on a machine with no caches and no docker daemon.
func TestMaintainIsSafeOnAnEmptyMachine(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	// Twice, because the interesting failure is a sweep that only works once.
	ta.Maintain(context.Background())
	ta.Maintain(context.Background())

	if ta.availableRunners() != 1 {
		t.Error("maintenance should not disturb a healthy runner")
	}
}

func TestBoolGauge(t *testing.T) {
	if boolGauge(true) != 1 || boolGauge(false) != 0 {
		t.Error("boolGauge is wrong")
	}
}

func TestLogRetentionFallsBackToTheDefault(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	a, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	// The config helper leaves log_retention unset, so §16's default applies.
	if got := a.logRetention(); got != 14*24*time.Hour {
		t.Errorf("logRetention = %s, want the documented 14d", got)
	}
	if got := a.queueTTL(); got != 72*time.Hour {
		t.Errorf("queueTTL = %s, want the configured 72h", got)
	}
}
