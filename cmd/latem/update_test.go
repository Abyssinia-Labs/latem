package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// fakeReleaser stands in for the feed and the download.
type fakeReleaser struct {
	rel      selfupdate.Release
	checkErr error
	fetchErr error
	fetched  bool
}

func (f *fakeReleaser) Check(context.Context) (selfupdate.Release, error) {
	return f.rel, f.checkErr
}

func (f *fakeReleaser) Fetch(_ context.Context, _ selfupdate.Release, dir string) (string, error) {
	f.fetched = true
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	path := filepath.Join(dir, ".latem.new")
	return path, os.WriteFile(path, []byte("the new binary"), 0o755)
}

// updateFixture replaces every seam `latem update` has: the version, the
// releases, the binary it would replace, and the agent it would signal.
type updateFixture struct {
	rel    *fakeReleaser
	target string
	asked  []string
	answer agent.ReloadStatus
	err    error
}

func newUpdateTest(t *testing.T, running string) *updateFixture {
	t.Helper()
	f := &updateFixture{rel: &fakeReleaser{rel: selfupdate.Release{Version: "v0.4.0", Bare: "0.4.0"}}}
	f.target = filepath.Join(t.TempDir(), "latem")
	if err := os.WriteFile(f.target, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.err = agent.ErrNoAgent

	oldVersion, oldReleaser, oldSelf, oldSignal := version, newReleaser, selfBinary, signalSwitch
	t.Cleanup(func() {
		version, newReleaser, selfBinary, signalSwitch = oldVersion, oldReleaser, oldSelf, oldSignal
	})
	version = running
	newReleaser = func() (releaser, error) { return f.rel, nil }
	selfBinary = func() (string, error) { return f.target, nil }
	signalSwitch = func(_ config.Paths, want string, _ time.Duration) (agent.ReloadStatus, error) {
		f.asked = append(f.asked, want)
		return f.answer, f.err
	}
	global.configDir = t.TempDir()
	t.Cleanup(func() { global.configDir = "" })
	asHomebrews(t, "")
	return f
}

// asHomebrews makes this binary one Homebrew installed with its opt link at
// link, or with "" one it did not, whatever installed the Go that built the
// test.
func asHomebrews(t *testing.T, link string) {
	t.Helper()
	old := homebrewLink
	t.Cleanup(func() { homebrewLink = old })
	homebrewLink = func() (string, bool) { return link, link != "" }
}

func (f *updateFixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdate_InstallsANewerReleaseAndMovesTheAgent(t *testing.T) {
	f := newUpdateTest(t, "v0.3.0")
	f.err = nil
	f.answer = agent.ReloadStatus{PID: 42, Version: "v0.4.0"}

	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, false); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if got := f.installed(t); got != "the new binary" {
		t.Errorf("on disk: %q, want the release", got)
	}
	if len(f.asked) != 1 || f.asked[0] != "v0.4.0" {
		t.Errorf("asked the agent to move onto %v, want [v0.4.0]", f.asked)
	}
	for _, want := range []string{"installed latem v0.4.0 at " + f.target + " (was v0.3.0)", "the agent (pid 42) is now running v0.4.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestUpdate_CheckChangesNothing(t *testing.T) {
	f := newUpdateTest(t, "v0.3.0")
	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, true); err != nil {
		t.Fatalf("runUpdate --check: %v", err)
	}
	if f.rel.fetched || f.installed(t) != "the old binary" || len(f.asked) != 0 {
		t.Errorf("--check downloaded, installed or signalled: fetched=%v asked=%v", f.rel.fetched, f.asked)
	}
	if !strings.Contains(out.String(), "latem v0.4.0 is available; this is v0.3.0") {
		t.Errorf("output: %s", out.String())
	}
}

// Up to date still makes sure the agent runs this binary: it may have been
// replaced by hand, or by an update whose agent restarted on the old one.
func TestUpdate_UpToDateStillMovesAnOlderAgent(t *testing.T) {
	f := newUpdateTest(t, "v0.4.0")
	f.rel.checkErr = selfupdate.ErrUpToDate
	f.err = nil
	f.answer = agent.ReloadStatus{PID: 42, Version: "v0.4.0"}

	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, false); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if f.rel.fetched {
		t.Error("an up-to-date binary downloaded a release")
	}
	if len(f.asked) != 1 || f.asked[0] != "v0.4.0" {
		t.Errorf("asked %v, want the agent on this binary's v0.4.0", f.asked)
	}
	if !strings.Contains(out.String(), "latem v0.4.0 is up to date") {
		t.Errorf("output: %s", out.String())
	}
}

