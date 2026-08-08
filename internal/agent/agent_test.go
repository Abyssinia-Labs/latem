package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/metrics"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestNewRequiresAConfig(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no config should fail; §6 step 1 is the caller's, and it must have happened")
	}
}

// An agent with no runners can accept nothing, and starting one would present
// as runs that queue and never move.
func TestStartRefusesWithNoRunners(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	cfg.Runners = nil

	a, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, errNoRunners) {
		t.Fatalf("Run with no runners = %v, want errNoRunners", err)
	}
}

// If every runner's executor fails to build there is nothing to serve, and an
// agent that started anyway would hold the lock and the socket while refusing
// all work — which looks, from the outside, exactly like a hung machine.
func TestStartRefusesWhenNoRunnerHasAnExecutor(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	a, err := New(Options{
		Config: cfg,
		Logger: testLogger(t),
		NewExecutor: func(*configRunner, configPaths) (executorIface, error) {
			return nil, errBoom
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no runner has a working executor") {
		t.Fatalf("Run = %v, want a refusal naming the cause", err)
	}
}

// The lock is what makes "exactly one agent per config directory" true, and a
// second agent has to fail with a message rather than corrupt state.
func TestASecondAgentOnOneConfigDirectoryRefusesToStart(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})

	first := newTestAgent(t, cfg, nil)
	first.run(t)

	second := newTestAgent(t, cfg, nil)
	err := second.Run(context.Background())
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("a second agent returned %v, want ErrLocked", err)
	}
}

// §20's rolling preflight: a runner that fails stops accepting work, and
// recovery is automatic when the check passes again. Both halves matter — the
// first stops a build failing forty minutes in, and the second is what keeps a
// Docker restart from needing an operator.
func TestPreflightMarksARunnerUnavailableAndRecovers(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.availableRunners() != 1 {
		t.Fatalf("the runner should start available: %q", ta.runners[0].unavailableReason())
	}

	ta.exec.setPreflight(errors.New("docker daemon unreachable"))
	waitFor(t, "the runner to be marked unavailable", 10*time.Second, func() bool {
		return ta.availableRunners() == 0
	})
	if reason := ta.runners[0].unavailableReason(); !strings.Contains(reason, "docker daemon unreachable") {
		t.Errorf("unavailable reason = %q, want the probe's own words", reason)
	}

	ta.exec.setPreflight(nil)
	waitFor(t, "the runner to recover on its own", 10*time.Second, func() bool {
		return ta.availableRunners() == 1
	})
}

// A machine where nothing can execute must leave its work queued rather than
// claiming it and failing it. A queued run starts when Docker comes back; a
// failed one needs a human.
func TestNoRunIsClaimedWhileEveryRunnerIsUnavailable(t *testing.T) {
	cfg := newRunnableConfig(t)
	ta := newTestAgent(t, cfg, nil)
	ta.exec.setPreflight(errors.New("daemon down"))
	ta.run(t)

	waitFor(t, "the runner to be unavailable", 10*time.Second, func() bool {
		return ta.availableRunners() == 0
	})
	enqueueDirectly(t, cfg, "r_waits", "main")

	time.Sleep(500 * time.Millisecond)
	db := openStore(t, cfg)
	r, err := db.Get(context.Background(), "r_waits")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateQueued {
		t.Fatalf("a run on a machine with no available runner became %s, want it left queued", r.State)
	}

	// And it starts by itself once the machine recovers, with no nudge.
	ta.exec.setPreflight(nil)
	got := waitState(t, cfg, "r_waits", 60*time.Second,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected)
	if got.State != protocol.StateSucceeded {
		t.Errorf("after recovery the run finished %s (%s)", got.State, got.ErrorCode)
	}
}

// The heartbeat is what a control plane uses to decide a machine is alive and
// what it can take (§20). It must carry per-runner availability, and it must
// keep beating while draining with Draining set — a machine that stops beating
// and one that is shutting down have to look different.
func TestHeartbeatCarriesCapacityAndDraining(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	waitFor(t, "the local broker to record a heartbeat", 10*time.Second, func() bool {
		_, _, ok := ta.localBrk.Health()
		return ok
	})

	h, _, _ := ta.localBrk.Health()
	if h.MachineID != "mch_test" {
		t.Errorf("machine id = %q", h.MachineID)
	}
	if h.AgentVer != "test" || h.OS == "" || h.Arch == "" {
		t.Errorf("health is missing build identity: %+v", h)
	}
	if len(h.Runners) != 1 {
		t.Fatalf("health carries %d runners, want 1", len(h.Runners))
	}
	if !h.Runners[0].Available {
		t.Error("a runner that passed preflight should be advertised as available")
	}
	if h.Runners[0].Concurrency != 1 {
		t.Errorf("concurrency = %d, want the configured 1", h.Runners[0].Concurrency)
	}
	if h.Draining {
		t.Error("a serving agent is not draining")
	}

	ta.beginDrain()
	waitFor(t, "a draining heartbeat", 10*time.Second, func() bool {
		got, _, _ := ta.localBrk.Health()
		return got.Draining
	})
}

