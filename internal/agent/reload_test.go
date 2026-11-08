package agent

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// appendConfig adds TOML to the config file a test agent was started from, as
// an operator (or `latem repo add`) would before sending SIGHUP.
func appendConfig(t *testing.T, cfg *config.Config, extra string) {
	t.Helper()
	f, err := os.OpenFile(cfg.Paths.File, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func healthRepos(a *Agent) []protocol.RepoInfo {
	return a.health(context.Background()).Repos
}

// A54: adding a repo is the reason SIGHUP exists. After a reload the machine
// advertises it, plans it, and says so in the status file.
func TestReload_AddsARepoWithoutARestart(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	if res := ta.makePlan(context.Background(), protocol.PlanRequest{ID: "p1", Repo: "added"}, planPeer{}); res.ErrorCode != protocol.ErrUnknownRepo {
		t.Fatalf("before the reload, planning an unconfigured repo answered %q, want %s", res.ErrorCode, protocol.ErrUnknownRepo)
	}

	appendConfig(t, cfg, `
[repos.added]
url          = "`+filepath.Join(cfg.Paths.Dir, "no-such-mirror")+`"
github       = "me/added"
allowed_refs = ["main", "refs/pull/*/merge"]

[label_aliases]
"blacksmith-4vcpu-ubuntu-2404" = ["test"]
`)
	deferred, err := ta.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(deferred) != 0 {
		t.Errorf("nothing restart-only changed, but the reload deferred %v", deferred)
	}

	repos := healthRepos(ta.Agent)
	if !slices.ContainsFunc(repos, func(r protocol.RepoInfo) bool { return r.Key == "added" && r.GitHub == "me/added" }) {
		t.Errorf("the heartbeat does not advertise the added repo: %+v", repos)
	}
	if got := ta.current().LabelAliases["blacksmith-4vcpu-ubuntu-2404"]; !slices.Equal(got, []string{"test"}) {
		t.Errorf("label_aliases after the reload = %v", got)
	}
	// The plan now reaches the fetch, which fails on the mirror path this
	// test made sure does not exist — anything but "unknown repo".
	if res := ta.makePlan(context.Background(), protocol.PlanRequest{ID: "p2", Repo: "added"}, planPeer{}); res.ErrorCode == protocol.ErrUnknownRepo {
		t.Errorf("after the reload, planning the added repo still answered %s: %s", res.ErrorCode, res.Error)
	}

	st, err := ReadReloadStatus(cfg.Paths.ReloadStatus)
	if err != nil {
		t.Fatalf("reading the reload status: %v", err)
	}
	if st.PID != os.Getpid() || st.Reloads != 1 || st.Error != "" || st.Started.IsZero() {
		t.Errorf("reload status = %+v, want this pid, one reload, no error", st)
	}
}

// A80: the status says when the agent read config.toml as it started, for
// doctor, and a reload leaves that alone: a reload's own read is `at`.
func TestReloadStatus_SaysWhenTheConfigWasRead(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	loaded := time.Now().Add(-time.Minute).Round(0)
	ta := newTestAgent(t, cfg, func(o *Options) { o.ConfigLoaded = loaded })
	ta.run(t)

	st, err := ReadReloadStatus(cfg.Paths.ReloadStatus)
	if err != nil {
		t.Fatalf("reading the reload status: %v", err)
	}
	if !st.Loaded.Equal(loaded) {
		t.Errorf("status loaded = %v, want %v", st.Loaded, loaded)
	}
	if !st.Started.After(st.Loaded) {
		t.Errorf("status started = %v, loaded = %v; the agent reads its config before it is ready", st.Started, st.Loaded)
	}

	if _, err := ta.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if st, err = ReadReloadStatus(cfg.Paths.ReloadStatus); err != nil || !st.Loaded.Equal(loaded) || st.Reloads != 1 {
		t.Errorf("after a reload, status = %+v (%v); want loaded unchanged and one reload", st, err)
	}
}

// An agent given no time, as `--once` under a debugger could be, writes none,
// and a reader takes the field as absent rather than as the year 1.
func TestReloadStatus_NoLoadedTimeIsLeftOut(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	data, err := os.ReadFile(cfg.Paths.ReloadStatus)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"loaded"`) {
		t.Errorf("status = %s, want no loaded field", data)
	}
}

// §8: a file that does not load must not take down a running agent, and must
// not half-apply either.
func TestReload_RefusesABrokenFileAndKeepsTheConfigItHad(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	appendConfig(t, cfg, "\n[repos.added]\nurl = 7\n")
	if _, err := ta.Reload(); err == nil {
		t.Fatal("a config that does not load was reloaded")
	}
	if repos := healthRepos(ta.Agent); len(repos) != 1 || repos[0].Key != "app" {
		t.Errorf("a refused reload changed what the machine advertises: %+v", repos)
	}
	st, err := ReadReloadStatus(cfg.Paths.ReloadStatus)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reloads != 1 || st.Error == "" {
		t.Errorf("a refused reload is not recorded as one: %+v", st)
	}
}

func TestReload_ReportsWhatNeedsARestart(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)

	appendConfig(t, cfg, `
[[runner]]
name     = "second"
executor = "docker"
labels   = ["second"]

  [runner.docker]
  default_image = "alpine:3"
`)
	deferred, err := ta.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !slices.ContainsFunc(deferred, func(d string) bool { return strings.Contains(d, `runner "second" was added`) }) {
		t.Errorf("an added runner was neither applied nor reported: %v", deferred)
	}
	if got := len(ta.current().Runners); got != 1 {
		t.Errorf("the live config has %d runners; the agent is running one", got)
	}
}

func TestReload_BeforeTheAgentHasStartedIsRefused(t *testing.T) {
	ta := newTestAgent(t, newConfigDir(t, configOptions{}), nil)
	if _, err := ta.Reload(); !errors.Is(err, errNotReady) {
		t.Errorf("a reload before start answered %v, want errNotReady", err)
	}
}

// The status is withdrawn as the agent stops, so a stopped agent never looks
// like one that would answer a SIGHUP.
func TestReload_StatusIsRemovedOnShutdown(t *testing.T) {
	cfg := newConfigDir(t, configOptions{})
	r := newTestAgent(t, cfg, nil).run(t)
	if _, err := os.Stat(cfg.Paths.ReloadStatus); err != nil {
		t.Fatalf("a ready agent wrote no reload status: %v", err)
	}
	r.stop()
	if err := r.wait(t, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Paths.ReloadStatus); !os.IsNotExist(err) {
		t.Errorf("the reload status outlived the agent: %v", err)
	}
}

func TestHolder(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "agent.lock")

	if _, held, err := Holder(path); err != nil || held {
		t.Errorf("no lock file: held=%v err=%v", held, err)
	}
	l, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if pid, held, err := Holder(path); err != nil || !held || pid != os.Getpid() {
		t.Errorf("a held lock: pid=%d held=%v err=%v", pid, held, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// The pid is still in the file; the kernel is what says nobody holds it.
	if _, held, err := Holder(path); err != nil || held {
		t.Errorf("a released lock: held=%v err=%v", held, err)
	}
}

// SignalReload's whole job is to deliver SIGHUP only to an agent that will
// reload on it. These cases run against this test process: it holds the lock,
// writes the status, and catches the signal — so nothing outside the test is
// ever signalled, and a stray delivery cannot stop the test binary.
func TestSignalReload(t *testing.T) {
	hup := make(chan os.Signal, 4)
	signal.Notify(hup, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(hup) })

	newPaths := func(t *testing.T) config.Paths {
		return config.DerivePaths(shortTempDir(t))
	}
	writeStatus := func(t *testing.T, path string, st ReloadStatus) {
		t.Helper()
		a := &Agent{cfg: &config.Config{Paths: config.Paths{ReloadStatus: path}}, log: testLogger(t),
			now: time.Now, startedAt: st.Started, reloads: st.Reloads}
		a.writeReloadStatus(nil, st.Deferred)
	}
	noSignal := func(t *testing.T) {
		t.Helper()
		select {
		case <-hup:
			t.Error("SIGHUP was delivered to a process that had not said it reloads")
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Run("no agent", func(t *testing.T) {
		if _, err := SignalReload(newPaths(t), time.Second); !errors.Is(err, ErrNoAgent) {
			t.Errorf("got %v, want ErrNoAgent", err)
		}
		noSignal(t)
	})

	t.Run("an agent that wrote no status", func(t *testing.T) {
		p := newPaths(t)
		l, err := acquireLock(p.Lock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		if _, err := SignalReload(p, time.Second); !errors.Is(err, ErrCannotReload) {
			t.Errorf("got %v, want ErrCannotReload", err)
		}
		noSignal(t)
	})

	t.Run("a status left by another process", func(t *testing.T) {
		p := newPaths(t)
		l, err := acquireLock(p.Lock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		if err := os.WriteFile(p.ReloadStatus, []byte(`{"pid":1,"reloads":3}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := SignalReload(p, time.Second); !errors.Is(err, ErrCannotReload) {
			t.Errorf("got %v, want ErrCannotReload", err)
		}
		noSignal(t)
	})

	t.Run("an agent that reloads", func(t *testing.T) {
		p := newPaths(t)
		l, err := acquireLock(p.Lock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Close() }()
		started := time.Now()
		writeStatus(t, p.ReloadStatus, ReloadStatus{Started: started})

		answered := make(chan struct{})
		go func() {
			defer close(answered)
			select {
			case <-hup:
				writeStatus(t, p.ReloadStatus, ReloadStatus{Started: started, Reloads: 1, Deferred: []string{"runner changed"}})
			case <-time.After(5 * time.Second):
			}
		}()
		st, err := SignalReload(p, 5*time.Second)
		<-answered
		if err != nil {
			t.Fatalf("SignalReload: %v", err)
		}
		if st.Reloads != 1 || !slices.Equal(st.Deferred, []string{"runner changed"}) {
			t.Errorf("status = %+v, want the reload the signal caused", st)
		}
	})
}
