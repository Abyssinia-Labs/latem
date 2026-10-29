package source

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A plan reads a commit's workflows out of the mirror, without a checkout,
// and only the directory's regular files (A52).
func TestReadDir_ReadsOneDirectoryOfACommit(t *testing.T) {
	f := newFixture(t)
	f.up.commit(".github/workflows/checks.yml", "  # leading space kept\non: push\n", "checks")
	f.up.commit(".github/workflows/nested/inner.yml", "on: push\n", "nested")
	f.up.commit(".github/workflows/big.yml", strings.Repeat("x", 300), "big")
	// A symlink in the directory pointing elsewhere in the repository.
	if err := os.Symlink("../../README.md", filepath.Join(f.up.dir, ".github/workflows/link.yml")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.up.dir, "add", "--", ".github/workflows/link.yml")
	runGit(t, f.up.dir, "commit", "-q", "-m", "link")
	sha := runGit(t, f.up.dir, "rev-parse", "HEAD")
	f.up.commit(".github/workflows/later.yml", "on: push\n", "after the planned commit")

	mirror := f.fetch()
	ctx := context.Background()
	fsys, err := mirror.ReadDir(ctx, sha, ".github/workflows", 10, 200)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(fsys, ".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"big.yml", "checks.yml"}) {
		t.Errorf("entries %v: want the regular files at that commit, and no symlink, subdirectory or later file", names)
	}
	data, err := fs.ReadFile(fsys, ".github/workflows/checks.yml")
	if err != nil || string(data) != "  # leading space kept\non: push\n" {
		t.Errorf("checks.yml = %q, %v", data, err)
	}
	if _, err := fs.ReadFile(fsys, ".github/workflows/big.yml"); err == nil || !strings.Contains(err.Error(), "larger than 200 bytes") {
		t.Errorf("an oversized file read as %v", err)
	}

	if _, err := mirror.ReadDir(ctx, sha, ".github/workflows", 1, 200); err == nil {
		t.Error("more files than the cap was not refused")
	}
	if _, err := mirror.ReadDir(ctx, strings.Repeat("e", 40), ".github/workflows", 10, 200); err == nil {
		t.Error("a commit the mirror does not have was not refused")
	}
}

func TestChanged(t *testing.T) {
	f := newFixture(t)
	root := runGit(t, f.up.dir, "rev-parse", "HEAD")
	a := f.up.commit("apps/web/a.ts", "a\n", "a")
	b := f.up.commit("docs/b.md", "b\n", "b")
	mirror := f.fetch()
	ctx := context.Background()

	for _, c := range []struct {
		name     string
		from, to string
		want     []string
	}{
		{"a push", root, b, []string{"apps/web/a.ts", "docs/b.md"}},
		{"a new branch: against the first parent", "", b, []string{"docs/b.md"}},
		{"a before the mirror never had", strings.Repeat("e", 40), a, []string{"apps/web/a.ts"}},
		{"a root commit: everything", "", root, []string{"README.md"}},
	} {
		got, err := mirror.Changed(ctx, c.from, c.to)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
