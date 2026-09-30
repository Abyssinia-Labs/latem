package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// configOptions describes the config.toml a test wants.
//
// It is a struct rather than a template string because the interesting
// variations here are structural — how many runners, which executor, whether
// metrics are on — and a test that assembled TOML by concatenation would fail
// at load with a parse error instead of at the assertion it was written for.
type configOptions struct {
	runners     string
	metricsAddr string
	repoURL     string
	queueTTL    string
	grace       string
	// remoteURL turns on [broker.remote] and writes an agent.key beside the
	// config, which the loader requires at mode 0600 when the transport is on.
	remoteURL string
}

// newConfigDir writes a valid config directory and loads it.
//
// The modes matter: §8 requires 0700 on the directory and 0600 on the file,
// and the loader enforces both. A test that created them at the default umask
// would fail on the config rather than on what it meant to check.
func newConfigDir(t *testing.T, opts configOptions) *config.Config {
	t.Helper()

	home := shortTempDir(t)
	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if opts.runners == "" {
		opts.runners = `
[[runner]]
name        = "test"
executor    = "native"
labels      = ["test", "local"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`
	}
	if opts.repoURL == "" {
		opts.repoURL = "https://git.example.invalid/app.git"
	}
	if opts.queueTTL == "" {
		opts.queueTTL = "72h"
	}
	if opts.grace == "" {
		opts.grace = "5s"
	}

	body := fmt.Sprintf(`schema_version = 1

[machine]
name = "test-machine"
id   = "mch_test"

[agent]
shutdown_grace = %q
queue_ttl      = %q
metrics_addr   = %q

[broker.local]
enabled = true
%s
[repos.app]
url          = %q
allowed_refs = ["main"]
require_approval_otherwise = true
approval_ttl = "24h"
%s`, opts.grace, opts.queueTTL, opts.metricsAddr, remoteBlock(t, dir, opts.remoteURL), opts.repoURL, opts.runners)

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(config.Options{
		Dir:      dir,
		Home:     home,
		Getenv:   func(string) string { return "" },
		LookupIP: func(string) ([]net.IP, error) { return []net.IP{net.IPv4(127, 0, 0, 1)}, nil },
	})
	if err != nil {
		t.Fatalf("loading the config this test just wrote: %v", err)
	}
	return cfg
}

