package main

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// needsDoctor is a doctor with a native Mac runner and a docker one, whose
// repository "app" reads as files and whose machine has tc.
func needsDoctor(t *testing.T, files fstest.MapFS, tc system.Toolchains) *doctor {
	t.Helper()
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners[0].Labels = []string{"linux", "docker"}
	cfg.Runners = append(cfg.Runners, &config.Runner{Name: "mac", Executor: config.ExecutorNative,
		Labels: []string{"macos", "arm64"}, Concurrency: 1})
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)
	d.cfg = cfg
	d.readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		return &repoFiles{fsys: files, sha: strings.Repeat("c", 40), where: "the mirror", close: func() {}}, nil
	}
	d.toolchains = func(context.Context, string) system.Toolchains { return tc }
	return d
}

var appleRepo = fstest.MapFS{".github/workflows/apple.yml": {Data: []byte(`on: pull_request
jobs:
  apple:
    runs-on: macos-latest
    steps:
      - run: DEVELOPER_DIR=/Applications/Xcode_27.app xcodebuild -scheme App -destination 'platform=iOS Simulator,name=iPhone 17' test
  web:
    runs-on: ubuntu-latest
    steps:
      - run: docker build .
`)}}

// What the Mac mini this was written for would hear: its jobs want Xcode 27
// and a simulator; it has 26.0.1 and no runtime. And the toolchain checks no
// job here needs are skipped, the rest say who needs them.
func TestDoctorNeeds_NamesWhatTheMachinesJobsLack(t *testing.T) {
	d := needsDoctor(t, appleRepo, system.Toolchains{
		Xcodes: []system.XcodeInstall{{Version: system.Version{Major: 26, Text: "26.0.1"}}},
		Tools:  map[string]bool{},
	})
	checks := d.checkNeeds(t.Context())
	got := checkByID(checks, "needs.app")
	if got == nil || got.Level != levelWarn {
		t.Fatalf("needs.app = %+v, want a warning", got)
	}
	text := got.Detail + "\n" + strings.Join(got.Notes, "\n")
	for _, want := range []string{
		"2 jobs run here (1 Linux with a Docker daemon, 1 iOS — Xcode 27); 3 of what they need is missing here",
		"apple.yml apple needs Xcode 27; this machine has Xcode 26.0.1",
		"apple.yml apple needs an iOS simulator runtime",
		"apple.yml web needs a Docker daemon",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("check lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(strings.Join(got.Fix, " "), "xcodebuild -downloadPlatform iOS") ||
		!strings.Contains(strings.Join(got.Manual, " "), "native Linux runner") {
		t.Errorf("fix %q, by hand %q", got.Fix, got.Manual)
	}

	gated := d.gateByNeeds([]check{
		warn("java.version", "JDK", "not on PATH"),
		warn("xcode.simruntime", "iOS simulator runtime", "none installed"),
	})
	if gated[0].Level != levelSkip || !strings.Contains(gated[0].Detail, "no job this machine takes needs it") {
		t.Errorf("java.version = %+v, want skipped: nothing here needs a JDK", gated[0])
	}
	if gated[1].Level != levelWarn || !strings.Contains(strings.Join(gated[1].Notes, " "), "Needed by app apple.yml apple") {
		t.Errorf("xcode.simruntime = %+v, want it to name the job", gated[1])
	}
}

func TestDoctorNeeds_NothingMissing(t *testing.T) {
	d := needsDoctor(t, fstest.MapFS{".github/workflows/ci.yml": {Data: []byte(
		"on: push\njobs:\n  test:\n    runs-on: macos-latest\n    steps:\n      - run: xcodegen generate\n")}},
		system.Toolchains{Tools: map[string]bool{"xcodegen": true}})
	got := checkByID(d.checkNeeds(t.Context()), "needs.app")
	if got == nil || got.Level != levelPass || !strings.Contains(got.Detail, "nothing they need is missing") {
		t.Errorf("needs.app = %+v", got)
	}
}

// Until the agent has a mirror, doctor cannot know, and gates nothing.
func TestDoctorNeeds_NoMirrorGatesNothing(t *testing.T) {
	d := needsDoctor(t, nil, system.Toolchains{})
	d.readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		return nil, context.Canceled
	}
	got := checkByID(d.checkNeeds(t.Context()), "needs.app")
	if got == nil || got.Level != levelSkip {
		t.Errorf("needs.app = %+v, want a skip", got)
	}
	if g := d.gateByNeeds([]check{warn("java.version", "JDK", "not on PATH")}); g[0].Level != levelWarn {
		t.Errorf("java.version was gated with a repository unread: %+v", g[0])
	}
}

// A native runner's path_prepend is on its steps' PATH (A60), so the probe
// that decides what is missing looks there too; a docker runner's job is
// never probed on the host.
func TestDoctorNeeds_ProbesTheRunnersPath(t *testing.T) {
	d := needsDoctor(t, appleRepo, system.Toolchains{})
	d.cfg.Runners[1].Native = &config.NativeRunner{PathPrepend: []string{"/opt/homebrew/opt/ruby/bin"}}
	var paths []string
	d.toolchains = func(_ context.Context, p string) system.Toolchains {
		paths = append(paths, p)
		return system.Toolchains{Tools: map[string]bool{}}
	}
	d.checkNeeds(t.Context())
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "/opt/homebrew/opt/ruby/bin:") {
		t.Errorf("probed with %q, want one probe of the mac runner's PATH", paths)
	}
}
