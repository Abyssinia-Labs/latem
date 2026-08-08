package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// workspace builds a tree from a map of relative path to contents and returns
// the workspace root. Parent directories are created as needed.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		write(t, filepath.Join(root, rel), body)
	}
	return root
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func rels(matches []Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Rel)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A pattern that cannot stay inside the workspace is refused before any
// filesystem access. These are the cheap half of the boundary: a hostile
// registry costs a string comparison.
func TestResolve_RejectsEscapingPatterns(t *testing.T) {
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})

	patterns := []struct {
		name    string
		pattern string
	}{
		{"parent traversal", "../secrets"},
		{"traversal mid-path", "build/../../etc/passwd"},
		{"traversal after a glob", "*/../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"absolute under home", "/Users/someone/.ssh/id_rsa"},
		{"tilde expansion", "~/.ssh/id_rsa"},
		{"tilde alone", "~"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"workspace root itself", "."},
		{"root by another spelling", "./"},
		{"NUL byte", "build/app\x00.ipa"},
	}

	for _, tc := range patterns {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(ws, tc.pattern)
			if err == nil {
				t.Fatalf("Resolve(%q) returned %v, want an error", tc.pattern, rels(got))
			}
			if Code(err) != protocol.ErrArtifact {
				t.Errorf("Code(%v) = %q, want %s", err, Code(err), protocol.ErrArtifact)
			}
		})
	}
}

// The expensive half of the boundary: the pattern is legal but the filesystem
// leads outside anyway. Every one of these must fail the call rather than
// quietly return fewer files, because a skipped escape looks exactly like a
// build that produced no artifacts.
func TestResolve_RejectsSymlinksLeavingTheWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		build   func(t *testing.T, root, outside string)
	}{
		{
			name:    "symlink to an absolute path outside",
			pattern: "build/*.txt",
			build: func(t *testing.T, root, _ string) {
				symlink(t, "/etc/passwd", filepath.Join(root, "build", "leak.txt"))
			},
		},
		{
			name:    "symlink to the parent directory",
			pattern: "escape",
			build: func(t *testing.T, root, outside string) {
				symlink(t, outside, filepath.Join(root, "escape"))
			},
		},
		{
			name:    "glob reaching through an escaping directory symlink",
			pattern: "escape/*.txt",
			build: func(t *testing.T, root, outside string) {
				symlink(t, outside, filepath.Join(root, "escape"))
			},
		},
		{
			name:    "relative symlink climbing out",
			pattern: "build/*.txt",
			build: func(t *testing.T, root, _ string) {
				symlink(t, "../../outside/loot.txt", filepath.Join(root, "build", "loot.txt"))
			},
		},
		{
			name:    "chain of symlinks ending outside",
			pattern: "build/*.txt",
			build: func(t *testing.T, root, outside string) {
				symlink(t, filepath.Join(outside, "loot.txt"), filepath.Join(root, "hop.txt"))
				symlink(t, "../hop.txt", filepath.Join(root, "build", "leak.txt"))
			},
		},
		{
			name:    "escape inside a matched directory",
			pattern: "build",
			build: func(t *testing.T, root, outside string) {
				symlink(t, filepath.Join(outside, "loot.txt"), filepath.Join(root, "build", "leak.txt"))
			},
		},
		{
			name:    "escape below a double-star match",
			pattern: "**/*.txt",
			build: func(t *testing.T, root, outside string) {
				symlink(t, filepath.Join(outside, "loot.txt"), filepath.Join(root, "deep", "nest", "leak.txt"))
			},
		},
		{
			name:    "dangling symlink whose target is outside",
			pattern: "build/*.txt",
			build: func(t *testing.T, root, outside string) {
				symlink(t, filepath.Join(outside, "never-existed.txt"), filepath.Join(root, "build", "leak.txt"))
			},
		},
		{
			// A sibling directory sharing the workspace's name as a prefix.
			// This is the case a strings.HasPrefix containment check gets
			// wrong, and it is why within() uses filepath.Rel.
			name:    "symlink into a sibling with a shared name prefix",
			pattern: "build/*.txt",
			build: func(t *testing.T, root, _ string) {
				sibling := root + "-evil"
				write(t, filepath.Join(sibling, "loot.txt"), "secret")
				symlink(t, filepath.Join(sibling, "loot.txt"), filepath.Join(root, "build", "leak.txt"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "ws")
			outside := filepath.Join(base, "outside")
			write(t, filepath.Join(root, "build", "app.txt"), "real artifact")
			write(t, filepath.Join(outside, "loot.txt"), "secret")
			tc.build(t, root, outside)

			got, err := Resolve(root, tc.pattern)
			if err == nil {
				t.Fatalf("Resolve(%q) returned %v, want a refusal", tc.pattern, rels(got))
			}
			if Code(err) != protocol.ErrArtifact {
				t.Errorf("Code(%v) = %q, want %s", err, Code(err), protocol.ErrArtifact)
			}
			if !strings.Contains(err.Error(), "outside the workspace") {
				t.Errorf("error does not say what went wrong: %v", err)
			}
		})
	}
}

