package invariants

// The cross-package harness for INV-2, INV-3 and INV-5.
//
// Each of those three invariants is a claim about what happens when several
// packages are wired together, so none of them can be asserted from inside one
// of them. INV-2 says no execution occurs outside the registry-declared
// command set, which needs a broker, an admission chain and an executor in the
// same process. INV-5 says the registry comes off the checked-out workspace at
// the resolved commit, which is only observable if there is a real git
// repository with two different registries at two different commits.
//
// So everything here is real: a real config on disk at the modes the loader
// insists on, a real sqlite database, a real Unix socket, a real git
// repository, and a real agent running §6 in this process. The one substituted
// piece is the executor, through the [agent.Options.NewExecutor] seam that
// already exists for exactly this — and it is substituted by something that
// *records* rather than by something that fakes an answer, because the
// assertion these tests make is about what was asked of it.

import (
	"context"
	"encoding/json"
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

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ---------------------------------------------------------------------------
// the executor that records instead of executing

// recordedStep is one call to [executor.Env.Exec], with the environment the
// step would have seen.
//
// The environment is captured because it is the other half of INV-2: a hostile
// value is allowed to reach an environment map and is not allowed to reach a
// command. A recorder that only kept the command could confirm the second and
// say nothing about the first, which would let a run that dropped the input
// entirely pass as compliant.
type recordedStep struct {
	RunID string
	Step  executor.Step
	Env   map[string]string
}

// recordingExecutor satisfies [executor.Executor] and runs nothing.
//
// It is not a mock in the sense of a thing that asserts it was called. It is a
// tap on the one interface every command in the system must pass through on
// its way to a process, which is what makes "no execution occurs outside the
// registry-declared command set" a checkable sentence rather than a hope: the
// declared set is a handful of literals in a committed jobs.yaml, and this
// records every string that was ever offered as a command.
type recordingExecutor struct {
	name string
	dir  string

	mu       sync.Mutex
	hold     time.Duration
	exitCode int
	// leaky makes a step behave like a build with `set -x` left on: it writes
	// every injected secret to stdout and stderr and then fails with one of
	// them in the error text. Both are ordinary things for real tooling to do,
	// and both are what INV-4's redactor exists for.
	leaky  bool
	specs  []executor.Spec
	steps  []recordedStep
	leaked []string
}

var _ executor.Executor = (*recordingExecutor)(nil)

func newRecordingExecutor(t *testing.T) *recordingExecutor {
	t.Helper()
	return &recordingExecutor{name: "native", dir: t.TempDir()}
}

func (r *recordingExecutor) Name() string { return r.name }

func (r *recordingExecutor) Preflight(context.Context) error { return nil }

func (r *recordingExecutor) Prepare(_ context.Context, spec executor.Spec) (executor.Env, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return &recordingEnv{ex: r, spec: spec}, nil
}

func (r *recordingExecutor) Reap(context.Context, func(string) bool) ([]executor.Reaped, error) {
	return nil, nil
}

// commands returns every string ever offered to this executor as a command.
func (r *recordingExecutor) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.steps))
	for _, s := range r.steps {
		out = append(out, s.Step.Run)
	}
	return out
}

func (r *recordingExecutor) recorded() []recordedStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedStep(nil), r.steps...)
}

func (r *recordingExecutor) prepared() []executor.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]executor.Spec(nil), r.specs...)
}

func (r *recordingExecutor) setLeaky() {
	r.mu.Lock()
	r.leaky = true
	r.mu.Unlock()
}

// emitted is everything a leaky step wrote, so a test can prove the pipeline
// was given something to redact before asserting that it redacted it.
func (r *recordingExecutor) emitted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.leaked...)
}

// injectedSecrets is what §15 actually resolved into the run environment,
// keyed by the names the job declared (amendment A10's Spec.Secrets).
func (r *recordingExecutor) injectedSecrets() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for _, spec := range r.specs {
		for _, name := range spec.Secrets {
			out[name] = spec.Env[name]
		}
	}
	return out
}

