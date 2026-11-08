package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// An agent Homebrew installed (A78): it moves onto what `brew upgrade` puts
// behind the opt link, and never writes into a keg.

// notHomebrews is a binary Homebrew did not install, whatever installed the
// Go that built the test.
func notHomebrews() (string, bool) { return "", false }

// brewFixture is a Homebrew prefix in a temporary directory, with a keg per
// version, and a ready agent that believes it runs from the first.
type brewFixture struct {
	a      *Agent
	prefix string
	link   string
	execed chan string

	mu       sync.Mutex
	versions map[string]string // binary path → what it says it is
}

func newBrewFixture(t *testing.T, running string, kegs ...string) *brewFixture {
	t.Helper()
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &brewFixture{prefix: prefix, execed: make(chan string, 2), versions: map[string]string{}}
	for _, v := range kegs {
		bin := f.keg(v)
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("latem "+v), 0o755); err != nil {
			t.Fatal(err)
		}
		f.versions[bin] = "v" + v
	}
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.link = filepath.Join(prefix, "opt", "latem", "bin", "latem")
	f.point(t, kegs[0])

	cfg := &config.Config{}
	cfg.Paths = config.DerivePaths(shortTempDir(t))
	a, err := New(Options{
		Config:           cfg,
		Version:          running,
		ShutdownGrace:    time.Minute,
		HomebrewInterval: 20 * time.Millisecond,
		Homebrew:         func() (string, bool) { return f.link, true },
		BinaryVersion: func(_ context.Context, path string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.versions[path], nil
		},
		Exec: func(path string, _, _ []string) error {
			f.execed <- path
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.startReloads()
	f.a = a
	return f
}

func (f *brewFixture) keg(v string) string {
	return filepath.Join(f.prefix, "Cellar", "latem", v, "bin", "latem")
}

// point moves the opt link the way `brew upgrade` does: relative, onto the
// keg, replacing what was there.
func (f *brewFixture) point(t *testing.T, v string) {
	t.Helper()
	opt := filepath.Join(f.prefix, "opt", "latem")
	tmp := opt + ".new"
	if err := os.Symlink(filepath.Join("..", "Cellar", "latem", v), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, opt); err != nil {
		t.Fatal(err)
	}
}

// watch runs the loop until the test ends.
func (f *brewFixture) watch(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.a.homebrewLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (f *brewFixture) expectExec(t *testing.T, want string, within time.Duration) {
	t.Helper()
	select {
	case path := <-f.execed:
		if path != want {
			t.Errorf("re-execed %s, want %s", path, want)
		}
	case <-time.After(within):
		t.Fatalf("the agent did not move onto %s", want)
	}
}

func (f *brewFixture) expectNoExec(t *testing.T, during time.Duration) {
	t.Helper()
	select {
	case path := <-f.execed:
		t.Fatalf("the agent re-execed %s", path)
	case <-time.After(during):
	}
}

func TestHomebrew_MovesOntoWhatBrewUpgradeInstalled(t *testing.T) {
	f := newBrewFixture(t, "v0.3.1", "0.3.1", "0.3.2")
	f.watch(t)
	f.expectNoExec(t, 200*time.Millisecond)

	f.point(t, "0.3.2")
	// brew deletes the old keg straight after; the agent never needed it.
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(f.keg("0.3.1")))); err != nil {
		t.Fatal(err)
	}
	f.expectExec(t, f.keg("0.3.2"), claimSettle+2*time.Second)
}

// An upgrade is not a reason to hold work back: the agent takes runs as it
// always does and moves the first moment none is in flight.
func TestHomebrew_WaitsForTheRunInFlightWithoutRefusingWork(t *testing.T) {
	old := switchRetry
	switchRetry = 50 * time.Millisecond
	t.Cleanup(func() { switchRetry = old })

	f := newBrewFixture(t, "v0.3.1", "0.3.1", "0.3.2")
	f.a.noteActive("r_busy", "app")
	p := &pool{a: f.a, accepting: true, active: map[string]time.Time{}}
	f.watch(t)

	f.point(t, "0.3.2")
	f.expectNoExec(t, claimSettle+500*time.Millisecond)
	if !p.accepts() {
		t.Error("the agent stopped taking work to move onto a Homebrew upgrade")
	}

	f.a.forgetActive("r_busy")
	f.expectExec(t, f.keg("0.3.2"), claimSettle+2*time.Second)
}

// `brew reinstall` pours the same version into a new keg: nothing to move for.
func TestHomebrew_TheSameVersionIsNothingToMoveFor(t *testing.T) {
	f := newBrewFixture(t, "v0.3.1", "0.3.1", "0.3.1_1")
	f.versions[f.keg("0.3.1_1")] = "v0.3.1"
	f.watch(t)
	f.point(t, "0.3.1_1")
	f.expectNoExec(t, claimSettle+300*time.Millisecond)
}

// SIGUSR2 asks the link, not the keg this process came from: that is where
// `latem service restart` and a hand-sent signal should land too.
func TestHomebrew_SIGUSR2MovesWhereTheLinkLeads(t *testing.T) {
	f := newBrewFixture(t, "v0.3.1", "0.3.1", "0.3.2")
	f.point(t, "0.3.2")
	f.a.Switch(context.Background())
	f.expectExec(t, f.keg("0.3.2"), claimSettle+2*time.Second)
}

// The keg is Homebrew's. auto_update must neither ask the feed nor write a
// release into it.
func TestHomebrew_AutoUpdateLeavesTheKegAlone(t *testing.T) {
	f := newUpdateFixture(t, true)
	f.a.opts.Homebrew = func() (string, bool) { return "/opt/homebrew/opt/latem/bin/latem", true }
	f.a.updateEvery = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	f.a.updateLoop(ctx)

	if checks, fetches := f.src.counts(); checks != 0 || fetches != 0 {
		t.Errorf("a Homebrew install made %d check(s) and %d fetch(es)", checks, fetches)
	}
}

// Not Homebrew's: no loop, and SIGUSR2 means the binary at this process's
// own path, as it always has.
func TestHomebrew_NotHomebrewsWatchesNothing(t *testing.T) {
	f := newSwitchFixture(t, "v0.4.0", time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.a.homebrewLoop(ctx)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("homebrewLoop kept running for a binary that is not in a keg")
	}
}
