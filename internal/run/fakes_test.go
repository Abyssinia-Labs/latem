package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ---------------------------------------------------------------- source ---

// fakeSource stands in for *source.Manager. It builds a real directory tree
// for each workspace, because gate 11 reads .latem/jobs.yaml off disk and a
// mock that returned bytes would test the wrong thing (INV-5).
type fakeSource struct {
	mu sync.Mutex

	root     string
	registry string // .latem/jobs.yaml content; empty writes no file at all
	refs     map[string]bool
	sha      string

	fetchErr   error
	acquireErr error

	fetched    []source.Repo
	workspaces []string
}

func newFakeSource(t *testing.T, registryYAML string) *fakeSource {
	t.Helper()
	return &fakeSource{
		root:     t.TempDir(),
		registry: registryYAML,
		refs:     map[string]bool{"refs/heads/main": true},
		sha:      "0123456789abcdef0123456789abcdef01234567",
	}
}

func (s *fakeSource) Fetch(_ context.Context, repo source.Repo) (Mirror, error) {
	s.mu.Lock()
	s.fetched = append(s.fetched, repo)
	err := s.fetchErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &fakeMirror{src: s}, nil
}

type fakeMirror struct{ src *fakeSource }

func (m *fakeMirror) HasRef(_ context.Context, fullyQualified string) (bool, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	return m.src.refs[fullyQualified], nil
}

func (m *fakeMirror) Acquire(_ context.Context, runID, normalizedRef string) (*source.Workspace, error) {
	s := m.src
	s.mu.Lock()
	err := s.acquireErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(s.root, runID)
	if mkErr := os.MkdirAll(filepath.Join(path, ".latem"), 0o750); mkErr != nil {
		return nil, mkErr
	}
	if s.registry != "" {
		file := filepath.Join(path, ".latem", "jobs.yaml")
		if wErr := os.WriteFile(file, []byte(s.registry), 0o600); wErr != nil {
			return nil, wErr
		}
	}

	s.mu.Lock()
	s.workspaces = append(s.workspaces, path)
	s.mu.Unlock()

	return &source.Workspace{
		Path: path, RunID: runID, Repo: "app", Ref: normalizedRef, SHA: s.sha,
	}, nil
}

// -------------------------------------------------------------- executor ---

type fakeEnv struct {
	mu      sync.Mutex
	spec    executor.Spec
	envFile string
	steps   []executor.Step
	closes  int

	exec func(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (int, error)
}

func (e *fakeEnv) Exec(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (int, error) {
	e.mu.Lock()
	e.steps = append(e.steps, step)
	fn := e.exec
	e.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, step, stdout, stderr)
}

func (e *fakeEnv) EnvFile() string {
	if e.envFile == "" {
		return "/tmp/latem/env"
	}
	return e.envFile
}

func (e *fakeEnv) Fetch(context.Context, string) ([]executor.File, error) { return nil, nil }

func (e *fakeEnv) Close(context.Context) error {
	e.mu.Lock()
	e.closes++
	e.mu.Unlock()
	return nil
}

func (e *fakeEnv) closeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closes
}

func (e *fakeEnv) executed() []executor.Step {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]executor.Step(nil), e.steps...)
}

func (e *fakeEnv) runSpec() executor.Spec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spec
}

type fakeExecutor struct {
	env        *fakeEnv
	prepareErr error
	prepared   int
	mu         sync.Mutex
}

func (f *fakeExecutor) Prepare(_ context.Context, spec executor.Spec) (executor.Env, error) {
	f.mu.Lock()
	f.prepared++
	err := f.prepareErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f.env.mu.Lock()
	f.env.spec = spec
	f.env.mu.Unlock()
	return f.env, nil
}

func (f *fakeExecutor) Name() string { return "fake" }

// Reap satisfies executor.Executor (amendment A23). The orchestrator does not
// call it — startup reconciliation belongs to the agent (§6 step 5) — so this
// records the call and reports nothing to clean.
func (f *fakeExecutor) Reap(context.Context, func(string) bool) ([]executor.Reaped, error) {
	return nil, nil
}

func (f *fakeExecutor) Preflight(context.Context) error { return nil }

// ---------------------------------------------------------------- broker ---

type reported struct {
	runID  string
	update protocol.Update
}

type fakeBroker struct {
	mu       sync.Mutex
	reports  []reported
	frames   []protocol.LogFrame
	failNext int
	logsErr  error
	acked    uint64
}

func (b *fakeBroker) Commands(context.Context) (<-chan protocol.Command, error) {
	return nil, errors.ErrUnsupported
}
func (b *fakeBroker) Claim(context.Context, string) error { return nil }

func (b *fakeBroker) Report(_ context.Context, runID string, u protocol.Update) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failNext > 0 {
		b.failNext--
		return errors.New("broker unavailable")
	}
	b.reports = append(b.reports, reported{runID, u})
	return nil
}

