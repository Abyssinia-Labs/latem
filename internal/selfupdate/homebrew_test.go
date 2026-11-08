package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// brewPrefix lays out a Homebrew prefix in a temporary directory: a keg for
// each version, the opt link at the first, and the bin link beside it.
func brewPrefix(t *testing.T, versions ...string) string {
	t.Helper()
	// Resolved, as /opt/homebrew already is: macOS's temporary directory is
	// under /var, which is a link to /private/var.
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		bin := filepath.Join(prefix, "Cellar", "latem", v, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "latem"), []byte("latem "+v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"opt", "bin"} {
		if err := os.MkdirAll(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Relative, as brew writes them.
	if err := os.Symlink(filepath.Join("..", "Cellar", "latem", versions[0]), filepath.Join(prefix, "opt", "latem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "Cellar", "latem", versions[0], "bin", "latem"), filepath.Join(prefix, "bin", "latem")); err != nil {
		t.Fatal(err)
	}
	return prefix
}

func TestHomebrew_FindsTheOptLinkFromAnyPathIntoTheKeg(t *testing.T) {
	prefix := brewPrefix(t, "0.3.1")
	want := filepath.Join(prefix, "opt", "latem", "bin", "latem")
	for name, exe := range map[string]string{
		"the keg":      filepath.Join(prefix, "Cellar", "latem", "0.3.1", "bin", "latem"),
		"the bin link": filepath.Join(prefix, "bin", "latem"),
		"the opt link": want,
	} {
		t.Run(name, func(t *testing.T) {
			link, ok := Homebrew(exe)
			if !ok {
				t.Fatalf("Homebrew(%s) = not Homebrew's", exe)
			}
			if link != want {
				t.Errorf("Homebrew(%s) = %s, want %s", exe, link, want)
			}
		})
	}
}

// `brew upgrade` deletes the keg the agent is running from. Linux still names
// the process's executable by that path, and it is still Homebrew's.
func TestHomebrew_AKegUpgradeDeletedIsStillHomebrews(t *testing.T) {
	prefix := brewPrefix(t, "0.3.2", "0.3.1")
	old := filepath.Join(prefix, "Cellar", "latem", "0.3.1")
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	link, ok := Homebrew(filepath.Join(old, "bin", "latem"))
	if !ok || link != filepath.Join(prefix, "opt", "latem", "bin", "latem") {
		t.Errorf("Homebrew(deleted keg) = %q, %v; want the opt link", link, ok)
	}
}

func TestHomebrew_NotHomebrews(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, ".local", "bin", "latem")
	if err := os.MkdirAll(filepath.Dir(plain), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	// A keg-shaped path with no opt link: somebody's copy of a Cellar, or a
	// prefix being uninstalled. Nothing would keep a service's path working.
	noLink := filepath.Join(dir, "Cellar", "latem", "0.3.1", "bin", "latem")
	if err := os.MkdirAll(filepath.Dir(noLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noLink, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	prefix := brewPrefix(t, "0.3.1")
	for name, exe := range map[string]string{
		"~/.local/bin":           plain,
		"no opt link":            noLink,
		"another formula":        filepath.Join(prefix, "Cellar", "other", "1.0", "bin", "latem"),
		"another binary":         filepath.Join(prefix, "Cellar", "latem", "0.3.1", "bin", "other"),
		"not under bin":          filepath.Join(prefix, "Cellar", "latem", "0.3.1", "latem"),
		"a go test binary":       filepath.Join(dir, "go-build123", "b001", "selfupdate.test"),
		"a relative path":        "latem",
		"the filesystem root":    "/",
		"the empty string":       "",
		"a Cellar of its own":    filepath.Join(dir, "Cellar", "latem"),
		"one directory too deep": filepath.Join(prefix, "Cellar", "latem", "0.3.1", "x", "bin", "latem"),
	} {
		t.Run(name, func(t *testing.T) {
			if link, ok := Homebrew(exe); ok {
				t.Errorf("Homebrew(%s) = %s, want not Homebrew's", exe, link)
			}
		})
	}
}
