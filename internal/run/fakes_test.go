package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	resolveErr error
	acquireErr error

	fetched    []source.Repo
	resolved   []string
	workspaces []string
	// checkedOut records the (ref, sha) pairs gate 10 was asked for, which is
	// what proves the checkout used the commit gate 8 admitted rather than
	// resolving the name a second time.
	checkedOut [][2]string
	// commits are the commits the mirror holds besides the ref's tip, each
	// mapped to whether it is on the ref (A52's pin).
	commits map[string]bool
	// containing is, per commit, every ref whose history holds it (A53); a
	// commit with no entry is held by the ref's tip alone.
	containing map[string][]string
	// packed records each PackSince the mirror was asked for, as sha^have.
	packed []string
	// upstream are the commits FetchCommit can bring by their names, and
	// fetchedByName the ones it was asked for.
	upstream      map[string]bool
	fetchedByName []string
	// opened are the repositories Open was asked for, unfetched (A76), and
	// noMirror makes it answer that there is no mirror yet.
	opened   []source.Repo
	noMirror bool
}

// setSHA moves the ref, as a force-push does.
func (s *fakeSource) setSHA(sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sha = sha
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

// Open is the mirror unfetched, for a local run (A76). noMirror makes it
// answer as a machine that has never fetched the repository.
func (s *fakeSource) Open(repo source.Repo) (Mirror, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, repo)
	if s.noMirror {
		return nil, fmt.Errorf("%s: %w", repo.Key, source.ErrNoMirror)
	}
	return &fakeMirror{src: s}, nil
}

type fakeMirror struct{ src *fakeSource }

func (m *fakeMirror) HasRef(_ context.Context, fullyQualified string) (bool, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	return m.src.refs[fullyQualified], nil
}

func (m *fakeMirror) HasCommit(_ context.Context, sha string) (bool, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	_, ok := m.src.commits[sha]
	return ok || sha == m.src.sha, nil
}

// FetchCommit brings a commit from upstream by its name, when upstream has it.
func (m *fakeMirror) FetchCommit(_ context.Context, sha string) error {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	m.src.fetchedByName = append(m.src.fetchedByName, sha)
	if !m.src.upstream[sha] {
		return errors.New("fatal: remote error: upload-pack: not our ref " + sha)
	}
	if m.src.commits == nil {
		m.src.commits = map[string]bool{}
	}
	m.src.commits[sha] = false
	return nil
}

func (m *fakeMirror) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	return ancestor == descendant || (descendant == m.src.sha && m.src.commits[ancestor]), nil
}

