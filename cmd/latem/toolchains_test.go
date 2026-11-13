package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// fakeMachine is what setup's toolchains step finds and does: a tool is
// found when its directory is on the PATH probed, and an install puts one
// there. Nothing on the machine running the tests is looked at or changed.
type fakeMachine struct {
	tc     system.Toolchains
	dirs   map[string]string // tool → its directory
	absent map[string]bool   // images Docker lacks
	// gives is what an install adds: a command line → tool and directory.
	gives    map[string][2]string
	installs []string
	probes   []string
	// brewOwner, when set, is the other user whose Homebrew this is.
	brewOwner string
	// notInstalled are programs lookPath does not find.
	notInstalled map[string]bool
}

func newFakeMachine() *fakeMachine {
	return &fakeMachine{dirs: map[string]string{}, absent: map[string]bool{}, gives: map[string][2]string{},
		notInstalled: map[string]bool{}}
}

func (m *fakeMachine) deps() toolchainDeps {
	return toolchainDeps{
		probe: func(_ context.Context, stepPath string) system.Toolchains {
			m.probes = append(m.probes, stepPath)
			tc := m.tc
			tc.Tools = map[string]bool{}
			for tool, dir := range m.dirs {
				tc.Tools[tool] = slices.Contains(filepath.SplitList(stepPath), dir)
			}
			return tc
		},
		image: func(_ context.Context, ref string) bool { return !m.absent[ref] },
		install: func(_ context.Context, argv []string, w io.Writer) error {
			// argv[0] is the program as lookPath found it.
			line := needs.CommandLine(append([]string{filepath.Base(argv[0])}, argv[1:]...))
			m.installs = append(m.installs, line)
			got, ok := m.gives[line]
			if !ok {
				return errors.New("exit status 1")
			}
			m.dirs[got[0]] = got[1]
			printf(w, "    (installed %s)\n", got[0])
			return nil
		},
		lookPath: func(name string) (string, error) {
			if m.notInstalled[name] {
				return "", errors.New(name + ": not found")
			}
			return "/opt/homebrew/bin/" + name, nil
		},
		brewOwner: func(string) (string, bool) {
			return m.brewOwner, m.brewOwner == ""
		},
		host:   func() system.Host { return system.Host{OS: "macOS", Arch: "arm64"} },
		stores: func(config.Paths) (secretStores, error) { return secretStores{}, nil },
	}
}

// setupMachine writes a Mac's config — a native macos runner and a docker
// linux one — with the repository app, whose files are files.
func setupMachine(t *testing.T, files fstest.MapFS) (string, *setupRecorder) {
	t.Helper()
	rec := fakeSetup(t, true)
	fakeRepoFiles(t, files)
	dir := filepath.Join(privateTempDir(t), "mac")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := renderInitConfig(initParams{Machine: "m", GOOS: "darwin", GOARCH: "arm64", Refs: []string{"main"}, Docker: true})
	body += "\n[repos.app]\nurl = \"https://github.com/me/app.git\"\nallowed_refs = [\"main\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, rec
}

var macRepo = fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: push
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - run: xcodegen generate
      - run: ruby scripts/version.rb
