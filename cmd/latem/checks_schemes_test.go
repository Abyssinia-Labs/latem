package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// ------------------------------------------------------------------- A34

// mirrorWithTree makes a bare mirror directory and teaches the fake machine
// what `git ls-tree` reports for it.
func mirrorWithTree(t *testing.T, cfg *config.Config, cmds map[string]fakeCmd, repo string, paths ...string) {
	t.Helper()
	mirror := filepath.Join(cfg.Paths.Mirrors, repo+".git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	key := strings.Join([]string{"git", "-C", mirror, "ls-tree", "-r", "--name-only", "HEAD"}, " ")
	cmds[key] = fakeCmd{out: strings.Join(paths, "\n") + "\n"}
}

// macOSConfig is testConfig with a native runner, which is what scopes A34's
// check.
func macOSConfig(dir string) *config.Config {
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{{
		Name:        "ios",
		Executor:    config.ExecutorNative,
		Concurrency: 1,
		Native:      &config.NativeRunner{MinFreeGB: 60},
	}}
	return cfg
}

func TestDoctorSchemes(t *testing.T) {
	cases := map[string]struct {
		tree []string
		want level
		says string
	}{
		"a project with no shared scheme": {
			tree: []string{
				"LatemExample/AppDelegate.swift",
				"LatemExample.xcodeproj/project.pbxproj",
				// xcuserdata is what Xcode writes by default, and nobody
				// commits it — but if someone did, it still is not a shared
				// scheme and must not satisfy the check.
				"LatemExample.xcodeproj/xcuserdata/me.xcuserdatad/xcschemes/LatemExample.xcscheme",
			},
			want: levelFail,
			says: "LatemExample.xcodeproj",
		},
		"a project with one": {
			tree: []string{
				"LatemExample.xcodeproj/project.pbxproj",
				"LatemExample.xcodeproj/xcshareddata/xcschemes/LatemExample.xcscheme",
			},
			want: levelPass,
			says: "LatemExample",
		},
		"a workspace with one": {
			tree: []string{
				"App.xcworkspace/contents.xcworkspacedata",
				"App.xcworkspace/xcshareddata/xcschemes/App.xcscheme",
			},
			want: levelPass,
		},
		"a repository with no Xcode project is not this check's business": {
			tree: []string{"go.mod", "main.go"},
			want: levelSkip,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := privateDir(t)
			cfg := macOSConfig(dir)
			cmds := healthyRunner("darwin")
			mirrorWithTree(t, cfg, cmds, "app", tc.tree...)

			d := newTestDoctor("darwin", dir, cmds, cfg, nil)
			got := d.checkSchemes(t.Context())
			if len(got) != 1 {
				t.Fatalf("got %d checks, want 1", len(got))
			}
			if got[0].Level != tc.want {
				t.Fatalf("level = %v, want %v (detail %q)", got[0].Level, tc.want, got[0].Detail)
			}
			if tc.says != "" && !strings.Contains(got[0].Detail, tc.says) {
				t.Fatalf("detail %q does not mention %q", got[0].Detail, tc.says)
			}
			if got[0].Level == levelFail && len(got[0].Fix) == 0 {
				t.Error("a failing scheme check carries no fix")
			}
		})
	}
}

// Before the first run there is no mirror, and the honest answer is that this
// check has nothing to read yet — not that the repository is fine.
func TestDoctorSchemes_NoMirrorYet(t *testing.T) {
	dir := privateDir(t)
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), macOSConfig(dir), nil)

	got := d.checkSchemes(t.Context())
	if len(got) != 1 || got[0].Level != levelSkip {
		t.Fatalf("got %v, want one skip", got)
	}
	if !strings.Contains(got[0].Detail, "no mirror yet") {
		t.Fatalf("detail = %q", got[0].Detail)
	}
}

// A34 scopes the check to macOS runners. On a docker-only machine an Xcode
// scheme is not something the operator can act on, and reporting it is noise.
func TestDoctorSchemes_OnlyForMacOSRunners(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir) // docker runner
	cmds := healthyRunner("darwin")
	mirrorWithTree(t, cfg, cmds, "app", "LatemExample.xcodeproj/project.pbxproj")

	d := newTestDoctor("darwin", dir, cmds, cfg, nil)
	if got := d.checkSchemes(t.Context()); len(got) != 0 {
		t.Fatalf("got %v, want nothing on a docker-only machine", got)
	}
}

// A mirror this process cannot read is a warning: the check failed, the
// repository did not.
func TestDoctorSchemes_UnreadableMirror(t *testing.T) {
	dir := privateDir(t)
	cfg := macOSConfig(dir)
	mirror := filepath.Join(cfg.Paths.Mirrors, "app.git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	// No ls-tree answer in the table, so the fake git fails.
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)

	got := d.checkSchemes(t.Context())
	if len(got) != 1 || got[0].Level != levelWarn {
		t.Fatalf("got %v, want one warning", got)
	}
}

func TestMirrorPathFor(t *testing.T) {
	if _, err := mirrorPathFor("/var/latem/mirrors", "app"); err != nil {
		t.Fatalf("a plain key was refused: %v", err)
	}
	// doctor runs on the machine whose config may be the thing that is wrong,
	// so a key that climbs out must not become a path this process stats.
	for _, key := range []string{"../../etc/passwd", "a/b", "sub/../../x"} {
		if _, err := mirrorPathFor("/var/latem/mirrors", key); err == nil {
			t.Errorf("key %q was accepted", key)
		}
	}

	// A bare ".." is contained rather than refused, and that is not a hole: the
	// ".git" suffix makes it the filename "...git" rather than a directory to
	// climb through. Asserted so that a future reader does not "fix" it.
	if got, err := mirrorPathFor("/var/latem/mirrors", ".."); err != nil || got != "/var/latem/mirrors/...git" {
		t.Errorf("mirrorPathFor(..) = %q, %v", got, err)
	}

	// The rule is containment, not spelling, and it matches internal/source's
	// own mirrorPath deliberately: a key filepath.Join cleans back into the
	// mirror directory names the file source would have created there, and a
	// doctor that refused to look at it would report a mirror as missing.
	if got, err := mirrorPathFor("/var/latem/mirrors", "/abs"); err != nil || got != "/var/latem/mirrors/abs.git" {
		t.Errorf("mirrorPathFor(/abs) = %q, %v", got, err)
	}
}

func TestScanForSchemes_SchemeNestedUnderXcschemesDoesNotCount(t *testing.T) {
	// Xcode reads direct children of xcschemes/ only, so a scheme filed in a
	// subdirectory is a file that looks like a scheme and is not one.
	projects, schemes := scanForSchemes(strings.Join([]string{
		"App.xcodeproj/project.pbxproj",
		"App.xcodeproj/xcshareddata/xcschemes/old/App.xcscheme",
	}, "\n"))
	if len(projects) != 1 {
		t.Fatalf("projects = %v", projects)
	}
	if len(schemes) != 0 {
		t.Fatalf("schemes = %v, want none", schemes)
	}
}
