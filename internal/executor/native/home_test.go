package native

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

// Amendment A38. A committed jobs.yaml runs on every machine (§12), so a cache
// target cannot be written absolutely — "/Users/someone/Library/..." is correct
// on exactly one host. `~` is the portable form, which is why §9's own example
// uses it, and why every reference registry shipped a cache path no executor
// would accept until the first agent-run job failed on it in under a second.
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, in, want string
	}{
		{"bare tilde", "~", home},
		{"the case that broke", "~/Library/Developer/Xcode/DerivedData",
			filepath.Join(home, "Library/Developer/Xcode/DerivedData")},
		{"absolute is untouched", "/home/runner/.gradle", "/home/runner/.gradle"},
		{"relative is untouched", "relative/stays", "relative/stays"},

		// A tilde that is not leading is an ordinary character. Expanding it
		// would corrupt a legitimate path rather than fix anything.
		{"embedded tilde", "/opt/~/x", "/opt/~/x"},

		// ~user is deliberately NOT supported. A step already runs as one
		// specific user, so naming another produces a directory that user
		// cannot write, and the failure surfaces as a permission error on a
		// cache mount rather than as anything mentioning ~.
		{"other user", "~otheruser/x", "~otheruser/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandHome(tc.in)
			if err != nil {
				t.Fatalf("expandHome(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The expansion must reach the code that MOUNTS the cache, through Prepare.
//
// This test has now been wrong twice, in the same way, and the reason is worth
// keeping. Version one called expandHome a second time and declared the wiring
// proven — asserting the helper twice. Version two called expandCacheTargets
// and linkCaches itself, in the right order, and passed while Prepare still
// handed linkCaches the UNEXPANDED slice: it tested the test's composition, not
// the production one, and a mutation to Prepare could not fail it.
//
// The bug both versions missed: filepath.Clean leaves "~/Library/..." relative,
// so MkdirAll created a literal "~" directory under the agent's working
// directory and symlinked into it. Every run logged "mounted 1 cache(s)"; no
// cache was mounted; the real directory was never touched. It reached a
// developer's checkout on disk before anything noticed.
//
// So: call Prepare, and assert on the filesystem.
func TestPrepareExpandsTildeCacheTarget(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	source := filepath.Join(root, "caches", "runner", "derived-data")
	ws := filepath.Join(root, "ws")
	for _, d := range []string{home, source, ws, filepath.Join(root, "env")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)

	// A working directory that would collect a stray "~", as the agent has.
	cwd := filepath.Join(root, "cwd")
	if err := os.MkdirAll(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	e, err := New(Options{EnvFileDir: filepath.Join(root, "env")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env, err := e.Prepare(t.Context(), executor.Spec{
		RunID:     "r_0000000000000000000000ff",
		Workspace: ws,
		Caches: []executor.CacheMount{{
			Name:   "derived-data",
			Source: source,
			Target: "~/Library/Developer/Xcode/DerivedData",
		}},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(t.Context()) })

	want := filepath.Join(home, "Library/Developer/Xcode/DerivedData")
	dest, err := os.Readlink(want)
	if err != nil {
		t.Fatalf("Prepare did not link the cache under HOME: no symlink at %s: %v", want, err)
	}
	if dest != source {
		t.Errorf("symlink points at %q, want %q", dest, source)
	}
	if _, err := os.Stat(filepath.Join(cwd, "~")); err == nil {
		t.Error(`Prepare created a literal "~" directory in the working directory`)
	}
}
