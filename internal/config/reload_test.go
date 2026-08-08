package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rewrite replaces the config file in place, the way an operator editing it
// before sending SIGHUP would.
func (f fixture) rewrite(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.Dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
}

func TestReload_AppliesTheReloadableSubset(t *testing.T) {
	f := newFixture(t, minimalValid)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewReloader(cfg)

	f.rewrite(t, `
schema_version = 1

[agent]
queue_ttl   = "12h"
remote_logs = "on_failure"

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[repos.added]
url          = "git@github.com:me/added.git"
allowed_refs = ["main", "release/*"]

[[runner]]
name     = "linux"
executor = "docker"
labels   = ["linux", "docker"]
`)

	next, deferred, err := r.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(deferred) != 0 {
		t.Errorf("nothing needed a restart, but got %v", deferred)
	}
	if got := next.Agent.QueueTTL.Duration(); got != 12*time.Hour {
		t.Errorf("queue_ttl = %s, want 12h", got)
	}
	if next.Agent.RemoteLogs != RemoteLogsOnFailure {
		t.Errorf("remote_logs = %q", next.Agent.RemoteLogs)
	}
	if _, ok := next.Repos["added"]; !ok {
		t.Error("a repo added on SIGHUP did not appear — adding a repo is the reason SIGHUP exists")
	}
	if r.Config() != next {
		t.Error("Config() does not return the reloaded config")
	}
}

// §8: "changes to [[runner]] require a restart". The live config must keep
// describing the agent as it actually is, or a heartbeat would advertise
// runners that do not exist.
func TestReload_RunnerChangesAreDeferredAndNotApplied(t *testing.T) {
	f := newFixture(t, minimalValid)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewReloader(cfg)

	f.rewrite(t, `
schema_version = 1

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "linux"
executor    = "docker"
labels      = ["linux", "docker"]
concurrency = 8

[[runner]]
name     = "ios"
executor = "tart"
labels   = ["macos"]

  [runner.tart]
  base_image = "latem-xcode-26"
`)

	next, deferred, err := r.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	joined := strings.Join(deferred, "; ")
	if !strings.Contains(joined, `runner "linux" was changed`) {
		t.Errorf("a changed runner was not reported: %v", deferred)
	}
	if !strings.Contains(joined, `runner "ios" was added`) {
		t.Errorf("an added runner was not reported: %v", deferred)
	}
	if len(next.Runners) != 1 || next.Runners[0].Concurrency != 1 {
		t.Errorf("the live config picked up runner changes that are not running: %+v", next.Runners)
	}
}

func TestReload_RemovedRunnerIsReported(t *testing.T) {
	f := newFixture(t, `
schema_version = 1

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name     = "linux"
executor = "docker"
labels   = ["linux", "docker"]

[[runner]]
name     = "spare"
executor = "docker"
labels   = ["linux", "spare"]
`)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	f.rewrite(t, minimalValid)
	_, deferred, err := NewReloader(cfg).Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if strings.Join(deferred, "; ") != `runner "spare" was removed` {
		t.Errorf("deferred = %v", deferred)
	}
}

// A broken file on SIGHUP must not take down an agent with builds in flight.
// The operator was editing, not deploying.
func TestReload_InvalidFileLeavesTheLiveConfigAlone(t *testing.T) {
	f := newFixture(t, minimalValid)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewReloader(cfg)

	f.rewrite(t, minimalValid+"\n[agent]\nmetrics_addr = \"0.0.0.0:9838\"\n")

	live, deferred, err := r.Reload()
	if err == nil {
		t.Fatal("a config violating INV-1 was accepted on reload")
	}
	if deferred != nil {
		t.Errorf("deferred = %v on a failed reload", deferred)
	}
	if live != cfg {
		t.Error("the live config changed despite the reload failing")
	}
	if r.Config() != cfg {
		t.Error("Config() no longer returns the config that is actually running")
	}
}

// Restart-only fields must not slip in through the merge.
func TestReload_RestartOnlyFieldsAreCarriedForward(t *testing.T) {
	f := newFixture(t, minimalValid+`
[machine]
id = "mch_first"

[agent]
metrics_addr = "127.0.0.1:9838"
`)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewReloader(cfg)

	f.rewrite(t, minimalValid+`
[machine]
id = "mch_second"

[agent]
work_dir     = "elsewhere"
metrics_addr = "127.0.0.2:9999"

[broker.local]
enabled = false
socket  = "other.sock"

[broker.remote]
enabled    = true
url        = "wss://api.example.com/agent"
token_file = "rotated.key"
`)
	f.writeFile(t, "rotated.key", 0o600)

	next, deferred, err := r.Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if next.Machine.ID != "mch_first" {
		t.Errorf("machine.id changed under a reload: %q", next.Machine.ID)
	}
	if next.Agent.MetricsAddr != "127.0.0.1:9838" {
		t.Errorf("metrics_addr changed under a reload: %q", next.Agent.MetricsAddr)
	}
	if next.Paths.Work != cfg.Paths.Work {
		t.Errorf("work_dir moved under a reload: %s", next.Paths.Work)
	}
	if next.Paths.Socket != cfg.Paths.Socket || !next.Broker.Local.Enabled {
		t.Errorf("the local broker changed under a reload: %+v", next.Broker.Local)
	}

	// The token file is the exception: rotating a machine token is routine.
	if want := filepath.Join(f.Dir, "rotated.key"); next.Paths.TokenFile != want {
		t.Errorf("token_file = %s, want %s — rotating a token should not need a restart", next.Paths.TokenFile, want)
	}
	if !next.Broker.Remote.Enabled || next.Broker.Remote.URL == "" {
		t.Error("the remote broker did not reload")
	}

	joined := strings.Join(deferred, "; ")
	for _, want := range []string{"machine.id", "agent.work_dir", "agent.metrics_addr", "broker.local.enabled", "broker.local.socket"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a change needing a restart was applied silently: %s not in %v", want, deferred)
		}
	}
}

func TestReload_ParsedConfigCannotReload(t *testing.T) {
	cfg, err := Parse([]byte(minimalValid), Options{
		Home:     "/nowhere",
		Getenv:   func(string) string { return "" },
		LookupIP: testLookupIP,
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, err := NewReloader(cfg).Reload(); !errors.Is(err, ErrNotFromFile) {
		t.Errorf("want ErrNotFromFile, got %v", err)
	}
}

// A reload re-reads the directory it was loaded from, even if the environment
// has changed underneath the process since.
func TestReload_PinsTheConfigDirectory(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	alt := filepath.Join(home, "second")
	if err := os.MkdirAll(alt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alt, FileName), []byte(minimalValid), 0o600); err != nil {
		t.Fatal(err)
	}

	env := alt
	cfg, err := Load(Options{
		Home:     home,
		Getenv:   func(k string) string { return map[string]string{EnvConfigDir: env}[k] },
		LookupIP: testLookupIP,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	env = filepath.Join(home, "moved") // the variable changes; the agent must not
	next, _, err := NewReloader(cfg).Reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if next.Paths.Dir != alt {
		t.Errorf("reload followed the environment to %s instead of staying at %s", next.Paths.Dir, alt)
	}
}

func TestRequiresRestart_IdenticalConfigsNeedNothing(t *testing.T) {
	f := newFixture(t, specExample)
	f.writeFile(t, "agent.key", 0o600)

	a, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := RequiresRestart(a, b); len(got) != 0 {
		t.Errorf("two loads of one file disagree: %v", got)
	}
}