func TestUpdate_Refusals(t *testing.T) {
	t.Run("a dev build", func(t *testing.T) {
		f := newUpdateTest(t, selfupdate.DevVersion)
		err := runUpdate(context.Background(), &bytes.Buffer{}, false)
		if err == nil || !strings.Contains(err.Error(), "development build") {
			t.Errorf("err = %v", err)
		}
		if f.rel.fetched {
			t.Error("a dev build downloaded a release")
		}
	})

	t.Run("a feed that is not public", func(t *testing.T) {
		f := newUpdateTest(t, "v0.3.0")
		f.rel.checkErr = errors.New("the release feed answered 404 Not Found")
		err := runUpdate(context.Background(), &bytes.Buffer{}, false)
		if err == nil || !strings.Contains(err.Error(), "not public") {
			t.Errorf("err = %v, want it to say why a 404", err)
		}
	})

	t.Run("a release that does not verify", func(t *testing.T) {
		f := newUpdateTest(t, "v0.3.0")
		f.rel.fetchErr = errors.New("latem_0.4.0_checksums.txt is not signed by the key this build trusts")
		err := runUpdate(context.Background(), &bytes.Buffer{}, false)
		if err == nil || !strings.Contains(err.Error(), "refusing latem v0.4.0") {
			t.Errorf("err = %v", err)
		}
		if f.installed(t) != "the old binary" || len(f.asked) != 0 {
			t.Error("a refused release replaced the binary or moved the agent")
		}
	})

	t.Run("a directory it cannot write", func(t *testing.T) {
		f := newUpdateTest(t, "v0.3.0")
		f.rel.fetchErr = &os.PathError{Op: "open", Path: "/usr/local/bin/.latem.new", Err: os.ErrPermission}
		err := runUpdate(context.Background(), &bytes.Buffer{}, false)
		if err == nil || !strings.Contains(err.Error(), "not writable by this user") {
			t.Errorf("err = %v", err)
		}
	})
}

// A keg is Homebrew's: it records the version there and deletes the keg on
// the next upgrade, so `latem update` writes nothing into it and moves no
// agent. It names the command that does update it (A78).
func TestUpdate_AHomebrewInstallIsHomebrews(t *testing.T) {
	f := newUpdateTest(t, "v0.3.0")
	asHomebrews(t, "/opt/homebrew/opt/latem/bin/latem")

	err := runUpdate(context.Background(), &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade latem") ||
		!strings.Contains(err.Error(), "/opt/homebrew/opt/latem/bin/latem") {
		t.Errorf("err = %v, want it to name the link and `brew upgrade latem`", err)
	}
	if f.rel.fetched || f.installed(t) != "the old binary" || len(f.asked) != 0 {
		t.Errorf("a Homebrew install downloaded, installed or signalled: fetched=%v asked=%v", f.rel.fetched, f.asked)
	}

	// --check changes nothing anywhere, so it still answers.
	var out bytes.Buffer
	if err := runUpdate(context.Background(), &out, true); err != nil {
		t.Fatalf("runUpdate --check: %v", err)
	}
	if !strings.Contains(out.String(), "latem v0.4.0 is available") ||
		!strings.Contains(out.String(), "run `brew upgrade latem` to install it") {
		t.Errorf("--check on a Homebrew install said:\n%s", out.String())
	}
}

// What the command says for each answer the agent can give. The binary is
// installed in every case; none of these fails the command.
func TestUpdate_ReportsTheAgentsAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		answer agent.ReloadStatus
		err    error
		want   string
	}{
		"no agent":         {err: agent.ErrNoAgent, want: "no agent is running; the next one to start runs v0.4.0"},
		"an older agent":   {err: agent.ErrCannotSwitch, want: "predates `latem update`"},
		"a busy agent":     {answer: agent.ReloadStatus{PID: 7, Version: "v0.3.0", Switch: &agent.SwitchState{Requests: 1, State: agent.SwitchWaiting, Runs: 2}}, want: "moves onto v0.4.0 once its 2 run(s) in flight finish"},
		"another binary":   {answer: agent.ReloadStatus{PID: 7, Version: "v0.3.0", Binary: "/opt/latem", Switch: &agent.SwitchState{Requests: 1, State: agent.SwitchCurrent, To: "v0.3.0"}}, want: "runs /opt/latem, not"},
		"a failed move":    {answer: agent.ReloadStatus{PID: 7, Version: "v0.3.0", Switch: &agent.SwitchState{Requests: 1, State: agent.SwitchFailed, Error: "exec format error"}}, want: "could not move onto v0.4.0: exec format error"},
		"a signal failure": {err: errors.New("operation not permitted"), want: "has not moved onto v0.4.0"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newUpdateTest(t, "v0.3.0")
			f.answer, f.err = c.answer, c.err
			var out bytes.Buffer
			if err := runUpdate(context.Background(), &out, false); err != nil {
				t.Fatalf("runUpdate: %v", err)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, out.String())
			}
		})
	}
}