// Symlinks staying inside the workspace are ordinary. A build that links its
// output directory somewhere else in the tree has done nothing wrong, and
// refusing those would make the boundary useless in practice.
func TestResolve_AllowsSymlinksInsideTheWorkspace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "real", "app.ipa"), "payload")
	symlink(t, filepath.Join(root, "real", "app.ipa"), filepath.Join(root, "build", "app.ipa"))

	got, err := Resolve(root, "build/*.ipa")
	if err != nil {
		t.Fatalf("a symlink inside the workspace was refused: %v", err)
	}
	if !equal(rels(got), []string{"build/app.ipa"}) {
		t.Fatalf("rels = %v", rels(got))
	}
	// Path must be the resolved target: the collector opens it, and resolving
	// once is what removes the window between the check and the open.
	if filepath.Base(filepath.Dir(got[0].Path)) != "real" {
		t.Errorf("Path = %q, want the resolved target under real/", got[0].Path)
	}
}

func TestResolve_WalksContainedDirectorySymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "outputs", "app.aab"), "bundle")
	symlink(t, filepath.Join(root, "outputs"), filepath.Join(root, "build"))

	got, err := Resolve(root, "build/*.aab")
	if err != nil {
		t.Fatal(err)
	}
	if !equal(rels(got), []string{"build/app.aab"}) {
		t.Errorf("rels = %v, want the file reached through the contained link", rels(got))
	}
}

// A symlink loop inside the workspace must terminate rather than recurse until
// the stack runs out. `ln -s . self` is one command and a build script can do
// it by accident.
func TestResolve_TerminatesOnSymlinkCycles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "app.ipa"), "payload")
	symlink(t, root, filepath.Join(root, "self"))

	got, err := Resolve(root, "**/*.ipa")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no matches; the walk should still find the real file")
	}
}

// A dangling link pointing inside the workspace is a build that deleted its
// own intermediate. There is nothing to collect and nothing to complain about.
func TestResolve_IgnoresDanglingLinksPointingInside(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "build", "app.ipa"), "payload")
	symlink(t, filepath.Join(root, "build", "gone.ipa"), filepath.Join(root, "build", "stale.ipa"))

	got, err := Resolve(root, "build/*.ipa")
	if err != nil {
		t.Fatalf("a dangling link inside the workspace should be ignored: %v", err)
	}
	if !equal(rels(got), []string{"build/app.ipa"}) {
		t.Errorf("rels = %v", rels(got))
	}
}

// An escaping symlink the glob cannot reach is not this glob's problem.
// Refusing on it would fail every job in a repository that has a node_modules
// link pointing at a global store.
func TestResolve_IgnoresEscapesOutsideTheGlobsReach(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	write(t, filepath.Join(root, "build", "app.ipa"), "payload")
	symlink(t, "/etc/passwd", filepath.Join(root, "node_modules", "leak"))

	got, err := Resolve(root, "build/*.ipa")
	if err != nil {
		t.Fatalf("an escape the glob never reaches must not fail the run: %v", err)
	}
	if !equal(rels(got), []string{"build/app.ipa"}) {
		t.Errorf("rels = %v", rels(got))
	}
}

