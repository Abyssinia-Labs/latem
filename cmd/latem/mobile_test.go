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
