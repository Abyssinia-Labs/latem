package docker

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// workspace builds a tree from a path→content map and returns its root.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func paths(files []executor.File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
		_ = f.Content.Close()
	}
	return out
}

func TestCollectArtifacts(t *testing.T) {
	tree := map[string]string{
		"app.aab":                  "aab",
		"README.md":                "readme",
		"dist/app.apk":             "apk",
		"dist/app.aab":             "aab",
		"dist/mapping.txt":         "mapping",
		"dist/nested/deep/out.apk": "deep",
		"build/reports/junit.xml":  "junit",
		"build/reports/a/b.xml":    "nested xml",
	}

	tests := map[string]struct {
		pattern string
		want    []string
	}{
		"exact file":            {"app.aab", []string{"app.aab"}},
		"star in one segment":   {"dist/*.apk", []string{"dist/app.apk"}},
		"star matches several":  {"dist/app.*", []string{"dist/app.aab", "dist/app.apk"}},
		"star does not descend": {"*.apk", nil},
		"doublestar descends": {"dist/**/*.apk",
			[]string{"dist/app.apk", "dist/nested/deep/out.apk"}},
		"trailing doublestar": {"build/**",
			[]string{"build/reports/a/b.xml", "build/reports/junit.xml"}},
		"leading doublestar": {"**/*.xml",
			[]string{"build/reports/a/b.xml", "build/reports/junit.xml"}},
		"leading dot slash":            {"./app.aab", []string{"app.aab"}},
		"redundant slashes":            {"dist//app.apk", []string{"dist/app.apk"}},
		"no match":                     {"nothing/here.zip", nil},
		"directory is not an artifact": {"dist", nil},
		"character class":              {"dist/app.a[ab]b", []string{"dist/app.aab"}},
	}

	root := workspace(t, tree)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := collectArtifacts(root, tc.pattern)
			if err != nil {
				t.Fatalf("collectArtifacts(%q): %v", tc.pattern, err)
			}
			// slices.Equal treats a nil want and an empty result as equal,
			// which is what "no match" means here.
			if p := paths(got); !slices.Equal(p, tc.want) {
				t.Errorf("collectArtifacts(%q) = %q, want %q", tc.pattern, p, tc.want)
			}
		})
	}
}

// §17: a glob is relative to the workspace and must not escape it. These are
// the escapes that are visible in the pattern text, so they cost no I/O to
// refuse.
func TestCollectArtifacts_RejectsTextualEscapes(t *testing.T) {
	root := workspace(t, map[string]string{"app.aab": "x"})

	tests := map[string]string{
		"absolute":             "/etc/passwd",
		"parent":               "../secrets",
		"parent in the middle": "dist/../../etc/passwd",
		"parent at the end":    "dist/..",
		"empty":                "",
		"whitespace only":      "   ",
		"nul byte":             "a\x00b",
	}

	for name, pattern := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := collectArtifacts(root, pattern)
			if err == nil {
				t.Fatalf("collectArtifacts(%q) was accepted", pattern)
			}
			if got := CodeOf(err); got != protocol.ErrArtifact {
				t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrArtifact, err)
			}
		})
	}
}

// The escape that actually happens: a build writes a convenience symlink, a
// glob picks it up, and the artifact archive quietly acquires the contents of
// somewhere else on the machine.
func TestCollectArtifacts_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := workspace(t, map[string]string{"dist/app.apk": "apk"})

	t.Run("symlinked file", func(t *testing.T) {
		link := filepath.Join(root, "dist", "leak.apk")
		if err := os.Symlink(filepath.Join(outside, "id_rsa"), link); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(link) }()

		if _, err := collectArtifacts(root, "dist/*.apk"); err == nil {
			t.Fatal("a symlink out of the workspace was collected")
		} else if got := CodeOf(err); got != protocol.ErrArtifact {
			t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrArtifact, err)
		}
	})

	t.Run("symlinked directory", func(t *testing.T) {
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(link) }()

		if _, err := collectArtifacts(root, "escape/*"); err == nil {
			t.Fatal("a glob through a symlinked directory left the workspace")
		} else if got := CodeOf(err); got != protocol.ErrArtifact {
			t.Errorf("error code = %q, want %q (%v)", got, protocol.ErrArtifact, err)
		}
	})

	t.Run("symlinked directory under doublestar", func(t *testing.T) {
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(link) }()

		if _, err := collectArtifacts(root, "**/*"); err == nil {
			t.Fatal("a ** walk followed a symlink out of the workspace")
		}
	})
}