// remoteBlock writes the machine token and returns the [broker.remote] table,
// or nothing when the test did not ask for one.
func remoteBlock(t *testing.T, dir, url string) string {
	t.Helper()
	if url == "" {
		return ""
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.key"), []byte("tok-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("\n[broker.remote]\nenabled = true\nurl     = %q\n", url)
}

// shortTempDir is t.TempDir() with a length budget.
//
// The default on macOS is /var/folders/<hash>/<hash>/T/<TestName><digits>/001,
// which is over a hundred characters before anything is joined to it — and a
// Unix socket path is capped at 103 bytes by the kernel. config.Load rejects
// the overrun with a clear message, which means every agent test would fail on
// its own temporary directory rather than on what it was written to check.
// This is the same constraint an operator hits with a deep --config-dir; the
// tests simply cannot opt out of it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// freeLoopbackAddr reserves and releases a loopback port.
//
// config validation rejects port 0 — it must be 1..65535 — so a test that
// wants a real metrics listener cannot ask the kernel for one at bind time and
// has to pick a port that was free a moment ago. The race is theoretical on a
// test machine and the alternative is a hard-coded 9838 that collides with the
// developer's own agent.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// fakeEnv is one run's environment for [fakeExecutor].
type fakeEnv struct {
	ex     *fakeExecutor
	runID  string
	marker string
	closed bool
	mu     sync.Mutex
}

func (e *fakeEnv) Exec(ctx context.Context, step executor.Step, stdout, _ io.Writer) (executor.Result, error) {
	_, _ = io.WriteString(stdout, "step "+step.Name+"\n")
	if e.ex.emit != nil {
		e.ex.emit(stdout)
	}
	if e.ex.hold > 0 {
		select {
		case <-time.After(e.ex.hold):
		case <-ctx.Done():
			return executor.Result{ExitCode: 143}, ctx.Err()
		}
	}
	return executor.Result{ExitCode: e.ex.exitCode}, nil
}

func (e *fakeEnv) EnvFile() string { return filepath.Join(e.ex.dir, e.runID+".env") }

func (e *fakeEnv) Close(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	return os.Remove(e.marker)
}

func (e *fakeEnv) ActionsDir() string { return "" }

// fakeExecutor stands in for docker, tart, and native.
//
// Its resources are marker files in a directory, which is enough to make the
// two claims that matter testable without a daemon: a resource exists while a
// run is in flight, and [fakeExecutor.Reap] removes exactly the ones whose run
// is no longer live. A crash is simulated by never calling Close, which is
// precisely what SIGKILL does.
type fakeExecutor struct {
	dir      string
	name     string
	hold     time.Duration
	exitCode int
	// emit lets a test drive a step's stdout: how much, and when. It is what
	// makes "the link partitions mid-run" mean mid-run rather than
	// mid-scheduling. Set it before the agent starts.
	emit func(stdout io.Writer)

	mu           sync.Mutex
	preflightErr error
	prepareErr   error
	reapErr      error
	prepared     []string
	reapCalls    int
}

// Type aliases so a test can write an executor factory without importing three
// packages for one function signature.
type (
	configRunner  = config.Runner
	configPaths   = config.Paths
	executorIface = executor.Executor
)

func newFakeExecutor(t *testing.T, name string) *fakeExecutor {
	t.Helper()
	dir := t.TempDir()
	return &fakeExecutor{dir: dir, name: name}
}

var _ executor.Executor = (*fakeExecutor)(nil)

func (f *fakeExecutor) Name() string { return f.name }

func (f *fakeExecutor) Preflight(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preflightErr
}

func (f *fakeExecutor) setPreflight(err error) {
	f.mu.Lock()
	f.preflightErr = err
	f.mu.Unlock()
}

func (f *fakeExecutor) Prepare(_ context.Context, spec executor.Spec) (executor.Env, error) {
	f.mu.Lock()
	if f.prepareErr != nil {
		err := f.prepareErr
		f.mu.Unlock()
		return nil, err
	}
	f.prepared = append(f.prepared, spec.RunID)
	f.mu.Unlock()

	marker := f.resource(spec.RunID)
	if err := os.WriteFile(marker, []byte(spec.RunID), 0o600); err != nil {
		return nil, err
	}
	return &fakeEnv{ex: f, runID: spec.RunID, marker: marker}, nil
}

func (f *fakeExecutor) Reap(_ context.Context, live func(string) bool) ([]executor.Reaped, error) {
	f.mu.Lock()
	f.reapCalls++
	err := f.reapErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	var out []executor.Reaped
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".resource")
		if name == e.Name() {
			continue
		}
		if live != nil && live(name) {
			continue
		}
		if err := os.Remove(filepath.Join(f.dir, e.Name())); err != nil {
			return out, err
		}
		out = append(out, executor.Reaped{RunID: name, Kind: "container", Ref: e.Name()})
	}
	return out, nil
}

// resource is where this executor records that a run has an environment.
func (f *fakeExecutor) resource(runID string) string {
	return filepath.Join(f.dir, runID+".resource")
}

// leak creates a resource with no environment behind it, which is what a
// SIGKILL mid-run leaves on disk.
func (f *fakeExecutor) leak(t *testing.T, runID string) {
	t.Helper()
	if err := os.WriteFile(f.resource(runID), []byte(runID), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeExecutor) resources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), ".resource"); name != e.Name() {
			out = append(out, name)
		}
	}
	return out
}

// staticSecrets resolves nothing, which is right for a job that declares no
// secrets and makes it impossible for a test to touch a real credential store.
type staticSecrets struct{}

func (staticSecrets) Name() string { return "test" }

func (staticSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	return nil, fmt.Errorf("test: %s: %w", name, secrets.ErrNotFound)
}

// testAgent builds an agent with test-scale intervals and a fake executor.
type testAgent struct {
	*Agent
	exec *fakeExecutor
	t    *testing.T
}

