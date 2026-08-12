package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The mobile toolchain checks all share one shape: a *working* tool is
// installed and the one on PATH is the wrong one. Every naive check — is ruby
// present, does java run — passes on a machine that cannot build, which is why
// these exist at all (manual-baseline.md §7.5, findings 4, 10, 13, 14, 16).

// The bundler check must follow the Ruby the check above chose, not PATH.
//
// This is the specific false pass that shipped in the first draft: with macOS's
// 2.6 on PATH and a usable 3.4 keg-only, `bundle --version` reports 2.6's
// bundler and the check said OK — directly beneath a line calling that Ruby
// unusable. It reported a working toolchain on a machine that could not build,
// and it did so more confidently the more broken the machine was.
func TestBundlerFollowsTheChosenRuby(t *testing.T) {
	dir := t.TempDir()
	pathBin := filepath.Join(dir, "usr-bin") // the wrong ruby, on PATH
	kegBin := filepath.Join(dir, "brew-bin") // the right ruby, off PATH
	for _, p := range []string{pathBin, kegBin} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ToolAt stats the path before running it, so the file must exist and be
	// executable; the fake runner supplies the banner.
	touchExec(t, filepath.Join(kegBin, "bundle"))

	cmds := map[string]fakeCmd{
		"bundle --version": {out: "Bundler version 1.17.2"},
		filepath.Join(kegBin, "bundle") + " --version": {out: "4.0.18"},
	}
	d := &doctor{sys: newSystemForTest("darwin", dir, cmds, "bundle")}

	// With a chosen Ruby, the bundler beside it wins over the one on PATH.
	got := d.checkBundler(context.Background(), kegBin)
	if !strings.Contains(got.Detail, "4.0.18") {
		t.Errorf("bundler check reported %q; want the one beside the chosen ruby (4.0.18)", got.Detail)
	}
	if strings.Contains(got.Detail, "1.17.2") {
		t.Error("bundler check reported the PATH bundler, which belongs to the wrong Ruby")
	}
}

// A bundler that belongs to a different Ruby than the chosen one is a warning,
// not a pass — reporting it as fine is how the false pass reappears.
func TestBundlerFromAnotherRubyWarns(t *testing.T) {
	dir := t.TempDir()
	kegBin := filepath.Join(dir, "brew-bin")
	if err := os.MkdirAll(kegBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// kegBin deliberately has no bundle at all; only the PATH one exists.
	cmds := map[string]fakeCmd{"bundle --version": {out: "Bundler version 1.17.2"}}
	d := &doctor{sys: newSystemForTest("darwin", dir, cmds, "bundle")}
	got := d.checkBundler(context.Background(), kegBin)
	if got.Level == levelPass {
		t.Errorf("a bundler belonging to another Ruby passed: %q", got.Detail)
	}
}

// touchExec creates an executable file. ToolAt stats before running, so the
// probe needs something on disk even though the fake runner answers the exec.
func touchExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a probe target must be executable
		t.Fatal(err)
	}
}

// The JDK check must have BOTH bounds.
//
// The first version tested only `> 17` and passed JDK 11 — reported "ok" on a
// Mac mini that could not build an Android release at all. Found by running
// doctor on a second machine, which is the only reason it was found: the
// machine it was written on happened to have a JDK that was too new, so the
// one branch that existed was the one being exercised.
func TestJDKRejectsBothTooOldAndTooNew(t *testing.T) {
	for _, tc := range []struct {
		name, banner string
		wantPass     bool
	}{
		{"the mini's JDK 11", `java version "11.0.20" 2023-07-18 LTS`, false},
		{"ancient 8", `java version "1.8.0_402"`, false},
		{"exactly 17", `openjdk version "17.0.20" 2026-07-21`, true},
		{"too new 26", `openjdk version "26.0.2" 2026-01-20`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmds := map[string]fakeCmd{"java -version": {out: tc.banner}}
			d := &doctor{sys: newSystemForTest("darwin", dir, cmds, "java")}
			got := d.checkJDK(context.Background())
			if pass := got.Level == levelPass; pass != tc.wantPass {
				t.Errorf("%s: level=%v (pass=%v), want pass=%v — %q",
					tc.banner, got.Level, pass, tc.wantPass, got.Detail)
			}
		})
	}
}

// The simulator-runtime check must catch BOTH "none installed" and the case
// that actually happened on the mini: runtimes present, none matching the SDK.
//
// A check written only for the first would have passed on the machine that
// failed — it had an iOS 17.2 runtime and an iOS 26 SDK.
func TestSimulatorRuntimeMatchesSDK(t *testing.T) {
	const sdk = "26.0\n"
	runtimes := func(json string) map[string]fakeCmd {
		return map[string]fakeCmd{
			"xcrun --sdk iphonesimulator --show-sdk-version": {out: sdk},
			"xcrun simctl list runtimes -j":                  {out: json},
		}
	}
	for _, tc := range []struct {
		name, json string
		wantPass   bool
	}{
		{"matching runtime", `{"runtimes":[{"platform":"iOS","version":"26.0","buildversion":"23A343","isAvailable":true}]}`, true},
		{"the mini: only 17.2", `{"runtimes":[{"platform":"iOS","version":"17.2","buildversion":"21C62","isAvailable":true}]}`, false},
		{"none at all", `{"runtimes":[]}`, false},
		{"right version but unavailable", `{"runtimes":[{"platform":"iOS","version":"26.0","buildversion":"23A343","isAvailable":false}]}`, false},
		{"only watchOS", `{"runtimes":[{"platform":"watchOS","version":"26.0","buildversion":"23A343","isAvailable":true}]}`, false},
		{"several, one matches", `{"runtimes":[{"platform":"iOS","version":"18.5","buildversion":"22F77","isAvailable":true},{"platform":"iOS","version":"26.0","buildversion":"23A343","isAvailable":true}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &doctor{sys: newSystemForTest("darwin", t.TempDir(), runtimes(tc.json), "xcrun")}
			got := d.checkSimulatorRuntime(context.Background())
			if pass := got.Level == levelPass; pass != tc.wantPass {
				t.Errorf("level=%v (pass=%v), want pass=%v — %q", got.Level, pass, tc.wantPass, got.Detail)
			}
		})
	}
}