func (b *fakeBroker) Logs(context.Context, string) (broker.LogSink, error) {
	if b.logsErr != nil {
		return nil, b.logsErr
	}
	return &fakeLogSink{b: b}, nil
}

func (b *fakeBroker) Heartbeat(context.Context, protocol.MachineHealth) error { return nil }

// Close satisfies broker.Broker (amendment A19).
func (b *fakeBroker) Close() error { return nil }

func (b *fakeBroker) states(runID string) []protocol.RunState {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []protocol.RunState
	for _, r := range b.reports {
		if r.runID == runID {
			out = append(out, r.update.State)
		}
	}
	return out
}

func (b *fakeBroker) updates() []reported {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]reported(nil), b.reports...)
}

func (b *fakeBroker) lines() []protocol.LogFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]protocol.LogFrame(nil), b.frames...)
}

type fakeLogSink struct{ b *fakeBroker }

func (s *fakeLogSink) Write(_ context.Context, frames []protocol.LogFrame) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	s.b.frames = append(s.b.frames, frames...)
	for _, f := range frames {
		if f.Seq > s.b.acked {
			s.b.acked = f.Seq
		}
	}
	return nil
}

func (s *fakeLogSink) Acked() uint64 {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.b.acked
}

func (s *fakeLogSink) Close(context.Context) error { return nil }

// ----------------------------------------------------------------- cache ---

type fakeCache struct {
	mu sync.Mutex

	free       float64
	freeErr    error
	acquireErr error

	acquired []cache.Request
	released [][]string
}

func (c *fakeCache) Acquire(_ context.Context, _ string, reqs []cache.Request) ([]executor.CacheMount, error) {
	c.mu.Lock()
	c.acquired = append(c.acquired, reqs...)
	err := c.acquireErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]executor.CacheMount, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, executor.CacheMount{Name: r.Name, Source: "vol-" + r.Name, Target: r.Target})
	}
	return out, nil
}

func (c *fakeCache) Release(_ context.Context, _ string, names []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, names)
	return nil
}

func (c *fakeCache) List(context.Context) ([]cache.Info, error) { return nil, nil }

func (c *fakeCache) Prune(context.Context, cache.PruneOptions) (cache.Report, error) {
	return cache.Report{}, nil
}

func (c *fakeCache) FreeGB(context.Context) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.free, c.freeErr
}

func (c *fakeCache) releaseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.released)
}

func (c *fakeCache) requests() []cache.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cache.Request(nil), c.acquired...)
}

// --------------------------------------------------------------- secrets ---

type fakeSecrets struct {
	values map[string]string
	err    error
}

func (s *fakeSecrets) Name() string { return "fake" }

func (s *fakeSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[name]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return secrets.Value(v), nil
}

// ----------------------------------------------------------------- store ---

// errStore wraps a real store and fails one named method, for the paths a
// working database cannot reach.
type errStore struct {
	Store
	failTransitionTo protocol.RunState
	failEnqueue      bool
}

func (s *errStore) Transition(
	ctx context.Context, runID string, to protocol.RunState, u protocol.Update,
) error {
	if s.failTransitionTo != "" && to == s.failTransitionTo {
		return errors.New("store is down")
	}
	return s.Store.Transition(ctx, runID, to, u)
}

func (s *errStore) Enqueue(ctx context.Context, runID string, priority int) error {
	if s.failEnqueue {
		return errors.New("store is down")
	}
	return s.Store.Enqueue(ctx, runID, priority)
}

// (approvingStore lived here. It faked the preparing → awaiting_approval edge
// that internal/store was missing, so the orchestrator's correct path could be
// tested before the edge existed. Amendment A17 added the edge, so the real
// store now serves and the shim is gone.)

// --------------------------------------------------------------- harness ---

const defaultRegistryYAML = `
version: 1
defaults:
  timeout: 10m
caches:
  - name: pnpm
    path: /caches/pnpm
jobs:
  test:
    description: Unit tests
    runs_on: [linux, docker]
    timeout: 10m
    caches: [pnpm]
    artifacts:
      - path: out/*.txt
        retain: 7d
    steps:
      - name: build
        run: ./ci/build.sh
      - name: verify
        run: ./ci/verify.sh
  secretive:
    runs_on: [linux]
    secrets: [MATCH_PASSWORD]
    steps:
      - name: sign
        run: ./ci/sign.sh
  grouped:
    runs_on: [linux]
    concurrency_group: ios
    steps:
      - name: only
        run: ./ci/only.sh
  macos:
    runs_on: [macos, xcode]
    steps:
      - name: only
        run: ./ci/only.sh
`

