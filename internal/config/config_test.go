package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// specExample is the configuration from RUNNER_SPEC §8, verbatim. If the
// spec's own illustration does not load, one of the two is wrong and it
// matters which.
const specExample = `
schema_version = 1

[machine]
name  = "mac-mini"
id    = "mch_01H"
labels = ["macos", "arm64", "xcode-16", "ios"]

[agent]
shutdown_grace = "30m"
work_dir       = "~/.latem/work"
log_retention  = "14d"
queue_ttl      = "72h"
auto_update    = false
remote_logs    = "always"
metrics_addr   = "127.0.0.1:9838"

[broker.local]
enabled = true
socket  = "~/.latem/agent.sock"

[broker.remote]
enabled    = true
url        = "wss://api.example.com/agent"
token_file = "~/.latem/agent.key"

[repos.myapp]
url          = "git@github.com:me/myapp.git"
allowed_refs = ["main", "release/*"]
require_approval_otherwise = true
approval_ttl = "24h"

[repos.api]
url          = "ssh://git@forgejo.internal/me/api.git"
allowed_refs = ["main", "refs/tags/v*"]

[[runner]]
name        = "linux-x64"
executor    = "docker"
labels      = ["linux", "amd64", "docker"]
concurrency = 2

  [runner.docker]
  default_image = "ghcr.io/me/ci-base:latest"
  gpu           = false
  network       = "bridge"
  cpus          = 4.0
  memory        = "8g"
  min_free_gb   = 40

[[runner]]
name        = "gpu"
executor    = "docker"
labels      = ["linux", "amd64", "gpu", "cuda"]
concurrency = 1

  [runner.docker]
  default_image = "nvidia/cuda:12.4.0-devel-ubuntu22.04"
  gpu           = true
  memory        = "32g"

[[runner]]
name        = "ios"
executor    = "tart"
labels      = ["macos", "arm64", "xcode", "ios"]
concurrency = 1

  [runner.tart]
  base_image   = "latem-xcode-26"
  cpus         = 6
  memory_gb    = 16
  ssh_user     = "admin"
  boot_timeout = "3m"
  min_free_gb  = 150
  derived_data = "vm"
`

// minimalValid is the smallest config that loads. Everything absent from it
// has a default, and a test that mutates one key should not have to restate
// the rest.
const minimalValid = `
schema_version = 1

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name     = "linux"
executor = "docker"
labels   = ["linux", "docker"]
`

// fixture is a config directory with the modes §8 requires, so a test that
// means to exercise a validation rule does not trip the mode checks on the way
// there.
type fixture struct {
	Home string
	Dir  string
	Opts Options
}

func newFixture(t *testing.T, content string) fixture {
	t.Helper()

	// Not t.TempDir: its path embeds the test name, and a long name would push
	// the derived socket path past the kernel's 104-byte cap and fail tests
	// that have nothing to do with socket paths.
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatalf("temp home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return fixture{
		Home: home,
		Dir:  dir,
		Opts: Options{
			Home:     home,
			Getenv:   func(string) string { return "" },
			LookupIP: testLookupIP,
		},
	}
}

