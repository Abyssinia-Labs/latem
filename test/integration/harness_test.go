//go:build integration

// Package integration is RUNNER_SPEC §24's cross-cutting matrix.
//
// It is build-tagged so the default gate stays daemon-free:
//
//	go test -tags=integration ./test/...
//
// # What is here and what is deliberately not
//
// Several §24 cases already exist as package tests and are not repeated:
// internal/executor/docker covers step execution, $LATEM_ENV, a step timeout at
// the executor level, and container labelling; internal/artifact covers glob
// resolution and symlink escapes, which is where they now live in full since
// amendment A29 removed the executors' unused second copy; internal/agent
// covers `kill -9` leaving no orphans; internal/cache covers volume lifecycle
// and pruning. Each of those is a property of one package and belongs with it.
//
// What is here is what no single package can assert: a whole run driven through
// a real agent, a real git repository, a real Docker daemon and a real
// registry read off a real checkout — and, in two cases, the same claim made
// against two executors at once so that the answer can be compared rather than
// merely observed.

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/executor/docker"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// testImage carries git and a POSIX shell and is 25 MB.
//
// It is not the base image RUNNER_SPEC §13.1 describes, and it does not have
// to be: what these tests assert is the agent's behaviour, and pinning a
// several-hundred-megabyte image with bash in it would make the suite slow
// without making it say anything more. The shell is `sh` for the same reason —
// the executor takes it as an option precisely so the argv contract can be
// exercised against whatever an image actually ships.
const (
	testImage = "alpine/git:latest"
	testShell = "sh"
)

// requireDocker skips when there is no daemon.
//
// A skip rather than a failure, because the tag already says this suite needs
// one and a developer running `-tags=integration` on a machine where Docker is
// merely not started should be told that rather than shown eighteen failures.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").
		CombinedOutput(); err != nil {
		t.Skipf("no docker daemon: %v\n%s", err, out)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; the registry is read from a real checkout (INV-5)")
	}
}

// ---------------------------------------------------------------------------
// a machine

type machine struct {
	t    *testing.T
	cfg  *config.Config
	db   *store.DB
	repo string
	home string
	// prepared counts environments the executor provisioned, for the tests
	// that care whether anything started at all.
	prepared *countingExecutor
}

type machineOptions struct {
	// jobs is the .latem/jobs.yaml committed at the fixture repository's first
	// commit.
	jobs string
	// files are extra files to commit, relative to the repository root.
	files map[string]string
	// executorKind is "docker" or "native".
	executorKind string
	// concurrency is the runner's slot count.
	concurrency int
	// grace is agent.shutdown_grace.
	grace  string
	mutate func(*agent.Options)
}

func newMachine(t *testing.T, opts machineOptions) *machine {
	t.Helper()
	requireGit(t)
	if opts.executorKind == "" {
		opts.executorKind = "docker"
	}
	if opts.executorKind == "docker" {
		requireDocker(t)
	}
	if opts.concurrency == 0 {
		opts.concurrency = 1
	}
	if opts.grace == "" {
		opts.grace = "10s"
	}

	home := shortTemp(t)
	repo := shortTemp(t)
	initRepo(t, repo, opts.jobs, opts.files)

	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	var runnerBlock string
	switch opts.executorKind {
	case "docker":
		runnerBlock = fmt.Sprintf(`
[[runner]]
name        = "it"
executor    = "docker"
labels      = ["test", "linux"]
concurrency = %d

  [runner.docker]
  default_image = %q
  min_free_gb   = 0.001
`, opts.concurrency, testImage)
	case "native":
		runnerBlock = `
[[runner]]
name        = "it"
executor    = "native"
labels      = ["test", "native"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`
	default:
		t.Fatalf("unknown executor kind %q", opts.executorKind)
	}

	body := fmt.Sprintf(`schema_version = 1

[machine]
name = "integration"
id   = "mch_integration"

[agent]
shutdown_grace = %q
queue_ttl      = "72h"

[broker.local]
enabled = true

[repos.app]
url          = %q
allowed_refs = ["main", "refs/tags/*"]
%s`, opts.grace, repo, runnerBlock)

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

	counter := &countingExecutor{}
	agentOpts := agent.Options{
		Config:      cfg,
		Version:     "integration",
		Logger:      testLogger(t),
		Secrets:     noSecrets{},
		NewExecutor: newExecutorFor(t, counter),

		HeartbeatInterval:   time.Hour,
		PreflightInterval:   time.Hour,
		MaintenanceInterval: time.Hour,
		ClaimInterval:       50 * time.Millisecond,
		SlotWait:            2 * time.Minute,
		PreflightTimeout:    60 * time.Second,
		TeardownGrace:       2 * time.Minute,
	}
	if opts.mutate != nil {
		opts.mutate(&agentOpts)
	}

	a, err := agent.New(agentOpts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var runErr error
	go func() {
		defer close(stopped)
		runErr = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(4 * time.Minute):
			t.Error("the agent did not shut down")
		}
	})

	select {
	case <-a.Ready():
	case <-stopped:
		cancel()
		t.Fatalf("the agent exited during startup: %v", runErr)
	case <-time.After(3 * time.Minute):
		cancel()
		t.Fatal("the agent never became ready")
	}

	m := &machine{t: t, cfg: cfg, repo: repo, home: home, prepared: counter}
	m.db = m.openDB()
	return m
}