// A symlink that stays inside the workspace is ordinary and must still work;
// rejecting it would break every project that links its build output.
func TestCollectArtifacts_AllowsSymlinkInsideWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := workspace(t, map[string]string{"build/app.apk": "apk"})
	if err := os.Symlink(filepath.Join(root, "build"), filepath.Join(root, "dist")); err != nil {
		t.Fatal(err)
	}

	got, err := collectArtifacts(root, "dist/*.apk")
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if p := paths(got); len(p) != 1 || p[0] != "dist/app.apk" {
		t.Errorf("got %q, want [dist/app.apk]", p)
	}
}

// A symlink loop inside the workspace is legal and must not hang collection.
func TestCollectArtifacts_SurvivesSymlinkLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := workspace(t, map[string]string{"a/b/file.txt": "x"})
	if err := os.Symlink(root, filepath.Join(root, "a", "loop")); err != nil {
		t.Fatal(err)
	}

	got, err := collectArtifacts(root, "**/file.txt")
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(got) == 0 {
		t.Error("the loop swallowed the real match")
	}
	for _, f := range got {
		_ = f.Content.Close()
	}
}

func TestCollectArtifacts_FileMetadataAndContent(t *testing.T) {
	root := workspace(t, map[string]string{"dist/app.apk": "hello"})

	files, err := collectArtifacts(root, "dist/app.apk")
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	f := files[0]
	defer func() { _ = f.Content.Close() }()

	if f.Path != "dist/app.apk" {
		t.Errorf("Path = %q, want a path relative to the workspace root", f.Path)
	}
	if f.SizeBytes != 5 {
		t.Errorf("SizeBytes = %d, want 5", f.SizeBytes)
	}
	if f.Mode == 0 {
		t.Error("Mode is unset")
	}
	body, err := io.ReadAll(f.Content)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("content = %q, want %q", body, "hello")
	}
}

// A glob matching thousands of files must not consume a descriptor per match
// before the caller has read any of them.
func TestLazyFile_OpensOnFirstReadOnly(t *testing.T) {
	root := workspace(t, map[string]string{"a.txt": "content"})
	full := filepath.Join(root, "a.txt")

	l := &lazyFile{path: full}
	if l.f != nil {
		t.Fatal("the file was opened before any read")
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close before read: %v", err)
	}

	l = &lazyFile{path: full}
	body, err := io.ReadAll(l)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "content" {
		t.Errorf("content = %q", body)
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Double close is what a caller doing `defer f.Content.Close()` in a loop
	// with an early error path will do.
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestLazyFile_MissingFileReportsOnce(t *testing.T) {
	l := &lazyFile{path: filepath.Join(t.TempDir(), "gone")}
	if _, err := l.Read(make([]byte, 1)); err == nil {
		t.Fatal("read of a missing file succeeded")
	}
	if _, err := l.Read(make([]byte, 1)); err == nil {
		t.Fatal("second read of a missing file succeeded")
	}
}

func TestWithin(t *testing.T) {
	tests := map[string]struct {
		root, p string
		want    bool
	}{
		"same":             {"/ws", "/ws", true},
		"child":            {"/ws", "/ws/a", true},
		"grandchild":       {"/ws", "/ws/a/b", true},
		"parent":           {"/ws", "/", false},
		"sibling":          {"/ws", "/other", false},
		"prefix confusion": {"/ws", "/workspace-other", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := within(tc.root, tc.p); got != tc.want {
				t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.p, got, tc.want)
			}
		})
	}
}

func TestFetch_UsesTheWorkspace(t *testing.T) {
	root := workspace(t, map[string]string{"dist/app.apk": "x"})
	f := &fakeAPI{envFileMissing: true}
	env := prepared(t, f, executor.Spec{RunID: "run-1", Workspace: root})

	files, err := env.Fetch(t.Context(), "dist/*.apk")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if p := paths(files); len(p) != 1 || p[0] != "dist/app.apk" {
		t.Errorf("Fetch = %q", p)
	}
}