func TestResolve_GlobSemantics(t *testing.T) {
	ws := workspace(t, map[string]string{
		"app.ipa":                        "a",
		"build/app.ipa":                  "b",
		"build/debug/app.ipa":            "c",
		"build/outputs/bundle/app.aab":   "d",
		"build/reports/index.html":       "e",
		"coverage/lcov.info":             "f",
		"node_modules/pkg/dist/index.js": "g",
	})

	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			name:    "a single star stays within one segment",
			pattern: "build/*.ipa",
			want:    []string{"build/app.ipa"},
		},
		{
			// Zero segments counts, as it does in gitignore and in the ref
			// policy globs of §5: build/**/*.ipa covers build/app.ipa as well
			// as build/debug/app.ipa. A job author writing ** means "at any
			// depth", and "at any depth except this one" would surprise them.
			name:    "a double star spans zero or more segments",
			pattern: "build/**/*.ipa",
			want:    []string{"build/app.ipa", "build/debug/app.ipa"},
		},
		{
			name:    "a leading double star reaches the root too",
			pattern: "**/*.aab",
			want:    []string{"build/outputs/bundle/app.aab"},
		},
		{
			name:    "a bare filename matches at the root only",
			pattern: "app.ipa",
			want:    []string{"app.ipa"},
		},
		{
			name:    "a directory expands to everything beneath it",
			pattern: "build/outputs",
			want:    []string{"build/outputs/bundle/app.aab"},
		},
		{
			name:    "a trailing slash names the same directory",
			pattern: "coverage/",
			want:    []string{"coverage/lcov.info"},
		},
		{
			name:    "a star matches a directory name",
			pattern: "*/lcov.info",
			want:    []string{"coverage/lcov.info"},
		},
		{
			name:    "no match is not an error",
			pattern: "build/*.dSYM",
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(ws, tc.pattern)
			if err != nil {
				t.Fatal(err)
			}
			if !equal(rels(got), tc.want) {
				t.Errorf("Resolve(%q) = %v, want %v", tc.pattern, rels(got), tc.want)
			}
		})
	}
}

func TestResolve_ReportsSizeAndSortsOutput(t *testing.T) {
	ws := workspace(t, map[string]string{
		"build/c.txt": "ccc",
		"build/a.txt": "a",
		"build/b.txt": "bb",
	})

	got, err := Resolve(ws, "build/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !equal(rels(got), []string{"build/a.txt", "build/b.txt", "build/c.txt"}) {
		t.Fatalf("output is not sorted: %v", rels(got))
	}
	if got[2].Size != 3 {
		t.Errorf("Size = %d, want 3", got[2].Size)
	}
}

// A workspace with a 200k-file node_modules is normal, and collection runs
// inside the run's wall clock. An unreadable directory the pattern cannot
// reach proves the walk never opened it.
func TestResolve_DoesNotDescendWhereNothingCanMatch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0 does not stop a read")
	}
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "build", "app.ipa"), "payload")

	forbidden := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(forbidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(forbidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(forbidden, 0o755) })

	if _, err := Resolve(root, "build/*.ipa"); err != nil {
		t.Fatalf("the walk descended into a directory the pattern cannot reach: %v", err)
	}
}

// Sockets, fifos and device nodes are not artifacts. Copying one would block
// forever or produce nonsense, and no build output is legitimately one.
func TestResolve_SkipsNonRegularFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	write(t, filepath.Join(root, "build", "app.ipa"), "payload")
	fifo := filepath.Join(root, "build", "pipe.ipa")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("cannot create a fifo here: %v", err)
	}

	got, err := Resolve(root, "build/*.ipa")
	if err != nil {
		t.Fatal(err)
	}
	if !equal(rels(got), []string{"build/app.ipa"}) {
		t.Errorf("rels = %v, want the fifo skipped", rels(got))
	}
}

func TestResolve_RejectsAMissingWorkspace(t *testing.T) {
	if _, err := Resolve(filepath.Join(t.TempDir(), "nope"), "build/*"); err == nil {
		t.Fatal("a missing workspace should be an error, not an empty result")
	}
}

func TestResolve_RejectsAWorkspaceThatIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	write(t, path, "not a directory")
	if _, err := Resolve(path, "*"); err == nil {
		t.Fatal("a workspace that is a file should be an error")
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		root, path string
		want       bool
	}{
		{"/work/ws", "/work/ws", true},
		{"/work/ws", "/work/ws/build/app.ipa", true},
		{"/work/ws", "/work/ws-evil/loot", false},
		{"/work/ws", "/work", false},
		{"/work/ws", "/etc/passwd", false},
		{"/work/ws", "/work/ws/../ws2", false},
	}
	for _, tc := range tests {
		if got := within(tc.root, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}