// writeFile drops a file into the config directory with an explicit mode.
func (f fixture) writeFile(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(f.Dir, name)
	if err := os.WriteFile(path, []byte("x"), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	// WriteFile applies the umask, which on most machines would quietly turn
	// 0644 into 0644 and 0600 into 0600 but is not guaranteed to.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
	return path
}

// testLookupIP is a resolver with known answers, so the INV-1 loopback rule is
// tested against fixed data rather than against whatever this machine believes
// about "localhost".
func testLookupIP(host string) ([]net.IP, error) {
	table := map[string][]string{
		"localhost":     {"127.0.0.1", "::1"},
		"loop.example":  {"127.0.0.2"},
		"split.example": {"127.0.0.1", "203.0.113.5"},
		"far.example":   {"203.0.113.5"},
		"void.example":  {},
	}
	addrs, ok := table[host]
	if !ok {
		return nil, fmt.Errorf("lookup %s: no such host", host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, net.ParseIP(a))
	}
	return ips, nil
}

func TestLoad_SpecExample(t *testing.T) {
	f := newFixture(t, specExample)
	f.writeFile(t, "agent.key", 0o600) // broker.remote.enabled = true

	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("the spec's own §8 example does not load: %v", err)
	}

	if cfg.Machine.Name != "mac-mini" {
		t.Errorf("machine.name = %q", cfg.Machine.Name)
	}
	if got := cfg.Agent.ShutdownGrace.Duration(); got != 30*time.Minute {
		t.Errorf("shutdown_grace = %s", got)
	}
	if got := cfg.Agent.LogRetention.Duration(); got != 14*24*time.Hour {
		t.Errorf("log_retention = %s, the day suffix should parse via protocol.Duration", got)
	}
	if len(cfg.Runners) != 3 {
		t.Fatalf("got %d runners, want 3 — §8's point is that one agent hosts several", len(cfg.Runners))
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(cfg.Repos))
	}

	ios, ok := cfg.Runner("ios")
	if !ok {
		t.Fatal("no runner named ios")
	}
	if ios.Tart.DerivedData != DerivedDataVM {
		t.Errorf("ios derived_data = %q", ios.Tart.DerivedData)
	}
	if got := ios.MinFreeGB(); got != 150 {
		t.Errorf("ios min_free_gb = %v, want 150", got)
	}
	if got := ios.Tart.BootTimeout.Duration(); got != 3*time.Minute {
		t.Errorf("ios boot_timeout = %s", got)
	}

	// The gpu runner declares no min_free_gb; §13.1's docker default applies.
	gpu, ok := cfg.Runner("gpu")
	if !ok {
		t.Fatal("no runner named gpu")
	}
	if got := gpu.MinFreeGB(); got != DefaultDockerMinFreeGB {
		t.Errorf("gpu min_free_gb = %v, want the docker default %v", got, DefaultDockerMinFreeGB)
	}
	if got := gpu.Docker.MemoryBytes(); got != 32<<30 {
		t.Errorf("gpu memory = %d bytes, want %d", got, int64(32)<<30)
	}

	// Gate 5 checks the weakest floor across runners before a runner is chosen.
	if got := cfg.WeakestMinFreeGB(); got != 40 {
		t.Errorf("WeakestMinFreeGB = %v, want 40 (the docker runners', not the tart runner's)", got)
	}
	if !cfg.MetricsEnabled() {
		t.Error("MetricsEnabled = false for 127.0.0.1:9838")
	}
}

func TestLoad_DefaultsFromMinimalConfig(t *testing.T) {
	f := newFixture(t, minimalValid)

	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("minimal config: %v", err)
	}

	checks := []struct {
		what string
		got  any
		want any
	}{
		{"agent.shutdown_grace", cfg.Agent.ShutdownGrace.Duration(), DefaultShutdownGrace},
		{"agent.log_retention", cfg.Agent.LogRetention.Duration(), DefaultLogRetention},
		{"agent.queue_ttl", cfg.Agent.QueueTTL.Duration(), DefaultQueueTTL},
		{"agent.remote_logs", cfg.Agent.RemoteLogs, RemoteLogsAlways},
		{"agent.metrics_addr", cfg.Agent.MetricsAddr, DefaultMetricsAddr},
		{"agent.auto_update", cfg.Agent.AutoUpdate, false},
		{"broker.local.enabled", cfg.Broker.Local.Enabled, true},
		{"secrets.provider", cfg.Secrets.Provider, SecretsProviderChain},
		{"repos.app.approval_ttl", cfg.Repos["app"].ApprovalTTL.Duration(), DefaultApprovalTTL},
		{"repos.app.require_approval_otherwise", cfg.Repos["app"].RequireApprovalOtherwise, false},
		{"runner concurrency", cfg.Runners[0].Concurrency, DefaultConcurrency},
		{"runner docker network", cfg.Runners[0].Docker.Network, DefaultDockerNetwork},
		{"runner min_free_gb", cfg.Runners[0].MinFreeGB(), DefaultDockerMinFreeGB},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}
}

// Absent metrics_addr means §20's default; an explicit empty string disables
// metrics. Conflating the two would open a socket on a machine whose owner
// deleted the key precisely to close one.
func TestLoad_MetricsAddrAbsentVersusEmpty(t *testing.T) {
	f := newFixture(t, minimalValid+"\n[agent]\nmetrics_addr = \"\"\n")

	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.MetricsAddr != "" {
		t.Errorf("explicit empty metrics_addr became %q", cfg.Agent.MetricsAddr)
	}
	if cfg.MetricsEnabled() {
		t.Error("MetricsEnabled = true after metrics were explicitly disabled")
	}
}

