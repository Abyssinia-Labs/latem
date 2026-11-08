package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// A directory named literally is everything beneath it, because that is what
// upload-artifact's own documentation says — and it is decided when collecting,
// since at compile time `dist` usually does not exist yet.
func TestCollectNamedExpandsADirectoryAndSubtracts(t *testing.T) {
	ws := workspace(t, map[string]string{
		"dist/app.bin":        "bin",
		"dist/sub/lib.so":     "lib",
		"dist/sub/debug.log":  "log",
		"reports/junit.xml":   "xml",
		"reports/private.key": "no",
	})
	c := NewCollector(t.TempDir())

	refs, err := c.CollectNamed(t.Context(), "r_up", ws, Upload{
		Name:    "build output",
		Include: []string{"dist", "reports/*.xml"},
		Exclude: []string{"dist/**/*.log"},
		Retain:  24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("CollectNamed: %v", err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, r.Path)
		if r.ExpiresAt == nil {
			t.Errorf("%s has no expiry", r.Path)
		}
	}
	sort.Strings(got)
	want := []string{"build output/dist/app.bin", "build output/dist/sub/lib.so", "build output/reports/junit.xml"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("collected %q, want %q", got, want)
	}

	dir, _ := c.Dir("r_up")
	if b, err := os.ReadFile(filepath.Join(dir, "build output", "dist", "sub", "lib.so")); err != nil || string(b) != "lib" {
		t.Errorf("stored copy = %q, %v", b, err)
	}
}

// Nothing matched is not an error here — whether it is one is the step's
// if-no-files-found, which the run loop decides.
func TestCollectNamedWithNothingMatched(t *testing.T) {
	ws := workspace(t, map[string]string{"a.txt": "a"})
	refs, err := NewCollector(t.TempDir()).CollectNamed(t.Context(), "r_none", ws, Upload{
		Name: "x", Include: []string{"missing/**"},
	})
	if err != nil || len(refs) != 0 {
		t.Errorf("refs = %v, err = %v", refs, err)
	}
}

// The same containment Collect has: a pattern cannot climb out, and a name
// cannot become a path.
func TestCollectNamedRefusals(t *testing.T) {
	ws := workspace(t, map[string]string{"a.txt": "a"})
	c := NewCollector(t.TempDir())
	for _, up := range []Upload{
		{Name: "../escape", Include: []string{"a.txt"}},
		{Name: ".hidden", Include: []string{"a.txt"}},
		{Name: "a/b", Include: []string{"a.txt"}},
		{Name: "ok", Include: []string{"../outside"}},
		{Name: "ok", Include: []string{"/etc/passwd"}},
	} {
		if _, err := c.CollectNamed(t.Context(), "r_bad", ws, up); err == nil {
			t.Errorf("%+v was accepted", up)
		}
	}
}

func TestRestoreByNameAndAll(t *testing.T) {
	ws := workspace(t, map[string]string{"dist/app.bin": "bin", "logs/run.log": "log"})
	c := NewCollector(t.TempDir())
	for _, up := range []Upload{
		{Name: "app", Include: []string{"dist"}},
		{Name: "logs", Include: []string{"logs"}},
	} {
		if _, err := c.CollectNamed(t.Context(), "r_rt", ws, up); err != nil {
			t.Fatal(err)
		}
	}

	// `dist` roots the artifact at dist (A77), so app.bin comes back
	// straight into the destination, where GitHub's download puts it.
	dest := workspace(t, map[string]string{"keep.txt": "k"})
	n, err := c.Restore("r_rt", dest, "app", "restored")
	if err != nil || n != 1 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "restored", "app.bin")); string(b) != "bin" {
		t.Errorf("restored content = %q", b)
	}

	// No name: every artifact, each under its own name — what GitHub does.
	all := workspace(t, map[string]string{"keep.txt": "k"})
	if n, err := c.Restore("r_rt", all, "", ""); err != nil || n != 2 {
		t.Fatalf("Restore all = %d, %v", n, err)
	}
	for _, p := range []string{"app/app.bin", "logs/run.log"} {
		if _, err := os.Stat(filepath.Join(all, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s was not restored: %v", p, err)
		}
	}

	if _, err := c.Restore("r_rt", dest, "never-uploaded", ""); !errors.Is(err, ErrNoSuchArtifact) {
		t.Errorf("a missing artifact = %v, want ErrNoSuchArtifact", err)
	}
}

