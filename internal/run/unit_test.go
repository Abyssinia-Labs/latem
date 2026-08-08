package run

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ------------------------------------------------------- concurrency group ---

func TestGroups_OneHolderAtATime(t *testing.T) {
	g := newGroups()

	if !g.acquire("ios", "run1") {
		t.Fatal("the first run could not take a free group")
	}
	if g.acquire("ios", "run2") {
		t.Fatal("two runs hold one group")
	}
	if !g.acquire("ios", "run1") {
		t.Fatal("re-acquiring is a self-deadlock")
	}
	if holder, ok := g.holder("ios"); !ok || holder != "run1" {
		t.Fatalf("holder = %q, %v", holder, ok)
	}

	// A run that lost must not release the winner's group.
	g.release("ios", "run2")
	if _, ok := g.holder("ios"); !ok {
		t.Fatal("a loser released the winner's group")
	}

	g.release("ios", "run1")
	if !g.acquire("ios", "run2") {
		t.Fatal("the group was not released")
	}

	// An empty group is not a group.
	if !g.acquire("", "run3") {
		t.Fatal("the empty group was contended")
	}
	g.release("", "run3")
}

// Gate 16 is "queued, wait, do not fail" (amendment A14): the second run
// returns to the queue, keeps its slot free, and takes nothing with it.
func TestExecute_ConcurrencyGroupRequeues(t *testing.T) {
	h := newHarness(t)
	running := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.env.exec = func(context.Context, executor.Step, io.Writer, io.Writer) (int, error) {
		once.Do(func() { close(running) })
		<-release
		return 0, nil
	}
	o := h.build()

	first := h.request("grouped")
	r1 := h.admitAndClaim(o, first)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := o.Execute(t.Context(), r1); err != nil {
			t.Errorf("first Execute: %v", err)
		}
	}()
	<-running

	second := h.request("grouped")
	second.ID = "run2"
	res := h.run(o, second)

	if !res.Requeued || res.State != protocol.StateQueued {
		t.Fatalf("res = %+v, want queued and requeued", res)
	}
	row := mustGet(t, h.db, "run2")
	if row.State != protocol.StateQueued {
		t.Fatalf("stored state = %s", row.State)
	}
	if row.ExpiresAt == nil {
		t.Error("the requeued run has no fresh queue_ttl deadline")
	}
	// Back in the queue and claimable again.
	if _, err := h.db.ClaimNext(t.Context(), nil); err != nil {
		t.Fatalf("a requeued run is not claimable: %v", err)
	}
	// It never reached the executor, and its workspace was torn down.
	if h.exec.prepared != 1 {
		t.Errorf("the executor was prepared %d times, want 1", h.exec.prepared)
	}
	if _, err := os.Stat(filepath.Join(h.source.root, "run2")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a requeued run leaked its workspace")
	}

	close(release)
	<-done

	// Once the holder finishes the group is free again.
	if _, held := o.groups.holder("ios"); held {
		t.Error("the group was not released when the run finished")
	}
}

// ------------------------------------------------------------- gate 11 ------

func TestReadRegistry_MissingIsENoRegistry(t *testing.T) {
	dir := t.TempDir()
	_, err := readRegistry(dir)
	if got := codeOf(err); got != protocol.ErrNoRegistry {
		t.Fatalf("code = %s, want E_NO_REGISTRY (%v)", got, err)
	}
}

func TestReadRegistry_UnparsableIsEBadRegistry(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "version: 1\njobs: [this is not a map]\n")
	_, err := readRegistry(dir)
	if got := codeOf(err); got != protocol.ErrBadRegistry {
		t.Fatalf("code = %s, want E_BAD_REGISTRY (%v)", got, err)
	}
}

// Strict decoding: an unknown field fails the run that introduced it rather
// than silently doing something other than what was written.
func TestReadRegistry_UnknownFieldIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, `
version: 1
jobs:
  test:
    runs_on: [linux]
    tiemout: 5m
    steps:
      - run: ./x.sh
`)
	if _, err := readRegistry(dir); err == nil {
		t.Fatal("a typo'd field was accepted")
	}
}

func TestReadRegistry_OversizedIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeRegistry(t, dir, "version: 1\n"+strings.Repeat("# padding\n", MaxRegistryBytes/9))
	_, err := readRegistry(dir)
	if got := codeOf(err); got != protocol.ErrBadRegistry {
		t.Fatalf("code = %s, want E_BAD_REGISTRY (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("message = %v", err)
	}
}