`)}}

// On a terminal: each missing tool is offered, an install that is keg-only
// is put on the runner's PATH in config.toml, and the probe that follows
// finds it there.
func TestSetupToolchains_OffersEachInstallAndPutsKegsOnThePath(t *testing.T) {
	dir, rec := setupMachine(t, macRepo)
	fakeInteractive(t)
	m := rec.machine
	m.dirs["xcodegen"], m.dirs["ruby"] = "/nowhere", "/nowhere"
	// /usr/local/bin is on the service's PATH on a Mac and on Linux.
	m.gives["brew install xcodegen"] = [2]string{"xcodegen", "/usr/local/bin"}
	m.gives["brew install ruby"] = [2]string{"ruby", "/opt/homebrew/opt/ruby/bin"}

	out, err := latemIn(t, dir, "y\ny\n", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !slices.Equal(m.installs, []string{"brew install ruby", "brew install xcodegen"}) {
		t.Errorf("installed %q", m.installs)
	}
	r, _ := loadConfigIn(t, dir).Runner("macos")
	if got := r.NativePathPrepend(); !slices.Equal(got, []string{"/opt/homebrew/opt/ruby/bin"}) {
		t.Errorf("path_prepend = %q", got)
	}
	for _, want := range []string{
		"3/6 toolchains",
		"xcodegen — for app apple.yml apple",
		"run `brew install xcodegen`? [y/N]",
		"path_prepend = [\"/opt/homebrew/opt/ruby/bin\"]",
		"nothing the jobs here need is missing now",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// Off a terminal nothing is installed; each fix is said.
func TestSetupToolchains_OffATerminalSaysTheFixes(t *testing.T) {
	dir, rec := setupMachine(t, macRepo)
	rec.machine.dirs["xcodegen"], rec.machine.dirs["ruby"] = "/nowhere", "/nowhere"

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if len(rec.machine.installs) != 0 {
		t.Errorf("installed %q off a terminal", rec.machine.installs)
	}
	for _, want := range []string{"fix: brew install xcodegen", "fix: brew install ruby", "2 still missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// With no repository, setup asks what the machine will build. A refused
// install stays missing, and what only a person can do is said.
func TestSetupToolchains_AsksWhatItWillBuild(t *testing.T) {
	rec := fakeSetup(t, false)
	fakeInteractive(t)
	rec.machine.tc.Xcodes = []system.XcodeInstall{{Version: system.Version{Major: 27, Text: "27.0"}}}
	dir := filepath.Join(privateTempDir(t), "mac")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := renderInitConfig(initParams{Machine: "m", GOOS: "darwin", GOARCH: "arm64", Refs: []string{"main"}})
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := latemIn(t, dir, "\nios, windows\nn\n", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{
		"what will this machine build? ios, macos, android, linux",
		`"windows" is not one of`,
		"an iOS simulator runtime — for iOS apps",
		"run `xcodebuild -downloadPlatform iOS`? [y/N]",
		"1 still missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(rec.machine.installs) != 0 {
		t.Errorf("installed %q after a no", rec.machine.installs)
	}
}

// A job the native runner refuses only for its remote actions still goes
// there once allow_unisolated is set, so what it needs is still said — a
// fresh account's take-one apple job was left out, and the step said nothing.
func TestSetupToolchains_AJobRefusedForItsActionsStillCounts(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: push
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - uses: oven-sh/setup-bun@0123456789abcdef0123456789abcdef01234567
      - run: xcodegen generate
`)}})
	rec.machine.dirs["xcodegen"] = "/nowhere"

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"xcodegen — for app apple.yml apple", "fix: brew install xcodegen"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A repository none of whose jobs a runner here takes is said, not passed
// over in silence.
func TestSetupToolchains_NoJobComesHere(t *testing.T) {
	dir, _ := setupMachine(t, fstest.MapFS{".github/workflows/win.yml": {Data: []byte(
		"on: push\njobs:\n  win:\n    runs-on: windows-latest\n    steps:\n      - run: echo hi\n")}})

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no job in app goes to a runner here") {
		t.Errorf("output:\n%s", out)
	}
}

// A Homebrew another user installed — the usual case for an account made
// for the agent — is not offered as an install this account can do.
func TestSetupToolchains_AnotherUsersHomebrew(t *testing.T) {
	dir, rec := setupMachine(t, macRepo)
	fakeInteractive(t)
	m := rec.machine
	m.brewOwner = "michael"
	m.dirs["xcodegen"], m.dirs["ruby"] = "/nowhere", "/nowhere"
	m.gives["brew install xcodegen"] = [2]string{"xcodegen", "/usr/local/bin"}

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if len(m.installs) != 0 {
		t.Errorf("installed %q into another user's Homebrew", m.installs)
	}
	if strings.Contains(out, "run `brew install") {
		t.Errorf("offered an install this account cannot do:\n%s", out)
	}
	if !strings.Contains(out, "brew install xcodegen — Homebrew here belongs to michael") {
		t.Errorf("output:\n%s", out)
	}
}

// A program on the agent's PATH and not this shell's is found: an account
// made for the agent may never have put Homebrew on its own.
func TestLookPathOr(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brew-for-test"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-executable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if got, err := lookPathOr("brew-for-test", "/nonexistent:"+dir); err != nil || got != filepath.Join(dir, "brew-for-test") {
		t.Errorf("lookPathOr = %q, %v", got, err)
	}
	if _, err := lookPathOr("not-executable", dir); err == nil {
		t.Error("found a file that cannot run")
	}
	if _, err := lookPathOr("nowhere-at-all", dir); err == nil {
		t.Error("found what is not there")
	}
}