type harness struct {
	t *testing.T

	dir    string
	cfg    *config.Config
	db     *store.DB
	broker *fakeBroker
	source *fakeSource
	secret *fakeSecrets
	caches *fakeCache
	exec   *fakeExecutor
	env    *fakeEnv

	runners []Runner
	opts    Options
	// brokerless builds an orchestrator with no broker at all, which is the
	// local-only mode RUNNER_SPEC §7.3 describes.
	brokerless bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	red := logstream.NewRedactor()
	db, err := store.Open(t.Context(), filepath.Join(dir, "state.db"), red)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	env := &fakeEnv{}
	h := &harness{
		t:      t,
		dir:    dir,
		db:     db,
		broker: &fakeBroker{},
		source: newFakeSource(t, defaultRegistryYAML),
		secret: &fakeSecrets{values: map[string]string{"MATCH_PASSWORD": "correct horse battery staple"}},
		caches: &fakeCache{free: 500},
		env:    env,
		exec:   &fakeExecutor{env: env},
	}
	h.cfg = &config.Config{
		Repos: map[string]*config.Repo{
			"app": {
				URL:                      "https://git.example.invalid/app.git",
				AllowedRefs:              []string{"main", "refs/tags/v*"},
				RequireApprovalOtherwise: true,
				ApprovalTTL:              protocol.Duration(2 * time.Hour),
			},
			"strict": {
				URL:         "https://git.example.invalid/strict.git",
				AllowedRefs: []string{"main"},
			},
		},
		Agent: config.Agent{QueueTTL: protocol.Duration(24 * time.Hour)},
		Paths: config.Paths{Logs: filepath.Join(dir, "logs"), Artifacts: filepath.Join(dir, "artifacts")},
	}
	if err := os.MkdirAll(h.cfg.Paths.Logs, 0o750); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	h.runners = []Runner{{
		Name:      "docker-1",
		Labels:    []string{"linux", "docker", "amd64"},
		Executor:  h.exec,
		MinFreeGB: 40,
	}}
	return h
}

// build assembles the orchestrator. Fields on the harness may be adjusted
// before calling it; opts overrides anything the harness would set.
func (h *harness) build() *Orchestrator {
	h.t.Helper()

	o := h.opts
	o.Config = h.cfg
	if o.Store == nil {
		o.Store = h.db
	}
	if o.Broker == nil && !h.brokerless {
		o.Broker = h.broker
	}
	o.Source = h.source
	o.Secrets = h.secret
	o.Caches = h.caches
	o.Artifacts = artifact.NewCollector(h.dir)
	o.Runners = h.runners
	if o.CancelGrace == 0 {
		o.CancelGrace = 2 * time.Second
	}
	if o.TeardownTimeout == 0 {
		o.TeardownTimeout = 5 * time.Second
	}
	if o.ReportBackoff == 0 {
		o.ReportBackoff = time.Millisecond
	}

	orch, err := New(o)
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.t.Cleanup(func() { _ = orch.Close() })
	return orch
}

func (h *harness) request(job string) protocol.RunRequest {
	return protocol.RunRequest{
		ID:     "run1",
		Repo:   "app",
		Job:    job,
		Ref:    "main",
		Origin: protocol.Origin{Kind: "cli", Actor: "tester"},
	}
}

// admitAndClaim runs a request through gates 1-5 and claims it, which is what
// the agent does before handing a run to Execute.
func (h *harness) admitAndClaim(o *Orchestrator, req protocol.RunRequest) store.Run {
	h.t.Helper()
	if _, err := o.Admit(h.t.Context(), req, 0); err != nil {
		h.t.Fatalf("Admit: %v", err)
	}
	if err := h.db.Claim(h.t.Context(), req.ID); err != nil {
		h.t.Fatalf("Claim: %v", err)
	}
	r, err := h.db.Get(h.t.Context(), req.ID)
	if err != nil {
		h.t.Fatalf("Get: %v", err)
	}
	return r
}

// run is admit + claim + execute, the whole path an agent drives.
func (h *harness) run(o *Orchestrator, req protocol.RunRequest) Result {
	h.t.Helper()
	res, err := o.Execute(h.t.Context(), h.admitAndClaim(o, req))
	if err != nil {
		h.t.Fatalf("Execute: %v", err)
	}
	return res
}

// logContents returns everything written to the run's local log file.
func (h *harness) logContents(runID string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.cfg.Paths.Logs, runID+".log"))
	if err != nil {
		h.t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// writeWorkspaceFile creates a file inside the workspace the fake source built,
// so artifact collection has something real to find.
func (h *harness) writeWorkspaceFile(runID, rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.source.root, runID, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

func mustGet(t *testing.T, db *store.DB, runID string) store.Run {
	t.Helper()
	r, err := db.Get(t.Context(), runID)
	if err != nil {
		t.Fatalf("Get %s: %v", runID, err)
	}
	return r
}

// waitFor polls until cond is true or the deadline passes. Reporting is
// asynchronous by design, so assertions about it have to be too.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fmtStates(states []protocol.RunState) string { return fmt.Sprint(states) }
