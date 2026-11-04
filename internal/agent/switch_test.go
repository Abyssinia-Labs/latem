package agent

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// switchFixture is a ready agent whose binary on disk reports `to`, and whose
// re-exec is recorded rather than performed.
type switchFixture struct {
	a      *Agent
	paths  config.Paths
	to     string
	failTo error
	execed chan string
}

func newSwitchFixture(t *testing.T, to string, grace time.Duration) *switchFixture {
	t.Helper()
	f := &switchFixture{to: to, execed: make(chan string, 2)}
	cfg := &config.Config{}
	cfg.Paths = config.DerivePaths(shortTempDir(t))
	a, err := New(Options{
		Config:        cfg,
		Version:       "v0.3.0",
		ShutdownGrace: grace,
		BinaryVersion: func(context.Context, string) (string, error) { return f.to, f.failTo },
		Exec: func(path string, _, _ []string) error {
			f.execed <- path
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.startReloads()
	f.a, f.paths = a, cfg.Paths
	return f
}

func (f *switchFixture) status(t *testing.T) ReloadStatus {
	t.Helper()
	st, err := ReadReloadStatus(f.paths.ReloadStatus)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *switchFixture) answer(t *testing.T) SwitchState {
	t.Helper()
	st := f.status(t)
	if st.Switch == nil {
		t.Fatal("the agent wrote no answer to SIGUSR2")
	}
	return *st.Switch
}

func (f *switchFixture) expectExec(t *testing.T, within time.Duration) {
	t.Helper()
	want, err := runningBinary()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-f.execed:
		if path != want {
			t.Errorf("re-execed %s, want the agent's own binary %s", path, want)
		}
	case <-time.After(within):
		t.Fatal("the agent did not move onto the new binary")
	}
}

func (f *switchFixture) expectNoExec(t *testing.T) {
	t.Helper()
	select {
	case path := <-f.execed:
		t.Fatalf("the agent re-execed %s", path)
	default:
	}
}

// The status names the version and the binary: that is the agent's word that
// it handles SIGUSR2, and what `latem update` compares against.
func TestSwitch_StatusSaysWhatTheAgentRuns(t *testing.T) {
	f := newSwitchFixture(t, "v0.4.0", time.Minute)
	st := f.status(t)
	bin, _ := runningBinary()
	if st.Version != "v0.3.0" || st.Binary != bin || st.Switch != nil {
		t.Errorf("status = %+v, want version v0.3.0, binary %s, no switch yet", st, bin)
	}
}

func TestSwitch_MovesOntoANewBinaryWhenIdle(t *testing.T) {
	f := newSwitchFixture(t, "v0.4.0", time.Minute)
	f.a.Switch(context.Background())
	f.expectExec(t, 5*time.Second)
	if sw := f.answer(t); sw.State != SwitchNow || sw.To != "v0.4.0" || sw.Requests != 1 {
		t.Errorf("answer = %+v, want switching to v0.4.0", sw)
	}
}

// The signal is a question; the file on disk is the answer. The same version
// is nothing to do, and no pause.
func TestSwitch_TheSameVersionIsCurrent(t *testing.T) {
	f := newSwitchFixture(t, "v0.3.0", time.Minute)
	f.a.Switch(context.Background())
	if sw := f.answer(t); sw.State != SwitchCurrent || sw.To != "v0.3.0" {
		t.Errorf("answer = %+v, want current", sw)
	}
	time.Sleep(claimSettle + 200*time.Millisecond)
	f.expectNoExec(t)
	if f.a.paused.Load() {
		t.Error("an agent with nothing to move onto stopped claiming")
	}
	// Still a nudge: whoever sent it may have changed the machine.
	if len(f.a.reprobe) != 1 {
		t.Error("a SIGUSR2 with nothing to move onto did not ask for a fresh toolchain probe")
	}
}

// A runner's labels are read once, at start (§8). After a reload that
// deferred one, the same binary re-executed is the restart that applies it,
// and it waits for runs the way a move does (A74).
func TestSwitch_RestartsInPlaceForWhatAReloadDeferred(t *testing.T) {
	execed := make(chan string, 2)
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, func(o *Options) {
		o.BinaryVersion = func(context.Context, string) (string, error) { return "test", nil }
		o.Exec = func(path string, _, _ []string) error { execed <- path; return nil }
	})
	ta.run(t)

	// Nothing deferred: the same version is current, as before.
	ta.Switch(context.Background())
	time.Sleep(claimSettle + 200*time.Millisecond)
	if len(execed) != 0 {
		t.Fatal("an agent with nothing deferred restarted")
	}

	appendConfig(t, cfg, `
[[runner]]
name     = "second"
executor = "docker"
labels   = ["second"]

  [runner.docker]
  default_image = "alpine:3"
`)
	if deferred, err := ta.Reload(); err != nil || len(deferred) == 0 {
		t.Fatalf("reload = %v, %v; want the new runner deferred", deferred, err)
	}
	ta.Switch(context.Background())
	select {
	case <-execed:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not restart for the runner its reload deferred")
	}
	st, err := ReadReloadStatus(cfg.Paths.ReloadStatus)
	if err != nil || st.Switch == nil || st.Switch.State != SwitchNow {
		t.Errorf("status = %+v, %v; want switching", st.Switch, err)
	}
}

// A file broken between the reload and the restart would leave a service
// manager starting an agent that exits. The agent keeps running instead.
func TestSwitch_ARestartForAFileThatNoLongerLoadsFails(t *testing.T) {
	execed := make(chan string, 2)
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, func(o *Options) {
		o.BinaryVersion = func(context.Context, string) (string, error) { return "test", nil }
		o.Exec = func(path string, _, _ []string) error { execed <- path; return nil }
	})
	ta.run(t)
	appendConfig(t, cfg, "\n[[runner]]\nname = \"second\"\nexecutor = \"docker\"\nlabels = [\"second\"]\n\n  [runner.docker]\n  default_image = \"alpine:3\"\n")
	if _, err := ta.Reload(); err != nil {
		t.Fatal(err)
	}
	appendConfig(t, cfg, "\nthis is not toml\n")
	ta.Switch(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := ReadReloadStatus(cfg.Paths.ReloadStatus)
		if st.Switch != nil && st.Switch.State == SwitchFailed {
			if len(execed) != 0 {
				t.Error("the agent re-execed onto a config that does not load")
			}
			if ta.paused.Load() {
				t.Error("the agent stopped taking work after a restart it did not do")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the agent did not say why it did not restart")
}

func TestSwitch_ABinaryThatWillNotSayItsVersionFails(t *testing.T) {
	f := newSwitchFixture(t, "", time.Minute)
	f.failTo = errors.New("exec format error")
	f.a.Switch(context.Background())
	if sw := f.answer(t); sw.State != SwitchFailed || sw.Error == "" {
		t.Errorf("answer = %+v, want failed with the reason", sw)
	}
	f.expectNoExec(t)
	if f.a.paused.Load() {
		t.Error("a failed move left the agent refusing work")
	}
}

// Never mid-run: it stops claiming, says it is waiting, and moves when the
// run ends.
func TestSwitch_WaitsForTheRunInFlight(t *testing.T) {
	f := newSwitchFixture(t, "v0.4.0", 10*time.Second)
	f.a.noteActive("r_busy", "app")
	p := &pool{a: f.a, accepting: true, active: map[string]time.Time{}}

	f.a.Switch(context.Background())
	time.Sleep(claimSettle + 300*time.Millisecond)
	f.expectNoExec(t)
	if p.accepts() {
		t.Error("the pool was still claiming while the agent waited to move")
	}
	if sw := f.answer(t); sw.State != SwitchWaiting || sw.Runs != 1 {
		t.Errorf("answer = %+v, want waiting for 1 run", sw)
	}

	// A second request while the first waits is the same request.
	f.a.Switch(context.Background())
	if sw := f.answer(t); sw.State != SwitchWaiting || sw.Requests != 2 {
		t.Errorf("second answer = %+v, want waiting, request 2", sw)
	}

	f.a.forgetActive("r_busy")
	f.expectExec(t, 5*time.Second)
	f.expectNoExec(t)
}

// Out of shutdown_grace it goes back to work rather than cancelling the run,
// and moves at the first idle moment after.
func TestSwitch_PastTheGraceItTakesWorkAndMovesWhenIdle(t *testing.T) {
	old := switchRetry
	switchRetry = 100 * time.Millisecond
	t.Cleanup(func() { switchRetry = old })

	f := newSwitchFixture(t, "v0.4.0", 200*time.Millisecond)
	f.a.noteActive("r_long", "app")
	p := &pool{a: f.a, accepting: true, active: map[string]time.Time{}}

	f.a.Switch(context.Background())
	time.Sleep(claimSettle + time.Second)
	f.expectNoExec(t)
	if !p.accepts() {
		t.Error("the agent was still refusing work after shutdown_grace")
	}
	if f.a.draining.Load() {
		t.Error("the move set the draining latch, which nothing clears")
	}
	if sw := f.answer(t); sw.State != SwitchWaiting {
		t.Errorf("answer = %+v, want waiting", sw)
	}

	f.a.forgetActive("r_long")
	f.expectExec(t, 5*time.Second)
}

// SignalSwitch delivers SIGUSR2 only to an agent that has said it handles it,
// run against this test process as TestSignalReload is.
func TestSignalSwitch(t *testing.T) {
	usr2 := make(chan os.Signal, 4)
	signal.Notify(usr2, syscall.SIGUSR2)
	t.Cleanup(func() { signal.Stop(usr2) })

	newPaths := func(t *testing.T) config.Paths { return config.DerivePaths(shortTempDir(t)) }
	write := func(t *testing.T, path string, st ReloadStatus) {
		t.Helper()
		st.PID = os.Getpid()
		a := &Agent{cfg: &config.Config{Paths: config.Paths{ReloadStatus: path}}, log: testLogger(t),
			now: time.Now, startedAt: st.Started, version: st.Version, switchState: st.Switch, deferred: st.Deferred}
		a.writeStatus()
	}
	locked := func(t *testing.T, p config.Paths) {
		t.Helper()
		l, err := acquireLock(p.Lock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	noSignal := func(t *testing.T) {
		t.Helper()
		select {
		case <-usr2:
			t.Error("SIGUSR2 was delivered when it should not have been")
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Run("no agent", func(t *testing.T) {
		if _, err := SignalSwitch(newPaths(t), "v0.4.0", time.Second); !errors.Is(err, ErrNoAgent) {
			t.Errorf("got %v, want ErrNoAgent", err)
		}
		noSignal(t)
	})

	t.Run("an agent from before A56", func(t *testing.T) {
		p := newPaths(t)
		locked(t, p)
		write(t, p.ReloadStatus, ReloadStatus{Started: time.Now()})
		if _, err := SignalSwitch(p, "v0.4.0", time.Second); !errors.Is(err, ErrCannotSwitch) {
			t.Errorf("got %v, want ErrCannotSwitch", err)
		}
		noSignal(t)
	})

	t.Run("an agent running it with a restart pending", func(t *testing.T) {
		p := newPaths(t)
		locked(t, p)
		write(t, p.ReloadStatus, ReloadStatus{Started: time.Now(), Version: "v0.4.0",
			Deferred: []string{`runner "macos" was changed`}})
		go func() {
			select {
			case <-usr2:
				write(t, p.ReloadStatus, ReloadStatus{Started: time.Now().Add(time.Second), Version: "v0.4.0"})
			case <-time.After(5 * time.Second):
			}
		}()
		got, err := SignalSwitch(p, "v0.4.0", 5*time.Second)
		if err != nil || len(got.Deferred) != 0 {
			t.Errorf("got %+v, %v; want the agent restarted with nothing deferred", got, err)
		}
	})

	t.Run("an agent already running it", func(t *testing.T) {
		p := newPaths(t)
		locked(t, p)
		write(t, p.ReloadStatus, ReloadStatus{Started: time.Now(), Version: "v0.4.0"})
		st, err := SignalSwitch(p, "v0.4.0", time.Second)
		if err != nil || st.Version != "v0.4.0" {
			t.Errorf("got %+v, %v", st, err)
		}
		noSignal(t)
	})

	t.Run("an agent that moves", func(t *testing.T) {
		p := newPaths(t)
		locked(t, p)
		started := time.Now().Add(-time.Hour)
		write(t, p.ReloadStatus, ReloadStatus{Started: started, Version: "v0.3.0"})
		go func() {
			select {
			case <-usr2:
				// The new image's first status: same pid, later start.
				write(t, p.ReloadStatus, ReloadStatus{Started: time.Now(), Version: "v0.4.0"})
			case <-time.After(5 * time.Second):
			}
		}()
		st, err := SignalSwitch(p, "v0.4.0", 5*time.Second)
		if err != nil || st.Version != "v0.4.0" {
			t.Errorf("got %+v, %v; want the agent on v0.4.0", st, err)
		}
	})

	t.Run("an agent that waits", func(t *testing.T) {
		p := newPaths(t)
		locked(t, p)
		started := time.Now()
		write(t, p.ReloadStatus, ReloadStatus{Started: started, Version: "v0.3.0"})
		go func() {
			select {
			case <-usr2:
				write(t, p.ReloadStatus, ReloadStatus{Started: started, Version: "v0.3.0",
					Switch: &SwitchState{Requests: 1, State: SwitchWaiting, To: "v0.4.0", Runs: 2}})
			case <-time.After(5 * time.Second):
			}
		}()
		st, err := SignalSwitch(p, "v0.4.0", 5*time.Second)
		if err != nil || st.Switch == nil || st.Switch.State != SwitchWaiting || st.Switch.Runs != 2 {
			t.Errorf("got %+v, %v; want waiting for 2 runs", st, err)
		}
	})
}