// The workspace is whatever a repository and a build put there. A registry
// symlinked out of the tree is refused by the kernel, not by string matching.
func TestReadRegistry_SymlinkOutOfTheWorkspaceIsRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(outside, []byte("version: 1\njobs: {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".latem"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".latem", "jobs.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := readRegistry(dir); err == nil {
		t.Fatal("a registry symlinked outside the workspace was read")
	}
}

func TestReadRegistry_MissingWorkspace(t *testing.T) {
	_, err := readRegistry(filepath.Join(t.TempDir(), "gone"))
	if got := codeOf(err); got != protocol.ErrNoRegistry {
		t.Fatalf("code = %s, want E_NO_REGISTRY", got)
	}
}

// End to end: a workspace with no .latem/jobs.yaml fails the run, and the run
// still passes through collecting because the workspace exists.
func TestExecute_NoRegistryAtThisCommit(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrNoRegistry {
		t.Fatalf("got %s/%s, want failed/E_NO_REGISTRY", res.State, res.ErrorCode)
	}
}

func writeRegistry(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".latem"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".latem", "jobs.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ------------------------------------------------------------ reporter ------

func TestReporter_RetriesWithBackoffAndMarksReported(t *testing.T) {
	h := newHarness(t)
	h.broker.failNext = 2
	h.opts.ReportAttempts = 5
	o := h.build()

	h.run(o, h.request("test"))
	waitFor(t, "the run to be reported despite two failures", func() bool {
		r, err := h.db.Get(t.Context(), "run1")
		return err == nil && r.ReportedState == protocol.StateSucceeded
	})
}

func TestReporter_KeepsOneRunsUpdatesInOrder(t *testing.T) {
	rep := newReporter(&fakeBroker{}, nil, 1, time.Millisecond)
	_ = rep

	h := newHarness(t)
	o := h.build()
	h.run(o, h.request("test"))
	_ = o.Close()

	var seen []protocol.RunState
	for _, r := range h.broker.updates() {
		if r.runID == "run1" {
			seen = append(seen, r.update.State)
		}
	}
	want := []protocol.RunState{
		protocol.StateQueued, protocol.StatePreparing, protocol.StateRunning,
		protocol.StateCollecting, protocol.StateSucceeded,
	}
	if len(seen) != len(want) {
		t.Fatalf("reported %v, want %v", fmtStates(seen), fmtStates(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("reported %v, want %v", fmtStates(seen), fmtStates(want))
		}
	}
}

func TestReporter_CloseIsIdempotentAndSilentAfterwards(t *testing.T) {
	rep := newReporter(&fakeBroker{}, nil, 1, time.Millisecond)
	rep.close()
	rep.close()
	// An enqueue after close is dropped rather than panicking on a closed
	// channel, which is what a late report from a torn-down run looks like.
	rep.enqueue("run1", protocol.Update{State: protocol.StateSucceeded})
	rep.forget("run1")
}

func TestReporter_WithoutABrokerDoesNothing(t *testing.T) {
	rep := newReporter(nil, nil, 0, 0)
	rep.enqueue("run1", protocol.Update{State: protocol.StateQueued})
	rep.close()
}

// -------------------------------------------------------- construction ------

func TestNew_RequiresConfigStoreAndALogDirectory(t *testing.T) {
	h := newHarness(t)

	if _, err := New(Options{Store: h.db}); err == nil {
		t.Error("New accepted a nil Config")
	}
	if _, err := New(Options{Config: h.cfg}); err == nil {
		t.Error("New accepted a nil Store")
	}
	if _, err := New(Options{Config: &config.Config{}, Store: h.db}); err == nil {
		t.Error("New accepted a config with no log directory")
	}
}

func TestNewRunner_ReadsThroughTheExecutorBlock(t *testing.T) {
	c := &config.Runner{
		Name:     "docker-1",
		Executor: config.ExecutorDocker,
		Labels:   []string{"linux", "docker"},
		Docker: &config.DockerRunner{
			DefaultImage: "ghcr.io/me/base:20",
			CPUs:         4,
			Memory:       "8g",
			GPU:          true,
			MinFreeGB:    77,
		},
	}
	r := NewRunner(c, nil)

	if r.Name != "docker-1" || r.DefaultImage != "ghcr.io/me/base:20" {
		t.Fatalf("runner = %+v", r)
	}
	if r.MinFreeGB != 77 || r.Resources.CPUs != 4 || r.Resources.Memory != "8g" || !r.Resources.GPU {
		t.Fatalf("runner = %+v", r)
	}
	// The labels are copied, so a later config reload cannot mutate them.
	c.Labels[0] = "mutated"
	if r.Labels[0] != "linux" {
		t.Error("labels alias the config slice")
	}

	// A native runner picks up its own floor and no docker fields.
	native := NewRunner(&config.Runner{
		Name: "native", Executor: config.ExecutorNative, Native: &config.NativeRunner{},
	}, nil)
	if native.MinFreeGB != config.DefaultNativeMinFreeGB {
		t.Errorf("native floor = %v, want %v", native.MinFreeGB, config.DefaultNativeMinFreeGB)
	}
	if native.DefaultImage != "" {
		t.Error("a native runner picked up a docker image")
	}
}

func TestSelectRunner_FirstMatchWins(t *testing.T) {
	h := newHarness(t)
	h.runners = []Runner{
		{Name: "mac", Labels: []string{"macos", "xcode"}, Executor: h.exec},
		{Name: "linux-a", Labels: []string{"linux", "docker"}, Executor: h.exec},
		{Name: "linux-b", Labels: []string{"linux", "docker"}, Executor: h.exec},
	}
	o := h.build()

	reg, err := registry.Parse([]byte(defaultRegistryYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := o.selectRunner(reg, "test"); got == nil || got.Name != "linux-a" {
		t.Fatalf("selected %+v, want linux-a", got)
	}
	if got := o.selectRunner(reg, "macos"); got == nil || got.Name != "mac" {
		t.Fatalf("selected %+v, want mac", got)
	}
	if got := o.selectRunner(reg, "no-such-job"); got != nil {
		t.Fatalf("selected %+v for an unknown job", got)
	}

	names := o.RunnerNames()
	if len(names) != 3 || names[0] != "mac" {
		t.Fatalf("RunnerNames = %v", names)
	}
}

func TestHasAllLabels(t *testing.T) {
	for _, tc := range []struct {
		have, need []string
		want       bool
	}{
		{[]string{"linux", "docker"}, []string{"linux"}, true},
		{[]string{"linux", "docker"}, []string{"linux", "docker"}, true},
		{[]string{"linux"}, []string{"linux", "docker"}, false},
		{nil, []string{"linux"}, false},
		{[]string{"linux"}, nil, true},
	} {
		if got := hasAllLabels(tc.have, tc.need); got != tc.want {
			t.Errorf("hasAllLabels(%v, %v) = %v", tc.have, tc.need, got)
		}
	}
}

func TestTTLDefaults(t *testing.T) {
	h := newHarness(t)
	h.cfg.Agent.QueueTTL = 0
	o := h.build()

	if got := o.queueTTL(); got != config.DefaultQueueTTL {
		t.Errorf("queueTTL = %s, want the default", got)
	}
	// The repo's own approval_ttl wins over the default.
	if got := o.approvalTTLFor("app"); got != 2*time.Hour {
		t.Errorf("approvalTTL(app) = %s, want 2h", got)
	}
	// A repo that does not set one, and a repo that does not exist.
	if got := o.approvalTTLFor("strict"); got != config.DefaultApprovalTTL {
		t.Errorf("approvalTTL(strict) = %s, want the default", got)
	}
	if got := o.approvalTTLFor("nope"); got != config.DefaultApprovalTTL {
		t.Errorf("approvalTTL(nope) = %s, want the default", got)
	}
}

func TestTruncateAndShortSHA(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	got := truncate(strings.Repeat("x", 20), 10)
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.Contains(got, "truncated") {
		t.Errorf("truncate = %q", got)
	}
	if shortSHA("0123456789abcdef") != "0123456789ab" {
		t.Errorf("shortSHA = %q", shortSHA("0123456789abcdef"))
	}
	if shortSHA("abc") != "abc" {
		t.Errorf("shortSHA short = %q", shortSHA("abc"))
	}
}

func TestStepLabel(t *testing.T) {
	if got := stepLabel(registry.ResolvedStep{Name: "build"}, 0); got != "build" {
		t.Errorf("stepLabel = %q", got)
	}
	if got := stepLabel(registry.ResolvedStep{}, 2); got != "step 3" {
		t.Errorf("stepLabel = %q", got)
	}
}

func TestCollectible(t *testing.T) {
	for s, want := range map[protocol.RunState]bool{
		protocol.StateSucceeded:   true,
		protocol.StateFailed:      true,
		protocol.StateTimedOut:    true,
		protocol.StateCancelled:   true,
		protocol.StateRejected:    false,
		protocol.StateInterrupted: false,
	} {
		if got := collectible(s); got != want {
			t.Errorf("collectible(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestSourceManager_NilIsNil(t *testing.T) {
	if SourceManager(nil) != nil {
		t.Fatal("a nil manager produced a non-nil Source")
	}
}

func TestExecute_WithoutASourceManager(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	o.src = nil

	res := h.run(o, h.request("test"))
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrSource {
		t.Fatalf("got %s/%s, want failed/E_SOURCE", res.State, res.ErrorCode)
	}
}

func TestExecute_WithoutACacheManager(t *testing.T) {
	h := newHarness(t)
	o := h.build()
	o.caches = nil

	// The job declares a cache, so gate 18 cannot be satisfied.
	res := h.run(o, h.request("test"))
	if res.ErrorCode != protocol.ErrCache {
		t.Fatalf("code = %s, want E_CACHE", res.ErrorCode)
	}
}

func TestExecute_RunnerWithNoExecutor(t *testing.T) {
	h := newHarness(t)
	h.runners[0].Executor = nil
	o := h.build()

	res := h.run(o, h.request("test"))
	if res.ErrorCode != protocol.ErrExecutor {
		t.Fatalf("code = %s, want E_EXECUTOR", res.ErrorCode)
	}
}

// Collection failing turns a green run red, but never overwrites a failure the
// build already had: E_STEP is why the build broke, E_ARTIFACT is a
// consequence of it.
func TestCollect_FailureOnlyDowngradesASuccess(t *testing.T) {
	// The registry refuses a ".." glob at parse, so the way to reach an
	// artifact failure is the one §17 is actually about: a symlink a build
	// produced that points out of the workspace.
	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("a key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	plant := func(h *harness, runID string) {
		t.Helper()
		dir := filepath.Join(h.source.root, runID, "out")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "stolen.txt")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	h := newHarness(t)
	o := h.build()
	r := h.admitAndClaim(o, h.request("test"))
	plant(h, "run1")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrArtifact {
		t.Fatalf("got %s/%s, want failed/E_ARTIFACT", res.State, res.ErrorCode)
	}

	// The same escape on a run that already failed keeps E_STEP: the step is
	// why the build broke, the glob is a consequence of getting that far.
	h2 := newHarness(t)
	h2.env.exec = func(context.Context, executor.Step, io.Writer, io.Writer) (int, error) {
		return 9, nil
	}
	o2 := h2.build()
	r2 := h2.admitAndClaim(o2, h2.request("test"))
	plant(h2, "run1")

	res2, err := o2.Execute(t.Context(), r2)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res2.ErrorCode != protocol.ErrStep {
		t.Fatalf("code = %s, want E_STEP preserved", res2.ErrorCode)
	}
}

func TestCollect_NoDeclarationsIsNoWork(t *testing.T) {
	h := newHarness(t)
	h.source.registry = `
version: 1
jobs:
  bare:
    runs_on: [linux]
    steps:
      - name: only
        run: ./only.sh
`
	o := h.build()

	res := h.run(o, h.request("bare"))
	if res.State != protocol.StateSucceeded || len(res.Artifacts) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestOutcomeHelpers(t *testing.T) {
	if !(outcome{state: protocol.StateSucceeded}).ok() {
		t.Error("a succeeded outcome is not ok")
	}
	if (outcome{state: protocol.StateFailed}).ok() {
		t.Error("a failed outcome is ok")
	}
	if got := codeOf(nil); got != "" {
		t.Errorf("codeOf(nil) = %s", got)
	}
	if got := codeOf(errors.New("plain")); got != "" {
		t.Errorf("codeOf(plain) = %s", got)
	}
	// A code with no terminal state falls back to failed.
	out := fail(errors.New("plain"), protocol.ErrAgentDown)
	if out.state != protocol.StateFailed {
		t.Errorf("state = %s, want failed", out.state)
	}
	if !isCancellation(context.Canceled) || isCancellation(errors.New("x")) {
		t.Error("isCancellation is wrong")
	}
}

// A cancel landing between two steps stops the loop before the next one
// starts: there is no point beginning work that is about to be killed.
func TestCheckBudget_CancelBetweenSteps(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	_, hnd, err := o.begin(t.Context(), "run1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer o.end(hnd)
	hnd.setPhase(protocol.StateRunning)
	hnd.request("stop", "ops")

	x := &execution{o: o, h: hnd}
	out, done := x.checkBudget(t.Context(), "next", time.Minute)
	if !done || out.state != protocol.StateCancelled {
		t.Fatalf("out = %+v, done = %v", out, done)
	}
	if !strings.Contains(out.reason, "stop") || !strings.Contains(out.reason, "ops") {
		t.Errorf("reason = %q", out.reason)
	}
}