type recordingEnv struct {
	ex   *recordingExecutor
	spec executor.Spec
}

func (e *recordingEnv) Exec(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (int, error) {
	env := map[string]string{}
	for k, v := range e.spec.Env {
		env[k] = v
	}
	for k, v := range step.Env {
		env[k] = v
	}

	e.ex.mu.Lock()
	e.ex.steps = append(e.ex.steps, recordedStep{RunID: e.spec.RunID, Step: step, Env: env})
	hold, code, leaky := e.ex.hold, e.ex.exitCode, e.ex.leaky
	e.ex.mu.Unlock()

	_, _ = io.WriteString(stdout, "ran step "+step.Name+"\n")

	if leaky {
		return e.leak(stdout, stderr)
	}

	if hold > 0 {
		select {
		case <-time.After(hold):
		case <-ctx.Done():
			return 143, ctx.Err()
		}
	}
	return code, nil
}

// leak writes every injected secret to both streams and then fails with one of
// them in the error text.
//
// Both streams, because §16 puts the redactor at the point of emission rather
// than at the sink, and a pipeline that redacted stdout and forwarded stderr
// raw would be exactly the kind of bypass that sentence forbids. The error is
// the second half: RUNNER_SPEC §11 has ErrorMsg reaching a broker and sqlite,
// and it is the surface a redactor sitting only in the log path would miss.
func (e *recordingEnv) leak(stdout, stderr io.Writer) (int, error) {
	var leaked []string
	for _, name := range e.spec.Secrets {
		v := e.spec.Env[name]
		if v == "" {
			continue
		}
		leaked = append(leaked, v)
		_, _ = io.WriteString(stdout, "+ echo \"$"+name+"\"\n"+v+"\n")
		_, _ = io.WriteString(stderr, name+"="+v+"\n")
	}
	_, _ = io.WriteString(stdout, "BUILD OUTPUT MARKER\n")

	e.ex.mu.Lock()
	e.ex.leaked = append(e.ex.leaked, leaked...)
	e.ex.mu.Unlock()

	if len(leaked) == 0 {
		return 1, nil
	}
	return 1, fmt.Errorf("the build driver failed and put its environment in the message: %s",
		strings.Join(leaked, " "))
}

func (e *recordingEnv) EnvFile() string {
	return filepath.Join(e.ex.dir, e.spec.RunID+".env")
}

func (e *recordingEnv) Close(context.Context) error { return nil }

// noSecrets refuses every name, which is right for jobs that declare none and
// makes it impossible for one of these tests to reach a real credential store.
type noSecrets struct{}

func (noSecrets) Name() string { return "test" }

func (noSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	return nil, fmt.Errorf("test: %s: %w", name, secrets.ErrNotFound)
}

// ---------------------------------------------------------------------------
// a machine

// machine is one agent, its config, its database, and the git repository its
// one configured repo points at.
type machine struct {
	t    *testing.T
	cfg  *config.Config
	db   *store.DB
	exec *recordingExecutor
	repo string // the upstream working tree, so a test can move a ref
	home string
}

// machineOptions varies the parts of the fixture individual tests care about.
type machineOptions struct {
	// jobs is the .latem/jobs.yaml committed at the repository's first commit.
	jobs string
	// allowedRefs is `[repos.app] allowed_refs`. Empty means ["main"].
	allowedRefs string
	// approvalTTL and queueTTL are the §11 deadlines.
	approvalTTL string
	queueTTL    string
	// mutate adjusts the agent options after the defaults are applied.
	mutate func(*agent.Options)
}

// newMachine builds the fixture and starts the agent.
//
// The repository is real because INV-5 requires it: the registry is read from
// the checked-out workspace at the resolved commit, so a test that wanted a
// job to exist had to commit one. That is also what makes these tests slow
// enough to be worth writing once, in a harness, rather than per case.
func newMachine(t *testing.T, opts machineOptions) *machine {
	t.Helper()
	requireGit(t)

	if opts.jobs == "" {
		opts.jobs = declaredJobs
	}
	if opts.allowedRefs == "" {
		opts.allowedRefs = `["main"]`
	}
	if opts.approvalTTL == "" {
		opts.approvalTTL = "24h"
	}
	if opts.queueTTL == "" {
		opts.queueTTL = "72h"
	}

	home := shortTemp(t)
	repo := shortTemp(t)
	initRepo(t, repo, opts.jobs)

	dir := filepath.Join(home, ".latem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`schema_version = 1

[machine]
name = "invariants"
id   = "mch_invariants"

[agent]
shutdown_grace = "5s"
queue_ttl      = %q

[broker.local]
enabled = true

[repos.app]
url                        = %q
allowed_refs               = %s
require_approval_otherwise = true
approval_ttl               = %q

[[runner]]
name        = "inv"
executor    = "native"
labels      = ["test"]
concurrency = 1

  [runner.native]
  min_free_gb = 0.001
`, opts.queueTTL, repo, opts.allowedRefs, opts.approvalTTL)

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

	rec := newRecordingExecutor(t)
	agentOpts := agent.Options{
		Config:  cfg,
		Version: "invariants",
		Logger:  quietLogger(t),
		Secrets: noSecrets{},
		NewExecutor: func(*config.Runner, config.Paths) (executor.Executor, error) {
			return rec, nil
		},
		HeartbeatInterval:   time.Hour,
		PreflightInterval:   time.Hour,
		MaintenanceInterval: time.Hour,
		ClaimInterval:       20 * time.Millisecond,
		SlotWait:            30 * time.Second,
		PreflightTimeout:    10 * time.Second,
		TeardownGrace:       10 * time.Second,
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
		case <-time.After(90 * time.Second):
			t.Error("the agent did not shut down")
		}
	})

	select {
	case <-a.Ready():
	case <-stopped:
		cancel()
		t.Fatalf("the agent exited during startup: %v", runErr)
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatal("the agent never became ready")
	}

	m := &machine{t: t, cfg: cfg, exec: rec, repo: repo, home: home}
	m.db = m.openDB()
	return m
}

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

// submit sends one command down the local socket as `latem run` does, and
// returns the broker's answer.
//
// It writes the frame itself rather than going through local.NewClient because
// several of these tests need to send bytes a client would refuse to
// construct: an unknown kind, a smuggled field, a payload that does not match
// its kind. The framing is newline-delimited JSON by specification (§7.3), so
// a raw connection is a faithful client rather than a shortcut.
func (m *machine) submit(t *testing.T, frame string) socketReply {
	t.Helper()
	conn, err := net.DialTimeout("unix", m.cfg.Paths.Socket, 5*time.Second)
	if err != nil {
		t.Fatalf("dialling the agent socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, frame+"\n"); err != nil {
		t.Fatalf("writing a command frame: %v", err)
	}

	buf := make([]byte, 64<<10)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		// A refusal the broker answers by closing without a line is still a
		// refusal, and one of the hostile frames below is oversized enough to
		// produce exactly that.
		return socketReply{OK: false, Raw: "", ReadErr: err}
	}
	return parseReply(t, strings.TrimSpace(string(buf[:n])))
}

// socketReply is the local broker's answer. Its `reply` type is unexported —
// deliberately, and recorded as a contract defect in that package — so this is
// decoded structurally rather than into the type that produced it.
type socketReply struct {
	OK      bool
	RunID   string
	Error   string
	Raw     string
	ReadErr error
}

func parseReply(t *testing.T, line string) socketReply {
	t.Helper()
	r := socketReply{Raw: line}
	if line == "" {
		return r
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("the broker answered something that is not JSON: %q", line)
	}
	if v, ok := m["ok"].(bool); ok {
		r.OK = v
	}
	if v, ok := m["run_id"].(string); ok {
		r.RunID = v
	}
	if v, ok := m["error"].(string); ok {
		r.Error = v
	}
	return r
}

// waitForState blocks until a run reaches one of the given states.
func (m *machine) waitForState(t *testing.T, id string, timeout time.Duration, want ...protocol.RunState) store.Run {
	t.Helper()
	var last store.Run
	var lastErr error
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := m.db.Get(context.Background(), id)
		if err != nil {
			lastErr = err
		} else {
			last = r
			for _, w := range want {
				if r.State == w {
					return r
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	names := make([]string, len(want))
	for i, w := range want {
		names[i] = string(w)
	}
	t.Fatalf("run %s never reached %s; it is %s (%s: %s) [last error: %v]",
		id, strings.Join(names, " or "), last.State, last.ErrorCode, last.ErrorMsg, lastErr)
	return last
}

// get reads a run row, or fails.
func (m *machine) get(t *testing.T, id string) store.Run {
	t.Helper()
	r, err := m.db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("reading run %s: %v", id, err)
	}
	return r
}

// ---------------------------------------------------------------------------
// the registry the whole of INV-2 is measured against

// declaredCommands is the closed set. Every string a job in [declaredJobs]
// declares as a `run:`, and nothing else, may ever reach an executor.
var declaredCommands = []string{
	"./ci/build.sh",
	"./ci/test.sh",
}

// declaredJobs is the committed .latem/jobs.yaml.
//
// The `run:` strings are deliberately boring and deliberately few. INV-2's
// claim is a set membership, and a set with two elements in it makes a
// violation obvious in a failure message rather than something a reader has to
// diff. The input exists so a hostile value has somewhere legitimate to land:
// §5 permits it in an environment map and forbids it in a command.
const declaredJobs = `version: 1

jobs:
  build:
    description: the only job this repository declares
    runs_on: [test]
    timeout: 5m
    inputs:
      channel:
        type: string
        max_length: 8192
        default: "internal"
    steps:
      - name: build
        run: ./ci/build.sh
        env:
          RELEASE_CHANNEL: ${input.channel}
      - name: test
        run: ./ci/test.sh
`

// ---------------------------------------------------------------------------
// git

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; INV-5 reads jobs.yaml off a real checkout")
	}
}

// hermeticGitEnv pins away the developer's own gitconfig. init.defaultBranch
// or commit.gpgsign in a personal config produces failures that reproduce on
// exactly one machine.
func hermeticGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=latem invariants",
		"GIT_AUTHOR_EMAIL=invariants@latem.invalid",
		"GIT_COMMITTER_NAME=latem invariants",
		"GIT_COMMITTER_EMAIL=invariants@latem.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
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

// initRepo creates a repository on main carrying .latem/jobs.yaml and the
// scripts the declared jobs name.
func initRepo(t *testing.T, dir, jobs string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".latem", "jobs.yaml"), jobs)
	writeFile(t, filepath.Join(dir, "ci", "build.sh"), "#!/bin/sh\necho build\n")
	writeFile(t, filepath.Join(dir, "ci", "test.sh"), "#!/bin/sh\necho test\n")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "declare the only jobs this repository has")
	return git(t, dir, "rev-parse", "HEAD")
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// odds and ends

// shortTemp is t.TempDir() with a length budget.
//
// A Unix socket path is capped at 103 bytes by the kernel, and the default
// temporary directory on macOS is longer than that before anything is joined
// to it. config.Load rejects the overrun with a clear message, so a test using
// t.TempDir() would fail on its own scratch space rather than on the invariant
// it was written for.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "inv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// quietLogger routes the agent's narration into the test log, so a failure
// carries the agent's own account of what it was doing.
func quietLogger(t *testing.T) *slog.Logger {
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
	// A goroutine still logging after the test returned would panic on t.Log.
	// The agent's loops are stopped by Cleanup, but the race is real.
	defer func() {
		if recover() != nil {
			w.done = true
		}
	}()
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