// INV-1 permits a loopback metrics listener and forbids everything else. The
// config loader has already refused a non-loopback address; this checks the
// agent actually serves the one it was given, because a metrics endpoint
// nobody can scrape is indistinguishable from one that was never started.
func TestMetricsServeOnLoopback(t *testing.T) {
	addr := freeLoopbackAddr(t)
	cfg := newConfigDir(t, configOptions{metricsAddr: addr})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if got := ta.MetricsAddr(); got != addr {
		t.Fatalf("metrics bound %q, want %q", got, addr)
	}
	if err := metrics.CheckLoopback(ta.MetricsAddr()); err != nil {
		t.Fatalf("the agent bound a non-loopback address (INV-1): %v", err)
	}

	resp, err := http.Get("http://" + addr + metrics.PathMetrics) //nolint:noctx // loopback, in a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"latem_runner_available", "latem_agent_info"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the exposition is missing %s:\n%s", want, body)
		}
	}
}

// Metrics are observability, not operation: a port somebody else is already
// using must not stop the machine building.
func TestMetricsFailureDoesNotStopTheAgent(t *testing.T) {
	addr := freeLoopbackAddr(t)
	blocker, err := listenOn(addr)
	if err != nil {
		t.Skipf("cannot hold the port: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	cfg := newConfigDir(t, configOptions{metricsAddr: addr})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if ta.MetricsAddr() != "" {
		t.Error("the metrics server should not have bound a port somebody else holds")
	}
	if ta.availableRunners() != 1 {
		t.Error("a metrics failure must not take the runners with it")
	}
}

// The daily ticker's job is to reclaim, and to say what it reclaimed: a sweep
// that silently drops a warm cache is indistinguishable from a bug (§14.4).
func TestMaintainSweepsExpiredRunsAndOldLogs(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	db := openStore(t, cfg)
	ctx := context.Background()
	seedRun(t, db, "r_expired", protocol.StateQueued)
	if err := db.SetExpiry(ctx, "r_expired", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// A log old enough to be deleted by retention, for a run nothing is
	// executing.
	if err := os.MkdirAll(cfg.Paths.Logs, 0o700); err != nil {
		t.Fatal(err)
	}
	oldLog := filepath.Join(cfg.Paths.Logs, "r_ancient.log")
	if err := os.WriteFile(oldLog, []byte("old output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(oldLog, ancient, ancient); err != nil {
		t.Fatal(err)
	}

	ta.Maintain(ctx)

	r, err := db.Get(ctx, "r_expired")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateRejected || r.ErrorCode != protocol.ErrQueueExpired {
		t.Errorf("the daily sweep left %s as %s (%s), want rejected/E_QUEUE_EXPIRED",
			r.ID, r.State, r.ErrorCode)
	}
	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("a log past agent.log_retention survived the daily sweep: %v", err)
	}
}

// Config supplies the executor for each runner, and a tart runner has to be
// unavailable-with-a-reason rather than a crash or a silent no-op, because
// WS-13 has not landed.
func TestDefaultExecutorPerConfiguredExecutor(t *testing.T) {
	paths := config.DerivePaths(shortTempDir(t))

	tests := []struct {
		name     string
		runner   config.Runner
		wantErr  string
		wantName string
	}{
		{
			name:     "native",
			runner:   config.Runner{Name: "n", Executor: config.ExecutorNative},
			wantName: "native",
		},
		{
			name:    "tart is not implemented",
			runner:  config.Runner{Name: "t", Executor: config.ExecutorTart},
			wantErr: "WS-13",
		},
		{
			name:    "unknown",
			runner:  config.Runner{Name: "u", Executor: config.Executor("nonsense")},
			wantErr: "unknown executor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex, err := DefaultExecutor(&tt.runner, paths)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultExecutor: %v", err)
			}
			if ex.Name() != tt.wantName {
				t.Errorf("Name = %q, want %q", ex.Name(), tt.wantName)
			}
		})
	}
}

func TestHasLabel(t *testing.T) {
	labels := []string{"macos", "arm64", "ios"}
	if !hasLabel(labels, "ios") {
		t.Error("hasLabel missed a present label")
	}
	if hasLabel(labels, "linux") {
		t.Error("hasLabel found an absent label")
	}
	if hasLabel(nil, "ios") {
		t.Error("hasLabel on nil should be false")
	}
}

// --drain-after is how an operator schedules a restart around builds: stop
// accepting, finish what is running, exit.
func TestDrainAfterStopsAcceptingAndExits(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, func(o *Options) { o.DrainAfter = 200 * time.Millisecond })

	done := make(chan error, 1)
	go func() { done <- ta.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("--drain-after did not shut the agent down")
	}
	if !ta.Draining() {
		t.Error("an agent that drained should say so")
	}
}