// newExecutorFor builds the real executor a runner names, with the test image
// and shell wired in.
//
// It goes through agent.Options.NewExecutor rather than through
// agent.DefaultExecutor because DefaultExecutor hard-codes bash — correctly,
// since §13.1's base image contract requires it — and the 25 MB image these
// tests use has only sh. Everything else is DefaultExecutor's own construction:
// a real docker.Executor talking to a real daemon, or a real native.Executor
// spawning real processes.
func newExecutorFor(t *testing.T, counter *countingExecutor) func(*config.Runner, config.Paths) (executor.Executor, error) {
	return func(r *config.Runner, paths config.Paths) (executor.Executor, error) {
		switch r.Executor {
		case config.ExecutorDocker:
			image := testImage
			if r.Docker != nil && r.Docker.DefaultImage != "" {
				image = r.Docker.DefaultImage
			}
			e, err := docker.New(docker.Options{
				DefaultImage: image,
				Shell:        testShell,
				// Alpine has no uid-1000 user, so forcing the host uid leaves
				// $HOME unwritable. §13.1's Linux uid mapping is an operator
				// concern and is the docker package's own test to make.
				User: docker.UserImageDefault,
			})
			if err != nil {
				return nil, err
			}
			t.Cleanup(func() { _ = e.Close() })
			counter.wrap(e)
			return counter, nil
		default:
			e, err := agent.DefaultExecutor(r, paths)
			if err != nil {
				return nil, err
			}
			counter.wrap(e)
			return counter, nil
		}
	}
}

// countingExecutor is a pass-through that records how many environments were
// provisioned, so a test can distinguish "nothing bad happened" from "nothing
// happened".
type countingExecutor struct {
	mu    sync.Mutex
	inner executor.Executor
	count int
}

var _ executor.Executor = (*countingExecutor)(nil)

func (c *countingExecutor) wrap(e executor.Executor) {
	c.mu.Lock()
	c.inner = e
	c.mu.Unlock()
}

func (c *countingExecutor) delegate() executor.Executor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner
}

func (c *countingExecutor) Name() string { return c.delegate().Name() }

func (c *countingExecutor) Preflight(ctx context.Context) error {
	return c.delegate().Preflight(ctx)
}

func (c *countingExecutor) Prepare(ctx context.Context, spec executor.Spec) (executor.Env, error) {
	env, err := c.delegate().Prepare(ctx, spec)
	if err == nil {
		c.mu.Lock()
		c.count++
		c.mu.Unlock()
	}
	return env, err
}

func (c *countingExecutor) Reap(ctx context.Context, live func(string) bool) ([]executor.Reaped, error) {
	return c.delegate().Reap(ctx, live)
}

func (c *countingExecutor) prepares() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// ---------------------------------------------------------------------------

func (m *machine) openDB() *store.DB {
	m.t.Helper()
	db, err := store.Open(context.Background(), m.cfg.Paths.DB, passthroughRedactor{})
	if err != nil {
		m.t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { _ = db.Close() })
	return db
}

type passthroughRedactor struct{}

func (passthroughRedactor) Redact(line string) string { return line }