// Writes go through os.Root: a step that left a symlink where a directory is
// about to be created cannot redirect the restore out of the workspace.
func TestRestoreCannotBeRedirectedOutOfTheWorkspace(t *testing.T) {
	ws := workspace(t, map[string]string{"dist/app.bin": "bin"})
	c := NewCollector(t.TempDir())
	if _, err := c.CollectNamed(t.Context(), "r_sym", ws, Upload{Name: "app", Include: []string{"dist"}}); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	dest := workspace(t, map[string]string{"keep.txt": "k"})
	symlink(t, outside, filepath.Join(dest, "restored"))

	if _, err := c.Restore("r_sym", dest, "app", "restored"); err == nil {
		t.Error("the restore followed a symlink out of the workspace")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files were written outside the workspace: %v", entries)
	}

	if _, err := c.Restore("r_sym", dest, "app", "../escape"); err == nil {
		t.Error("a destination that climbs out was accepted")
	}
}

// The root an upload's files are stored under is upload-artifact's (A77), so
// that `latem artifacts` lists what GitHub's zip would hold and a download
// puts the files where GitHub's would.
func TestArtifactRootIsUploadArtifacts(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		files    []string
		want     string
	}{
		{"a directory", []string{"dist/**"}, []string{"dist/a.js", "dist/sub/b.js"}, "dist"},
		{"a pattern's literal prefix", []string{"export/*.ipa"}, []string{"export/App.ipa"}, "export"},
		{"a lone file, by name", []string{"out/App.dmg"}, []string{"out/App.dmg"}, "out"},
		{"a lone file at the top", []string{"App.dmg"}, []string{"App.dmg"}, ""},
		{"the deepest shared directory", []string{"a/b/*.txt", "a/c/**"}, []string{"a/b/x.txt", "a/c/y"}, "a"},
		{"nothing shared", []string{"dist/**", "reports/*.xml"}, []string{"dist/a", "reports/j.xml"}, ""},
		{"a search path inside another", []string{"dist/**", "dist/sub/*.so"}, []string{"dist/sub/l.so"}, "dist"},
		{"the same file twice", []string{"out/App.dmg", "out/App.dmg"}, []string{"out/App.dmg"}, "out"},
		{"everything", []string{"**", "dist/*.js"}, []string{"a", "dist/b.js"}, ""},
	}
	for _, c := range cases {
		if got := artifactRoot(c.patterns, c.files); got != c.want {
			t.Errorf("%s: root of %q = %q, want %q", c.name, c.patterns, got, c.want)
		}
	}
}

// archive: false (A77): the file's own name is the artifact's, and more than
// one file is refused.
func TestCollectNamedSingle(t *testing.T) {
	ws := workspace(t, map[string]string{"out/app-1.2.zip": "zip", "out/app-1.2.crx": "crx"})
	c := NewCollector(t.TempDir())
	refs, err := c.CollectNamed(t.Context(), "r_one", ws, Upload{Include: []string{"out/app-1.2.zip"}, Single: true})
	if err != nil || len(refs) != 1 || refs[0].Path != "app-1.2.zip/app-1.2.zip" {
		t.Fatalf("refs = %+v, err = %v", refs, err)
	}
	// A directory holding the one file still gives the file itself.
	deep := workspace(t, map[string]string{"pkg/sub/app.zip": "zip"})
	refs, err = c.CollectNamed(t.Context(), "r_deep", deep, Upload{Include: []string{"pkg"}, Single: true})
	if err != nil || len(refs) != 1 || refs[0].Path != "app.zip/app.zip" {
		t.Errorf("one file under a directory: refs = %+v, err = %v", refs, err)
	}
	if _, err := c.CollectNamed(t.Context(), "r_two", ws, Upload{Include: []string{"out/*"}, Single: true}); !errors.Is(err, ErrNotSingle) {
		t.Errorf("two files = %v, want ErrNotSingle", err)
	}
}