func newTestAgent(t *testing.T, cfg *config.Config, mutate func(*Options)) *testAgent {
	t.Helper()

	fx := newFakeExecutor(t, "docker")
	opts := Options{
		Config:  cfg,
		Version: "test",
		Logger:  testLogger(t),
		Secrets: staticSecrets{},
		NewExecutor: func(*config.Runner, config.Paths) (executor.Executor, error) {
			return fx, nil
		},
		HeartbeatInterval:   50 * time.Millisecond,
		PreflightInterval:   50 * time.Millisecond,
		MaintenanceInterval: time.Hour,
		ClaimInterval:       20 * time.Millisecond,
		SlotWait:            2 * time.Second,
		PreflightTimeout:    2 * time.Second,
		TeardownGrace:       2 * time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &testAgent{Agent: a, exec: fx, t: t}
}

// running is a started agent: the cancel that begins its shutdown, and a
// channel that closes when Run has returned.
//
// The completion signal is a closed channel rather than the error channel
// because a test that waits for shutdown itself must not make the cleanup wait
// forever on a value somebody already took. Closing says "finished" to every
// reader; sending says it to exactly one.
type running struct {
	stop    context.CancelFunc
	stopped chan struct{}
	err     error
}

// wait blocks until Run has returned and reports what it returned.
func (r *running) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-r.stopped:
		return r.err
	case <-time.After(timeout):
		t.Fatal("the agent did not shut down")
		return nil
	}
}

// run starts the agent and waits for it to be serving.
func (ta *testAgent) run(t *testing.T) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{stop: cancel, stopped: make(chan struct{})}
	go func() {
		defer close(r.stopped)
		r.err = ta.Run(ctx)
	}()

	select {
	case <-ta.Ready():
	case <-r.stopped:
		cancel()
		t.Fatalf("agent exited during startup: %v", r.err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("agent never became ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.stopped:
		case <-time.After(60 * time.Second):
			t.Error("agent did not shut down")
		}
	})
	return r
}

// testLogger routes the agent's narration into the test log, so a failure
// carries the agent's own account of what it was doing.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct {
	mu   sync.Mutex
	t    *testing.T
	done bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return len(p), nil
	}
	// A goroutine still logging after the test returned would panic on
	// t.Log; the agent's loops are stopped by Cleanup but the race is real.
	defer func() {
		if recover() != nil {
			w.done = true
		}
	}()
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// openStore opens the agent's database from a test, for arranging rows before
// startup and asserting on them after shutdown.
func openStore(t *testing.T, cfg *config.Config) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), cfg.Paths.DB, noRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type noRedactor struct{}

func (noRedactor) Redact(line string) string { return line }

// seedRun writes a run row directly, standing in for whatever put it there.
func seedRun(t *testing.T, db *store.DB, id string, state protocol.RunState) store.Run {
	t.Helper()
	r := store.Run{
		ID:         id,
		Repo:       "app",
		Job:        "test",
		Ref:        "main",
		State:      state,
		OriginKind: "cli",
		CreatedAt:  time.Now(),
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

// seedParkedRun is a run in awaiting_approval with a commit already resolved,
// which is the only shape gate 8 can produce (amendment A27) and the only one
// `latem approve` can act on.
func seedParkedRun(t *testing.T, db *store.DB, id string) store.Run {
	t.Helper()
	r := store.Run{
		ID:            id,
		Repo:          "app",
		Job:           "test",
		Ref:           "feature/x",
		RefNormalized: "refs/heads/feature/x",
		SHA:           "0123456789abcdef0123456789abcdef01234567",
		State:         protocol.StateAwaitingApproval,
		OriginKind:    "cli",
		CreatedAt:     time.Now(),
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

// waitFor polls until cond holds, and fails with why if it never does.
func waitFor(t *testing.T, why string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, why)
}

// hasGit reports whether the machine can build a git fixture. The end-to-end
// tests need a real repository because INV-5 reads jobs.yaml off a checkout,
// and a machine without git cannot run them.
func hasGit(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("git")
	return err == nil
}

// gitEnv pins away the developer's own gitconfig, for the same reasons
// internal/source's helpers do: init.defaultBranch or commit.gpgsign would
// otherwise produce failures that reproduce nowhere else.
func gitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=latem test",
		"GIT_AUTHOR_EMAIL=test@latem.invalid",
		"GIT_COMMITTER_NAME=latem test",
		"GIT_COMMITTER_EMAIL=test@latem.invalid",
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a repository on main carrying a .latem/jobs.yaml.
func newRepo(t *testing.T, dir, jobsYAML string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".latem"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".latem", "jobs.yaml"), []byte(jobsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "jobs")
	return runGit(t, dir, "rev-parse", "HEAD")
}

var errBoom = errors.New("boom")

// statPath is os.Stat under a name that reads as an assertion at the call
// site rather than as a filesystem call.
func statPath(path string) (os.FileInfo, error) { return os.Stat(path) }

// listenOn holds a TCP address so a test can prove the agent survives a
// metrics port it cannot bind.
func listenOn(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