// submit sends a run down the local socket, exactly as `latem run` does.
func (m *machine) submit(t *testing.T, id, job, ref string, inputs map[string]string) {
	t.Helper()
	client, err := local.NewClient(local.ClientOptions{Socket: m.cfg.Paths.Socket, Store: m.db})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := client.Submit(context.Background(), protocol.RunRequest{
		ID: id, Repo: "app", Job: job, Ref: ref, Inputs: inputs,
		Origin: protocol.Origin{Kind: "cli", Actor: "integration"},
	}, local.SubmitOptions{})
	if err != nil {
		t.Fatalf("submitting %s: %v", id, err)
	}
	if !sub.AgentRunning {
		t.Fatalf("the agent was up but %s took the queue-only path", id)
	}
}

func (m *machine) cancel(t *testing.T, id, reason string) {
	t.Helper()
	client, err := local.NewClient(local.ClientOptions{Socket: m.cfg.Paths.Socket, Store: m.db})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Cancel(context.Background(),
		protocol.CancelRequest{ID: id, Actor: "integration", Reason: reason}); err != nil {
		t.Fatalf("cancelling %s: %v", id, err)
	}
}

// terminal blocks until a run reaches a terminal state.
func (m *machine) terminal(t *testing.T, id string, timeout time.Duration) store.Run {
	t.Helper()
	return m.waitForState(t, id, timeout,
		protocol.StateSucceeded, protocol.StateFailed, protocol.StateRejected,
		protocol.StateTimedOut, protocol.StateCancelled, protocol.StateInterrupted)
}

func (m *machine) waitForState(t *testing.T, id string, timeout time.Duration,
	want ...protocol.RunState) store.Run {
	t.Helper()
	var last store.Run
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := m.db.Get(context.Background(), id)
		if err == nil {
			last = r
			for _, w := range want {
				if r.State == w {
					return r
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	names := make([]string, len(want))
	for i, w := range want {
		names[i] = string(w)
	}
	t.Fatalf("run %s never reached %s; it is %s (%s: %s)\n--- log ---\n%s",
		id, strings.Join(names, " or "), last.State, last.ErrorCode, last.ErrorMsg, m.log(id))
	return last
}

// log is the run's persisted output, for a failure message that says what the
// build was doing rather than only that it did not finish.
func (m *machine) log(id string) string {
	b, err := os.ReadFile(filepath.Join(m.cfg.Paths.Logs, id+".log"))
	if err != nil {
		return "(no log file: " + err.Error() + ")"
	}
	return string(b)
}

func (m *machine) artifacts(t *testing.T, id string) []protocol.ArtifactRef {
	t.Helper()
	refs, err := m.db.Artifacts(context.Background(), id)
	if err != nil {
		t.Fatalf("reading artifacts for %s: %v", id, err)
	}
	return refs
}

func (m *machine) artifactPaths(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, r := range m.artifacts(t, id) {
		out = append(out, r.Path)
	}
	return out
}

// ---------------------------------------------------------------------------
// git

func hermeticGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=latem integration",
		"GIT_AUTHOR_EMAIL=integration@latem.invalid",
		"GIT_COMMITTER_NAME=latem integration",
		"GIT_COMMITTER_EMAIL=integration@latem.invalid",
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hermeticGitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T, dir, jobs string, files map[string]string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".latem", "jobs.yaml"), jobs)
	for rel, body := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "fixture")
	// An annotated tag, so `git describe` inside the container has something
	// to find. Without one it exits 128 and the F2 regression test would be
	// asserting against git's own failure rather than against the workspace.
	git(t, dir, "tag", "-a", "v1.0.0", "-m", "the tag git describe reports")
	return git(t, dir, "rev-parse", "HEAD")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------

func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
	defer func() {
		if recover() != nil {
			w.done = true
		}
	}()
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

type noSecrets struct{}

func (noSecrets) Name() string { return "test" }

func (noSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	return nil, fmt.Errorf("test: %s: %w", name, secrets.ErrNotFound)
}

// execDocker runs the docker CLI and returns its combined output.
//
// The CLI rather than the API client, deliberately: the assertions that use it
// are about whether a resource still exists on the daemon, and asking the same
// library the code under test uses would make a mistake in that library
// invisible to both.
func execDocker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}