// A docker runner's image is pulled before a first job would wait on it.
func TestSetupToolchains_PullsADockerRunnersImage(t *testing.T) {
	dir, rec := setupMachine(t, fstest.MapFS{})
	fakeInteractive(t)
	r, _ := loadConfigIn(t, dir).Runner("linux")
	img := r.Docker.DefaultImage
	rec.machine.absent[img] = true
	rec.machine.gives["docker pull -- "+img] = [2]string{"image", "/"}

	out, err := latemIn(t, dir, "y\n", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !slices.Equal(rec.machine.installs, []string{"docker pull -- " + img}) {
		t.Errorf("installed %q", rec.machine.installs)
	}
	if !strings.Contains(out, "its image, "+img+" — for the linux runner's jobs") {
		t.Errorf("output:\n%s", out)
	}
}

func TestKegConfig(t *testing.T) {
	for _, c := range []struct {
		argv    []string
		goos    string
		prepend []string
		env     map[string]string
	}{
		{brewArgv("openjdk@17"), "darwin", []string{"/opt/homebrew/opt/openjdk@17/bin"},
			map[string]string{"JAVA_HOME": "/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home"}},
		{brewArgv("openjdk@17"), "linux", []string{"/opt/homebrew/opt/openjdk@17/bin"},
			map[string]string{"JAVA_HOME": "/opt/homebrew/opt/openjdk@17/libexec"}},
		{brewArgv("ruby"), "darwin", []string{"/opt/homebrew/opt/ruby/bin"}, nil},
		{brewArgv("--cask", "android-commandlinetools"), "darwin", nil,
			map[string]string{"ANDROID_HOME": "/opt/homebrew/share/android-commandlinetools"}},
		{brewArgv("xcodegen"), "darwin", nil, nil},
		{[]string{"xcodebuild", "-downloadPlatform", "iOS"}, "darwin", nil, nil},
	} {
		prepend, env := kegConfig(c.argv, "/opt/homebrew", c.goos)
		if !slices.Equal(prepend, c.prepend) || len(env) != len(c.env) || env["JAVA_HOME"] != c.env["JAVA_HOME"] ||
			env["ANDROID_HOME"] != c.env["ANDROID_HOME"] {
			t.Errorf("kegConfig(%q, %s) = %q, %v", c.argv, c.goos, prepend, env)
		}
	}
}

func brewArgv(args ...string) []string { return append([]string{"brew", "install"}, args...) }

// setRunnerKey finds a [[runner]] by its place among them, adds a table at
// the end of that runner's own, and leaves the other runner's alone.
func TestSetRunnerKey(t *testing.T) {
	body := `schema_version = 1

[[runner]]
name = "macos"
executor = "native"
labels = ["macos"]

  [runner.native]
  min_free_gb = 80

# the Linux one
[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.docker]
  default_image = "img:1"
`
	cfg := &config.Config{Runners: []*config.Runner{{Name: "macos"}, {Name: "linux"}}}
	got, err := setRunnerKey(body, cfg, "macos", []string{"native"}, "path_prepend", `["/opt/a/bin"]`)
	if err != nil {
		t.Fatal(err)
	}
	got, err = setRunnerKey(got, cfg, "macos", []string{"native", "env"}, "JAVA_HOME", `"/opt/jdk"`)
	if err != nil {
		t.Fatal(err)
	}
	want := `schema_version = 1

[[runner]]
name = "macos"
executor = "native"
labels = ["macos"]

  [runner.native]
  min_free_gb = 80
  path_prepend = ["/opt/a/bin"]

  [runner.native.env]
  JAVA_HOME = "/opt/jdk"

# the Linux one
[[runner]]
name = "linux"
executor = "docker"
labels = ["linux"]

  [runner.docker]
  default_image = "img:1"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, err := setRunnerKey(body, cfg, "gpu", []string{"native"}, "k", `"v"`); err == nil {
		t.Error("a runner that is not there was edited")
	}
}

// A script's or a service's stdin is often /dev/null, a character device
// like a terminal; nothing there will answer a question.
func TestIsTerminal_NotDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTerminal(f) {
		t.Error("/dev/null was taken for a terminal")
	}
}
