package native

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		"leading dot slash": {"./app.aab", []string{"app.aab"}},
		"redundant slashes": {"dist//app.apk", []string{"dist/app.apk"}},
		"no match":          {"nothing/here.zip", nil},
		// Identical to the docker executor's answer, deliberately: a job's
		// artifacts: block must mean the same thing on every runner.
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
			if !slices.Equal(paths(got), tc.want) {
				t.Errorf("collectArtifacts(%q) = %v, want %v", tc.pattern, paths(got), tc.want)
			}
		})
	}
}

// A pattern that leaves the workspace by its own text costs a string
// comparison, before any I/O at all.
func TestCollectArtifacts_TextualEscapesAreRejected(t *testing.T) {
	root := workspace(t, map[string]string{"a.txt": "a"})

	for _, pattern := range []string{
		"/etc/passwd",
		"../secrets",
		"../../etc/passwd",
		"dist/../../outside",
		"",
		"   ",
		"a\x00b",
	} {
		t.Run(pattern, func(t *testing.T) {
			_, err := collectArtifacts(root, pattern)
			if err == nil {
				t.Fatalf("collectArtifacts(%q) succeeded; a glob that escapes must be E_ARTIFACT", pattern)
			}
			if got := CodeOf(err); got != protocol.ErrArtifact {
				t.Errorf("code = %q, want %q", got, protocol.ErrArtifact)
			}
		})
	}
}

// The escape that actually happens. A build tool writes a convenience symlink,
// a glob picks it up, and the artifact archive quietly acquires the contents of
// somebody's home directory. On this executor there is no container boundary
// underneath the check, so it is the only boundary there is.
func TestCollectArtifacts_SymlinkEscapesAreRejected(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "id_rsa"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("symlinked file", func(t *testing.T) {
		root := workspace(t, map[string]string{"dist/keep.txt": "ok"})
		if err := os.Symlink(filepath.Join(outside, "id_rsa"), filepath.Join(root, "dist", "leak.txt")); err != nil {
			t.Fatal(err)
		}
		_, err := collectArtifacts(root, "dist/*.txt")
		if err == nil {
			t.Fatal("a symlink out of the workspace was collected")
		}
		if got := CodeOf(err); got != protocol.ErrArtifact {
			t.Errorf("code = %q, want %q", got, protocol.ErrArtifact)
		}
	})

	t.Run("symlinked directory walked through", func(t *testing.T) {
		root := workspace(t, map[string]string{"dist/keep.txt": "ok"})
		if err := os.Symlink(outside, filepath.Join(root, "dist", "escape")); err != nil {
			t.Fatal(err)
		}
		_, err := collectArtifacts(root, "dist/**/*")
		if err == nil {
			t.Fatal("a symlinked directory pointing out of the workspace was walked")
		}
		if got := CodeOf(err); got != protocol.ErrArtifact {
			t.Errorf("code = %q, want %q", got, protocol.ErrArtifact)
		}
	})
}

// A symlink that stays inside is ordinary: build systems make them constantly,
// and refusing them would break more jobs than it protects.
func TestCollectArtifacts_SymlinkInsideIsFine(t *testing.T) {
	root := workspace(t, map[string]string{"build/out.apk": "apk"})
	if err := os.Symlink(filepath.Join(root, "build"), filepath.Join(root, "dist")); err != nil {
		t.Fatal(err)
	}
	got, err := collectArtifacts(root, "dist/*.apk")
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want one match through the in-workspace symlink", paths(got))
	}
	_ = got[0].Content.Close()
}

// A cycle would otherwise turn `**` into an infinite descent.
func TestCollectArtifacts_SymlinkCycleTerminates(t *testing.T) {
	root := workspace(t, map[string]string{"a/b/c.txt": "x"})
	if err := os.Symlink(root, filepath.Join(root, "a", "loop")); err != nil {
		t.Fatal(err)
	}
	got, err := collectArtifacts(root, "**/*.txt")
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(got) == 0 {
		t.Error("the real file was not found")
	}
	for _, f := range got {
		_ = f.Content.Close()
	}
}

func TestCollectArtifacts_ContentAndMetadata(t *testing.T) {
	root := workspace(t, map[string]string{"dist/app.apk": "hello"})
	got, err := collectArtifacts(root, "dist/app.apk")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d files, want 1", len(got))
	}
	f := got[0]
	defer func() { _ = f.Content.Close() }()

	if f.Path != "dist/app.apk" {
		t.Errorf("Path = %q, want dist/app.apk", f.Path)
	}
	if f.SizeBytes != 5 {
		t.Errorf("SizeBytes = %d, want 5", f.SizeBytes)
	}
	body, err := io.ReadAll(f.Content)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Errorf("content = %q, want hello", body)
	}
}

// Closing without reading must not fail: a caller that decides an artifact is
// too large to archive still has to release it.
func TestLazyFile_CloseWithoutRead(t *testing.T) {
	l := &lazyFile{path: filepath.Join(t.TempDir(), "never-opened")}
	if err := l.Close(); err != nil {
		t.Errorf("Close on an unopened lazyFile: %v", err)
	}
}

func TestLazyFile_ReadErrorIsSticky(t *testing.T) {
	l := &lazyFile{path: filepath.Join(t.TempDir(), "missing")}
	if _, err := l.Read(make([]byte, 1)); err == nil {
		t.Fatal("reading a missing file succeeded")
	}
	if _, err := l.Read(make([]byte, 1)); err == nil {
		t.Fatal("the second read did not report the remembered error")
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close after a failed open: %v", err)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join("/a", "b")
	cases := map[string]bool{
		filepath.Join("/a", "b"):        true,
		filepath.Join("/a", "b", "c"):   true,
		filepath.Join("/a", "bc"):       false,
		"/a":                            false,
		filepath.Join("/a", "b", ".."):  false,
		filepath.Join("/etc", "passwd"): false,
	}
	for p, want := range cases {
		if got := within(root, p); got != want {
			t.Errorf("within(%q, %q) = %v, want %v", root, p, got, want)
		}
	}
}

func TestGlobSegments_MalformedPattern(t *testing.T) {
	_, err := globSegments("dist/[unclosed")
	if err == nil {
		t.Fatal("a malformed pattern was accepted")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error = %q, want it to say the glob is malformed", err)
	}
}
