package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// xcodeRunner answers CheckXcode's probes by the command they run: the
// developer directory, the version, and each xcodebuild probe's verdict.
type xcodeRunner struct {
	selected string
	version  string
	fail     map[string]error // keyed by xcodebuild's first flag
	seen     []string
}

func (f *xcodeRunner) Run(_ context.Context, argv []string) ([]byte, error) {
	f.seen = append(f.seen, strings.Join(argv, " "))
	switch argv[0] {
	case "xcode-select":
		return []byte(f.selected + "\n"), nil
	case "plutil":
		return []byte(f.version + "\n"), nil
	case "env":
		if err, ok := f.fail[argv[3]]; ok {
			return nil, err
		}
		return nil, nil
	}
	return nil, errors.New("unexpected " + argv[0])
}

// fakeXcode lays out the two files CheckXcode looks for, and returns the
// bundle and its developer directory.
func fakeXcode(t *testing.T) (app, dev string) {
	t.Helper()
	app = filepath.Join(t.TempDir(), "Xcode.app")
	dev = filepath.Join(app, "Contents", "Developer")
	for _, f := range []string{filepath.Join(app, "Contents", "Info.plist"), filepath.Join(dev, "usr", "bin", "xcodebuild")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return app, dev
}

func TestCheckXcode(t *testing.T) {
	refused := errors.New("exit status 69")
	for name, c := range map[string]struct {
		fail    map[string]error
		gone    bool   // the bundle is half-installed
		own     bool   // the runner's env names the developer directory
		clt     bool   // xcode-select names the Command Line Tools
		wantIn  string // empty: the check passes
		wantRun string // a command the check must have run
	}{
		"ready":              {wantRun: "-checkFirstLaunchStatus"},
		"licence":            {fail: map[string]error{"-license": refused}, wantIn: "Xcode 27.3 at %s needs its licence accepted: `sudo xcodebuild -license accept` on this Mac"},
		"first launch":       {fail: map[string]error{"-checkFirstLaunchStatus": refused}, wantIn: "needs its first-launch setup: `sudo xcodebuild -runFirstLaunch` on this Mac"},
		"mid update":         {gone: true, wantIn: "%s is missing or half-installed"},
		"not answering":      {fail: map[string]error{"-license": context.DeadlineExceeded}, wantIn: "did not answer `xcodebuild -license check` in time"},
		"the runner's own":   {own: true, fail: map[string]error{"-license": refused}, wantIn: "`sudo env DEVELOPER_DIR=%s/Contents/Developer xcodebuild -license accept`"},
		"command line tools": {clt: true},
	} {
		t.Run(name, func(t *testing.T) {
			app, dev := fakeXcode(t)
			r := &xcodeRunner{selected: dev, version: "27.3", fail: c.fail}
			if c.clt {
				r.selected = "/Library/Developer/CommandLineTools"
			}
			if c.gone {
				if err := os.Remove(filepath.Join(app, "Contents", "Info.plist")); err != nil {
					t.Fatal(err)
				}
			}
			own := ""
			if c.own {
				own, r.selected = dev, "/Applications/Other.app/Contents/Developer"
			}

			err := CheckXcode(t.Context(), r, own)
			if c.wantIn == "" {
				if err != nil {
					t.Fatalf("CheckXcode: %v", err)
				}
			} else if want := strings.ReplaceAll(c.wantIn, "%s", app); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want it to contain %q", err, want)
			}
			if c.wantRun != "" && !strings.Contains(strings.Join(r.seen, "\n"), c.wantRun) {
				t.Errorf("ran %v, want %s among them", r.seen, c.wantRun)
			}
			if c.clt && len(r.seen) != 1 {
				t.Errorf("probed the Command Line Tools: %v", r.seen)
			}
			if c.own && strings.Contains(strings.Join(r.seen, "\n"), "xcode-select") {
				t.Errorf("asked xcode-select with the runner's own DEVELOPER_DIR set: %v", r.seen)
			}
		})
	}
}

// Preflight asks it, so a broken Xcode makes the runner unavailable (§20).
func TestPreflight_XcodeMidUpdate(t *testing.T) {
	app, dev := fakeXcode(t)
	if err := os.RemoveAll(filepath.Join(app, "Contents", "Developer", "usr")); err != nil {
		t.Fatal(err)
	}
	// The session probe answers as a GUI login; the rest is Xcode's.
	e := newExecutor(t, Options{Runner: sessionThen{managerRunner("Aqua"), &xcodeRunner{selected: dev}}})
	e.goos = "darwin"
	if err := e.Preflight(t.Context()); err == nil || !strings.Contains(err.Error(), "half-installed") {
		t.Fatalf("Preflight = %v, want the runner refused while Xcode is half-installed", err)
	}
}

// sessionThen sends launchctl to one runner and everything else to another.
type sessionThen struct {
	session *fakeRunner
	rest    Runner
}

func (s sessionThen) Run(ctx context.Context, argv []string) ([]byte, error) {
	if argv[0] == "launchctl" {
		return s.session.Run(ctx, argv)
	}
	return s.rest.Run(ctx, argv)
}