// Tart defaults matter more than most: derived_data is the one value §13.2
// says changes an architecture, and it must not silently be the empty string.
func TestLoad_TartDefaults(t *testing.T) {
	f := newFixture(t, `
schema_version = 1

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name     = "ios"
executor = "tart"
labels   = ["macos", "xcode"]

  [runner.tart]
  base_image = "latem-xcode-26"
`)

	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tart := cfg.Runners[0].Tart
	if tart.SSHUser != DefaultTartSSHUser {
		t.Errorf("ssh_user = %q, want %q", tart.SSHUser, DefaultTartSSHUser)
	}
	if got := tart.BootTimeout.Duration(); got != DefaultBootTimeout {
		t.Errorf("boot_timeout = %s, want %s", got, DefaultBootTimeout)
	}
	if tart.DerivedData != DerivedDataVM {
		t.Errorf("derived_data = %q, want %q — the VM's own disk is the option that cannot be pathologically slow", tart.DerivedData, DerivedDataVM)
	}
	if got := cfg.Runners[0].MinFreeGB(); got != DefaultTartMinFreeGB {
		t.Errorf("min_free_gb = %v, want %v (a base image with Xcode is ~90 GB)", got, DefaultTartMinFreeGB)
	}
}

func TestLoad_NativeDefaults(t *testing.T) {
	f := newFixture(t, `
schema_version = 1

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "host"
executor    = "native"
labels      = ["macos"]
concurrency = 1
`)

	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Runners[0].MinFreeGB(); got != DefaultNativeMinFreeGB {
		t.Errorf("min_free_gb = %v, want %v", got, DefaultNativeMinFreeGB)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	_, err = Load(Options{Home: home, Getenv: func(string) string { return "" }, LookupIP: testLookupIP})
	if err == nil {
		t.Fatal("loading a nonexistent config succeeded")
	}
	if !strings.Contains(err.Error(), "latem register") {
		t.Errorf("a missing config should name the command that creates one, got: %v", err)
	}
}

func TestRunner_MinFreeGB_UnknownExecutorIsZero(t *testing.T) {
	// Defensive: an invalid config never reaches a caller, but MinFreeGB is
	// exported and must not invent a floor for an executor it does not know.
	r := &Runner{Name: "x", Executor: Executor("mystery")}
	if got := r.MinFreeGB(); got != 0 {
		t.Errorf("MinFreeGB for an unknown executor = %v, want 0", got)
	}
	if got := (&Config{}).WeakestMinFreeGB(); got != 0 {
		t.Errorf("WeakestMinFreeGB with no runners = %v, want 0", got)
	}
	if _, ok := (&Config{}).Runner("nope"); ok {
		t.Error("Runner found a runner in an empty config")
	}
}

func TestDockerRunner_MemoryBytes(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"1024", 1024},
		{"512m", 512 << 20},
		{"8g", 8 << 30},
		{"8G", 8 << 30},
		{"8gb", 8 << 30},
		{"8GiB", 8 << 30},
		{"1.5g", 1610612736},
		{"2t", 2 << 40},
		{"64k", 64 << 10},
		{"nonsense", 0}, // rejected at load; MemoryBytes must not panic
	}
	for _, tt := range tests {
		if got := (&DockerRunner{Memory: tt.in}).MemoryBytes(); got != tt.want {
			t.Errorf("MemoryBytes(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	if got := (*DockerRunner)(nil).MemoryBytes(); got != 0 {
		t.Errorf("MemoryBytes on a nil runner = %d", got)
	}
}

func TestError_Formatting(t *testing.T) {
	one := &Error{Path: "/x/config.toml", Problems: []error{errors.New("first")}}
	if got := one.Error(); !strings.Contains(got, "first") || strings.Contains(got, "problems") {
		t.Errorf("single-problem error reads oddly: %q", got)
	}

	many := &Error{Path: "/x/config.toml", Problems: []error{errors.New("first"), errors.New("second")}}
	got := many.Error()
	if !strings.Contains(got, "2 problems") || !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("multi-problem error should list every problem, got %q", got)
	}

	empty := &Error{}
	if got := empty.Error(); !strings.Contains(got, "invalid") {
		t.Errorf("empty error = %q", got)
	}

	// errors.Is must see through to the individual problems, so a caller can
	// test for a specific one without string matching.
	sentinel := errors.New("sentinel")
	wrapped := &Error{Problems: []error{errors.New("other"), fmt.Errorf("wrapped: %w", sentinel)}}
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is does not reach the individual problems")
	}
}

func TestProblems(t *testing.T) {
	if got := Problems(nil); got != nil {
		t.Errorf("Problems(nil) = %v", got)
	}
	plain := errors.New("plain")
	if got := Problems(plain); len(got) != 1 || !errors.Is(got[0], plain) {
		t.Errorf("Problems on a non-config error = %v", got)
	}

	f := newFixture(t, "schema_version = 9\n")
	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("expected a load failure")
	}
	if len(Problems(err)) < 2 {
		t.Errorf("a config with several faults should report several problems, got %d", len(Problems(err)))
	}
}

func TestItoa(t *testing.T) {
	for _, n := range []int{0, 1, 9, 10, 4207} {
		if got, want := itoa(n), fmt.Sprint(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}