func (m *fakeMirror) ContainingRefs(_ context.Context, sha string, prefixes []string) ([]string, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	held, ok := m.src.containing[sha]
	if !ok && sha == m.src.sha {
		held = []string{"refs/heads/main"}
	}
	var out []string
	for _, r := range held {
		for _, p := range prefixes {
			if r == p || strings.HasPrefix(r, strings.TrimSuffix(p, "/")+"/") {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

func (m *fakeMirror) PackSince(_ context.Context, sha, have string) ([]byte, error) {
	m.src.mu.Lock()
	defer m.src.mu.Unlock()
	if sha == have {
		return nil, nil
	}
	m.src.packed = append(m.src.packed, sha+"^"+have)
	return []byte("PACK " + sha), nil
}

// Resolve is gate 9. It answers with whatever the ref points at *now*, which
// is what makes a test able to move it between one attempt and the next.
func (m *fakeMirror) Resolve(_ context.Context, normalizedRef string) (string, error) {
	s := m.src
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = append(s.resolved, normalizedRef)
	if s.resolveErr != nil {
		return "", s.resolveErr
	}
	return s.sha, nil
}

func (m *fakeMirror) Checkout(
	_ context.Context, runID, normalizedRef, sha string,
) (*source.Workspace, error) {
	s := m.src
	s.mu.Lock()
	err := s.acquireErr
	s.checkedOut = append(s.checkedOut, [2]string{normalizedRef, sha})
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

	// The workspace is at the sha it was told to check out, not at whatever the
	// ref points at by now. A real `git checkout --detach <sha>` behaves the
	// same way, and the difference is the whole point of gate 10 taking a
	// commit rather than a name.
	return &source.Workspace{
		Path: path, RunID: runID, Repo: "app", Ref: normalizedRef, SHA: sha,
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
	// outputs, when set, is what a step "wrote" to $GITHUB_OUTPUT. The fake
	// has no file to read back, so a test that wants outputs states them.
	outputs func(step executor.Step) map[string]string
	// state, when set, is what a step "wrote" to $GITHUB_STATE (A51).
	state      func(step executor.Step) map[string]string
	actionsDir string
	tempDir    string
	// puts is what PutTemp was handed, by name (A53).
	puts map[string][]byte
}

func (e *fakeEnv) Exec(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (executor.Result, error) {
	e.mu.Lock()
	e.steps = append(e.steps, step)
	fn, outs, state := e.exec, e.outputs, e.state
	e.mu.Unlock()
	res := executor.Result{}
	if outs != nil {
		res.Outputs = outs(step)
	}
	if state != nil {
		res.State = state(step)
	}
	if fn == nil {
		return res, nil
	}
	code, err := fn(ctx, step, stdout, stderr)
	res.ExitCode = code
	return res, err
}

func (e *fakeEnv) EnvFile() string {
	if e.envFile == "" {
		return "/tmp/latem/env"
	}
	return e.envFile
}

func (e *fakeEnv) ActionsDir() string { return e.actionsDir }

func (e *fakeEnv) TempDir() string { return e.tempDir }

func (e *fakeEnv) ToolCacheDir() string { return e.spec.ToolCache }

func (e *fakeEnv) PutTemp(_ context.Context, name string, data []byte) (string, error) {
	if err := executor.ValidTempName(name); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.puts == nil {
		e.puts = map[string][]byte{}
	}
	e.puts[name] = append([]byte(nil), data...)
	dir := e.tempDir
	if dir == "" {
		dir = "/tmp/latem/temp"
	}
	return dir + "/" + name, nil
}

func (e *fakeEnv) put(name string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.puts[name]
	return b, ok
}

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
	// name is what Name reports, "fake" when empty; a test about behaviour
	// that depends on the executor's kind sets it.
	name string
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

func (f *fakeExecutor) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

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

// fakeBroker stands in for a transport, including the part of a transport's
// contract that amendment A28 made explicit: a broker records what it has
// accepted, under its own key, and nothing else does it for it. key and st are
// what make this fake behave like internal/broker/local rather than like a
// recorder that quietly leaves the store alone.
type fakeBroker struct {
	// key is the broker_state key this transport writes under. Empty means it
	// keeps no durable record, which is what a transport with no store does.
	key string
	st  Store
	// block holds Report until it is closed, standing in for a transport whose
	// far side has stopped answering. It is unbuffered and never receives, so a
	// reporter that published to two brokers from one goroutine would never
	// reach the second.
	block <-chan struct{}

	mu       sync.Mutex
	name     string
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

func (b *fakeBroker) Report(ctx context.Context, runID string, u protocol.Update) error {
	if b.block != nil {
		select {
		case <-b.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	b.mu.Lock()
	if b.failNext > 0 {
		b.failNext--
		b.mu.Unlock()
		return errors.New("broker unavailable")
	}
	b.reports = append(b.reports, reported{runID, u})
	key, st := b.key, b.st
	b.mu.Unlock()

	if key != "" && st != nil {
		return st.MarkReported(ctx, key, runID, u.State)
	}
	return nil
}

func (b *fakeBroker) Logs(_ context.Context, runID string) (broker.LogSink, error) {
	if b.logsErr != nil {
		return nil, b.logsErr
	}
	return &fakeLogSink{b: b, runID: runID}, nil
}

func (b *fakeBroker) Heartbeat(context.Context, protocol.MachineHealth) error { return nil }

func (b *fakeBroker) ReportPlan(context.Context, protocol.PlanResult) error { return nil }

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

type fakeLogSink struct {
	b     *fakeBroker
	runID string
}

func (s *fakeLogSink) Write(ctx context.Context, frames []protocol.LogFrame) error {
	s.b.mu.Lock()
	s.b.frames = append(s.b.frames, frames...)
	for _, f := range frames {
		if f.Seq > s.b.acked {
			s.b.acked = f.Seq
		}
	}
	high, key, st := s.b.acked, s.b.key, s.b.st
	s.b.mu.Unlock()

	// Persisting the resume point is the transport's job now, not
	// internal/run's: only the transport knows which key its acknowledgements
	// belong under (amendment A28).
	if key != "" && st != nil && high > 0 {
		return st.SetLastAckedSeq(ctx, key, s.runID, high)
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
		broker: &fakeBroker{name: store.BrokerLocal, key: store.BrokerLocal, st: db},
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
	if len(o.Brokers) == 0 && !h.brokerless {
		o.Brokers = []broker.Broker{h.broker}
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

// rerun claims and executes a run that is already in the queue, which is what
// the agent's pool does on its next poll after an approval.
func (h *harness) rerun(o *Orchestrator, runID string) Result {
	h.t.Helper()

	if err := h.db.Claim(h.t.Context(), runID); err != nil {
		h.t.Fatalf("Claim %s: %v", runID, err)
	}
	r, err := h.db.Get(h.t.Context(), runID)
	if err != nil {
		h.t.Fatalf("Get %s: %v", runID, err)
	}
	res, err := o.Execute(h.t.Context(), r)
	if err != nil {
		h.t.Fatalf("Execute %s: %v", runID, err)
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
