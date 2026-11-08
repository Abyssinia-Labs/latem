package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// §20's self-update, from the side that owns the machine.
//
// internal/selfupdate's own tests answer for the trust chain — signatures,
// checksums, versions — and none of that is repeated here. What is under test
// is the promise those tests cannot make: **never mid-run**, and a pause that
// ends even when the update does not.

// fakeReleases is a release source that touches no network.
type fakeReleases struct {
	rel      selfupdate.Release
	checkErr error
	fetchErr error
	// body is what the "download" writes, so a test can tell the binary that
	// arrived from the one that was there.
	body string

	mu      sync.Mutex
	checks  int
	fetches int
}

func (f *fakeReleases) Check(context.Context) (selfupdate.Release, error) {
	f.mu.Lock()
	f.checks++
	f.mu.Unlock()
	return f.rel, f.checkErr
}

func (f *fakeReleases) Fetch(_ context.Context, _ selfupdate.Release, dir string) (string, error) {
	f.mu.Lock()
	f.fetches++
	f.mu.Unlock()
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	// Staged where the real one stages, because [selfupdate.Install] refuses a
	// staged file in another directory and this test should meet that rule
	// rather than route around it.
	path := filepath.Join(dir, ".latem.new")
	if err := os.WriteFile(path, []byte(f.body), 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func (f *fakeReleases) counts() (checks, fetches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.fetches
}

// updateFixture is an agent with just enough of itself to run one update
// cycle: a config, a log, the active-run bookkeeping, and a binary on disk
// standing in for the one being replaced.
type updateFixture struct {
	a      *Agent
	src    *fakeReleases
	target string
	execed chan string
}

func newUpdateFixture(t *testing.T, autoUpdate bool) *updateFixture {
	t.Helper()

	dir := t.TempDir()
	target := filepath.Join(dir, "latem")
	if err := os.WriteFile(target, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	f := &updateFixture{
		src:    &fakeReleases{rel: selfupdate.Release{Version: "v0.4.0", Bare: "0.4.0"}, body: "the new binary"},
		target: target,
		execed: make(chan string, 1),
	}

	cfg := &config.Config{}
	cfg.Agent.AutoUpdate = autoUpdate
	a, err := New(Options{
		Config:         cfg,
		Version:        "v0.3.0",
		Releases:       f.src,
		UpdateIdleWait: 750 * time.Millisecond,
		Homebrew:       notHomebrews,
		Exec: func(path string, _, _ []string) error {
			f.execed <- path
			// The real syscall.Exec never returns. Returning nil here is the
			// closest a test can get without becoming another program.
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.a = a
	return f
}

func (f *updateFixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSelfUpdate_ReplacesTheBinaryAndReExecs(t *testing.T) {
	f := newUpdateFixture(t, true)

	f.a.updateOnce(context.Background(), f.src, f.target)

	if got := f.installed(t); got != "the new binary" {
		t.Errorf("the binary on disk is %q, want the downloaded one", got)
	}
	select {
	case path := <-f.execed:
		if path != f.target {
			t.Errorf("re-execed %s, want %s", path, f.target)
		}
	default:
		t.Error("the agent installed an update and did not re-exec (§20)")
	}
	// The pause is over even on the happy path: an exec that failed must not
	// leave a machine refusing work.
	if f.a.paused.Load() {
		t.Error("claims are still paused after the update")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.target), ".latem.new")); err == nil {
		t.Error("the staging file survived the install")
	}
}

// The claim this exists for. A run is in flight and never finishes, so the
// update must not happen — and the binary must be the old one afterwards,
// because §6's reconciliation would otherwise mark that build interrupted and
// reap it on the way back up.
func TestSelfUpdate_WillNotReplaceTheBinaryUnderARunningBuild(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.a.noteActive("r_busy", "app")

	start := time.Now()
	f.a.updateOnce(context.Background(), f.src, f.target)
	waited := time.Since(start)

	if got := f.installed(t); got != "the old binary" {
		t.Errorf("the binary was replaced mid-run: on disk is %q", got)
	}
	select {
	case path := <-f.execed:
		t.Errorf("the agent re-execed to %s with a run in flight", path)
	default:
	}
	// It waited rather than giving up immediately: a build that finishes in
	// the next few seconds should get its update, and a check that returned at
	// once would never update a machine that is ever busy.
	if waited < 500*time.Millisecond {
		t.Errorf("gave up after %v, and the idle wait is %v", waited, f.a.updateIdleWait)
	}
	// And the pause ended. This is the whole reason self-update does not reuse
	// `draining`: a machine that failed to update must go back to work.
	if f.a.paused.Load() {
		t.Error("the machine is still refusing runs after abandoning the update")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.target), ".latem.new")); err == nil {
		t.Error("the abandoned download was left staged")
	}
}

// The same situation, resolved: the run finishes during the wait.
func TestSelfUpdate_WaitsForTheRunToFinish(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.a.noteActive("r_finishing", "app")

	go func() {
		time.Sleep(150 * time.Millisecond)
		f.a.forgetActive("r_finishing")
	}()

	f.a.updateOnce(context.Background(), f.src, f.target)

	if got := f.installed(t); got != "the new binary" {
		t.Errorf("the update did not happen once the run finished: on disk is %q", got)
	}
	select {
	case <-f.execed:
	default:
		t.Error("no re-exec after the run finished")
	}
}

// While it waits, the machine must actually stop taking work. Without this the
// wait is decorative: a worker could claim a run during it and the update
// would land on top of the build it just started.
func TestSelfUpdate_StopsClaimingWhileItWaits(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.a.noteActive("r_busy", "app")

	p := &pool{a: f.a, accepting: true, active: map[string]time.Time{}}
	if !p.accepts() {
		t.Fatal("control: a pool with nothing in the way should accept")
	}

	paused := make(chan bool, 1)
	go func() {
		// Sampled while the update is inside its idle wait.
		time.Sleep(250 * time.Millisecond)
		paused <- !p.accepts()
	}()
	f.a.updateOnce(context.Background(), f.src, f.target)

	if !<-paused {
		t.Error("the pool was still claiming runs while self-update waited for it to go idle")
	}
	// Reversible: draining is the one-way flag, and this is not it.
	if !p.accepts() {
		t.Error("the pool never resumed claiming")
	}
	if f.a.draining.Load() {
		t.Error("self-update set the draining latch, which nothing clears")
	}
}

func TestSelfUpdate_NothingToDo(t *testing.T) {
	// The ordinary daily answer. It must not pause the machine, and must not
	// download anything.
	f := newUpdateFixture(t, true)
	f.src.checkErr = selfupdate.ErrUpToDate

	f.a.updateOnce(context.Background(), f.src, f.target)

	if _, fetches := f.src.counts(); fetches != 0 {
		t.Errorf("an up-to-date agent downloaded %d asset(s)", fetches)
	}
	if got := f.installed(t); got != "the old binary" {
		t.Errorf("the binary changed: %q", got)
	}
	if f.a.paused.Load() {
		t.Error("an up-to-date check left the machine refusing runs")
	}
}

// A release this agent refuses — a bad signature, a checksum mismatch, a
// missing asset — must leave the machine exactly as it was, including taking
// work again.
func TestSelfUpdate_ARefusedReleaseChangesNothing(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.src.fetchErr = errors.New("checksums file is not signed by the key this build trusts")

	f.a.updateOnce(context.Background(), f.src, f.target)

	if got := f.installed(t); got != "the old binary" {
		t.Errorf("a refused release was installed: %q", got)
	}
	select {
	case <-f.execed:
		t.Error("the agent re-execed after refusing a release")
	default:
	}
	if f.a.paused.Load() {
		t.Error("a failed download left the machine refusing runs")
	}
}

// auto_update defaults to false, and the loop must not so much as ask the feed
// on a machine that did not opt in. §20 is explicit that this is opt-in, and a
// daily outbound request nobody asked for is the kind of thing an operator
// finds in a firewall log and has to write an issue about.
func TestSelfUpdate_DoesNothingWhenNotEnabled(t *testing.T) {
	f := newUpdateFixture(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	f.a.updateEvery = 10 * time.Millisecond
	f.a.updateLoop(ctx)

	if checks, fetches := f.src.counts(); checks != 0 || fetches != 0 {
		t.Errorf("auto_update is false and the agent made %d check(s) and %d fetch(es)", checks, fetches)
	}
}

// The loop does not check at startup. A machine that has just been restarted —
// possibly by the previous update — is the worst moment to replace its binary
// again.
func TestSelfUpdate_DoesNotCheckAtStartup(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.a.updateEvery = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	f.a.updateLoop(ctx)

	if checks, _ := f.src.counts(); checks != 0 {
		t.Errorf("the agent checked the feed %d time(s) before its first interval elapsed", checks)
	}
}

// runningBinary has to resolve a symlink, because ~/.local/bin/latem is
// sometimes one: replacing the link rather than its target would install the
// new binary somewhere nothing looks.
func TestRunningBinary_ResolvesSymlinks(t *testing.T) {
	got, err := runningBinary()
	if err != nil {
		t.Fatalf("runningBinary: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Errorf("runningBinary = %q, want an absolute path", got)
	}
	resolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != got {
		t.Errorf("runningBinary returned %s, which still resolves to %s", got, resolved)
	}
}
