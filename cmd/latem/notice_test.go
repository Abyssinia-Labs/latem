package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noticeFixture runs startNotice against a fake feed and a temporary cache,
// with a file standing in for the terminal.
type noticeFixture struct {
	cache  string
	stderr *os.File
	asks   atomic.Int32
	latest string
	err    error
	delay  time.Duration
	now    time.Time
}

func newNoticeTest(t *testing.T, running string) *noticeFixture {
	t.Helper()
	f := &noticeFixture{cache: filepath.Join(t.TempDir(), "latem", "release.json"), now: time.Now()}
	var err error
	f.stderr, err = os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.stderr.Close() })

	oldV, oldLatest, oldCache, oldNow, oldAllowed := version, latestRelease, noticeCache, noticeNow, noticeAllowed
	t.Cleanup(func() {
		version, latestRelease, noticeCache, noticeNow, noticeAllowed = oldV, oldLatest, oldCache, oldNow, oldAllowed
	})
	version = running
	latestRelease = func(context.Context) (string, error) {
		f.asks.Add(1)
		time.Sleep(f.delay)
		return f.latest, f.err
	}
	noticeCache = func() (string, error) { return f.cache, nil }
	noticeNow = func() time.Time { return f.now }
	noticeAllowed = func(*os.File) bool { return true }
	return f
}

// run is one command invocation: decide, (the command), print.
func (f *noticeFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	if err := f.stderr.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stderr.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	startNotice(newRootCommand(), args, f.stderr)()
	b, err := os.ReadFile(f.stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *noticeFixture) remember(t *testing.T, at time.Time, latest string) {
	t.Helper()
	writeReleaseCheck(f.cache, releaseCheck{CheckedAt: at, Latest: latest})
}

func TestNotice_SaysANewerReleaseExists(t *testing.T) {
	f := newNoticeTest(t, "v0.3.0")
	f.latest = "v0.4.0"
	got := f.run(t, "list")
	for _, want := range []string{
		"A new release of latem is available: v0.3.0 → v0.4.0",
		"To upgrade, run: latem update",
		"https://github.com/Abyssinia-Labs/latem/releases/tag/v0.4.0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q:\n%s", want, got)
		}
	}
	if c := readReleaseCheck(f.cache); c.Latest != "v0.4.0" || !c.CheckedAt.Equal(f.now) {
		t.Errorf("remembered %+v", c)
	}
}

// Once a day: a fresh answer is read, not asked for again.
func TestNotice_AsksAtMostOnceADay(t *testing.T) {
	f := newNoticeTest(t, "v0.3.0")
	f.remember(t, f.now.Add(-time.Hour), "v0.4.0")
	if got := f.run(t, "list"); !strings.Contains(got, "v0.3.0 → v0.4.0") {
		t.Errorf("a remembered newer release was not shown:\n%s", got)
	}
	if n := f.asks.Load(); n != 0 {
		t.Errorf("asked the feed %d times with a fresh answer on disk", n)
	}

	f.remember(t, f.now.Add(-25*time.Hour), "v0.4.0")
	f.latest = "v0.5.0"
	if got := f.run(t, "list"); !strings.Contains(got, "v0.3.0 → v0.5.0") {
		t.Errorf("a stale answer was not refreshed:\n%s", got)
	}
	if n := f.asks.Load(); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
}

func TestNotice_SaysNothing(t *testing.T) {
	t.Run("on the latest release", func(t *testing.T) {
		f := newNoticeTest(t, "v0.4.0")
		f.latest = "v0.4.0"
		if got := f.run(t, "list"); got != "" {
			t.Errorf("printed %q", got)
		}
	})

	t.Run("on a build past the latest release", func(t *testing.T) {
		f := newNoticeTest(t, "v0.4.0-12-gabc1234")
		f.latest = "v0.4.0"
		if got := f.run(t, "list"); got != "" {
			t.Errorf("a source build was told to move to the tag it was built after: %q", got)
		}
	})

	t.Run("when the feed fails, and it waits a day to ask again", func(t *testing.T) {
		f := newNoticeTest(t, "v0.3.0")
		f.err = errors.New("the release feed answered 404 Not Found")
		if got := f.run(t, "list"); got != "" {
			t.Errorf("printed %q", got)
		}
		if c := readReleaseCheck(f.cache); !c.CheckedAt.Equal(f.now) {
			t.Errorf("a failed check was not remembered, so every command would ask: %+v", c)
		}
		f.run(t, "list")
		if n := f.asks.Load(); n != 1 {
			t.Errorf("asked %d times, want once", n)
		}
	})

	t.Run("for a dev build", func(t *testing.T) {
		f := newNoticeTest(t, "dev")
		f.latest = "v0.4.0"
		if got := f.run(t, "list"); got != "" || f.asks.Load() != 0 {
			t.Errorf("printed %q after %d asks", got, f.asks.Load())
		}
	})

	for _, cmd := range [][]string{{"agent"}, {"update", "--check"}, {"__complete", "li"}} {
		t.Run("for "+cmd[0], func(t *testing.T) {
			f := newNoticeTest(t, "v0.3.0")
			f.latest = "v0.4.0"
			if got := f.run(t, cmd...); got != "" || f.asks.Load() != 0 {
				t.Errorf("printed %q after %d asks", got, f.asks.Load())
			}
		})
	}

	t.Run("off a terminal, in CI, or turned off", func(t *testing.T) {
		f := newNoticeTest(t, "v0.3.0")
		f.latest = "v0.4.0"
		noticeAllowed = func(stderr *os.File) bool {
			// The real check, against a file.
			fi, err := stderr.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		}
		if got := f.run(t, "list"); got != "" || f.asks.Load() != 0 {
			t.Errorf("a file got the notice: %q", got)
		}
	})
}

// A slow feed costs a finished command at most noticeWait, and the answer
// the command did not wait for is still remembered for the next one.
func TestNotice_DoesNotHoldACommandForASlowFeed(t *testing.T) {
	f := newNoticeTest(t, "v0.3.0")
	f.latest, f.delay = "v0.4.0", noticeWait+500*time.Millisecond

	start := time.Now()
	got := f.run(t, "list")
	if waited := time.Since(start); waited > noticeWait+300*time.Millisecond {
		t.Errorf("the command waited %v for the feed", waited)
	}
	if got != "" {
		t.Errorf("printed %q with no answer yet", got)
	}

	deadline := time.Now().Add(3 * time.Second)
	for readReleaseCheck(f.cache).Latest != "v0.4.0" {
		if time.Now().After(deadline) {
			t.Fatal("the late answer was not remembered for the next command")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := f.run(t, "list"); !strings.Contains(got, "v0.3.0 → v0.4.0") {
		t.Errorf("the next command did not show the remembered answer:\n%s", got)
	}
}

func TestNoticeEnv(t *testing.T) {
	for _, env := range []string{envNoNotice, "CI"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "1")
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close(); _ = w.Close() }()
			if noticeAllowed(w) {
				t.Errorf("%s=1 allowed the notice", env)
			}
		})
	}
}
