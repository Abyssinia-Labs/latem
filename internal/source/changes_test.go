package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *fixture) change(c Changes) Changed {
	f.t.Helper()
	got, err := f.mgr.CommitChanges(context.Background(), f.repo, c)
	if err != nil {
		f.t.Fatalf("CommitChanges: %v", err)
	}
	return got
}

func mirrorOf(f *fixture) string { return filepath.Join(f.root, "mirrors", f.repo.Key+".git") }

// The commit a changes run builds is the base with the files written and the
// removes removed, as the base's child, under no ref.
func TestCommitChanges(t *testing.T) {
	f := newFixture(t)
	f.up.commit("keep.txt", "kept\n", "keep")
	f.up.commit("edit.txt", "before\n", "edit")
	f.up.commit("gone.txt", "going\n", "gone")
	write(t, filepath.Join(f.up.dir, "run.sh"), "#!/bin/sh\necho v1\n")
	if err := os.Chmod(filepath.Join(f.up.dir, "run.sh"), 0o755); err != nil { // #nosec G302
		t.Fatal(err)
	}
	runGit(t, f.up.dir, "add", "--", "run.sh")
	runGit(t, f.up.dir, "commit", "-q", "-m", "a script")
	base := runGit(t, f.up.dir, "rev-parse", "HEAD")
	f.fetch()
	mirror := mirrorOf(f)
	refsBefore := runGit(t, mirror, "for-each-ref")

	c := Changes{
		Base: base,
		Writes: []FileChange{
			{Path: "edit.txt", Data: []byte("after\n")},
			{Path: "src/new file.txt", Data: []byte("new\n")},
			{Path: "run.sh", Data: []byte("#!/bin/sh\necho v2\n")},
			{Path: "bin/tool", Data: []byte{0, 1, 2, 0xff}, Mode: "100755"},
			{Path: "notes/*.md", Data: []byte("a star in a name\n")},
		},
		Removes: []string{"gone.txt"},
	}
	got := f.change(c)

	if got.SHA == base || got.Paths != 6 {
		t.Fatalf("CommitChanges = %+v, want a new commit and 6 paths changed", got)
	}
	if parent := runGit(t, mirror, "rev-parse", got.SHA+"^"); parent != base {
		t.Errorf("the commit's parent is %s, want the base %s", parent, base)
	}
	tree := runGit(t, mirror, "ls-tree", "-r", got.SHA)
	for _, want := range []string{
		"100644 blob", "\tkeep.txt", "\tedit.txt", "\tsrc/new file.txt", "\tREADME.md",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("the tree lacks %q:\n%s", want, tree)
		}
	}
	if strings.Contains(tree, "gone.txt") {
		t.Errorf("a removed file is still in the tree:\n%s", tree)
	}
	for _, line := range strings.Split(tree, "\n") {
		// A script keeps the mode the base has it at; a new file is 644
		// unless it says otherwise.
		switch {
		case strings.HasSuffix(line, "\trun.sh"), strings.HasSuffix(line, "\tbin/tool"):
			if !strings.HasPrefix(line, "100755 ") {
				t.Errorf("want an executable: %s", line)
			}
		case strings.HasSuffix(line, "\tsrc/new file.txt"):
			if !strings.HasPrefix(line, "100644 ") {
				t.Errorf("want a plain file: %s", line)
			}
		}
	}
	if body := runGit(t, mirror, "show", got.SHA+":edit.txt"); body != "after" {
		t.Errorf("edit.txt holds %q", body)
	}
	if body := runGit(t, mirror, "show", got.SHA+":notes/*.md"); body != "a star in a name" {
		t.Errorf("the file with a star in its name holds %q", body)
	}
	if after := runGit(t, mirror, "for-each-ref"); after != refsBefore {
		t.Errorf("the mirror's refs changed:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}

	// The same base and changes are the same commit.
	if again := f.change(c); again.SHA != got.SHA {
		t.Errorf("the same changes made %s, then %s", got.SHA, again.SHA)
	}

	// A run of it checks out like any commit.
	ws, err := f.fetch().Checkout(context.Background(), run1, "refs/heads/main", got.SHA)
	if err != nil {
		t.Fatalf("checking out the changes: %v", err)
	}
	if got := read(t, filepath.Join(ws.Path, "edit.txt")); got != "after\n" {
		t.Errorf("the workspace's edit.txt holds %q", got)
	}
	if exists(filepath.Join(ws.Path, "gone.txt")) {
		t.Error("the workspace still has the removed file")
	}
}

// Changes that change nothing are the base, and no commit is made.
func TestCommitChangesThatChangeNothing(t *testing.T) {
	f := newFixture(t)
	base := f.up.commit("a.txt", "same\n", "a")
	f.fetch()
	got := f.change(Changes{Base: base, Writes: []FileChange{{Path: "a.txt", Data: []byte("same\n")}}})
	if got.SHA != base || got.Paths != 0 {
		t.Fatalf("CommitChanges = %+v, want the base itself", got)
	}
}

func TestCommitChangesRefuses(t *testing.T) {
	f := newFixture(t)
	f.up.commit("dir/inner.txt", "inner\n", "dir")
	if err := os.Symlink("README.md", filepath.Join(f.up.dir, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.up.dir, "add", "--", "link")
	runGit(t, f.up.dir, "commit", "-q", "-m", "a link")
	base := runGit(t, f.up.dir, "rev-parse", "HEAD")
	f.fetch()

	for name, tc := range map[string]struct {
		c    Changes
		want string
	}{
		"a remove the base does not have": {
			Changes{Base: base, Removes: []string{"never.txt"}}, "not a file the base"},
		"a remove of a directory": {
			Changes{Base: base, Removes: []string{"dir"}}, "not a file the base"},
		"a write to a symbolic link": {
			Changes{Base: base, Writes: []FileChange{{Path: "link", Data: []byte("x")}}}, "symbolic link"},
		"a mode that is not a file's": {
			Changes{Base: base, Writes: []FileChange{{Path: "a", Data: []byte("x"), Mode: "120000"}}}, "100644 or 100755"},
		"a path into .git": {
			Changes{Base: base, Writes: []FileChange{{Path: ".git/config", Data: []byte("x")}}}, "inside a .git"},
		"a path into a .git spelt another way": {
			Changes{Base: base, Writes: []FileChange{{Path: "sub/.GIT./hooks/pre-commit", Data: []byte("x")}}}, "inside a .git"},
		"a path that climbs out": {
			Changes{Base: base, Writes: []FileChange{{Path: "a/../../b", Data: []byte("x")}}}, "has a .. part"},
		"an absolute path": {
			Changes{Base: base, Removes: []string{"/etc/passwd"}}, "is absolute"},
		"a path with a newline": {
			Changes{Base: base, Writes: []FileChange{{Path: "a\nb", Data: []byte("x")}}}, "control character"},
		"a file where the base has a directory": {
			Changes{Base: base, Writes: []FileChange{{Path: "dir", Data: []byte("x")}}}, "a directory in the base"},
		"a file under a file": {
			Changes{Base: base, Writes: []FileChange{{Path: "README.md/x", Data: []byte("x")}}}, "is a file in the base"},
		"a file and a file under it": {
			Changes{Base: base, Writes: []FileChange{{Path: "n", Data: []byte("x")}, {Path: "n/x", Data: []byte("x")}}}, "a file under it"},
		"a path written and removed": {
			Changes{Base: base, Writes: []FileChange{{Path: "README.md", Data: []byte("x")}}, Removes: []string{"README.md"}}, "both write and remove"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.mgr.CommitChanges(context.Background(), f.repo, tc.c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CommitChanges = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// A file can become a directory in one set of changes: the file is removed
// and files are written under its name.
func TestCommitChangesTurnsAFileIntoADirectory(t *testing.T) {
	f := newFixture(t)
	base := f.up.commit("thing", "a file\n", "thing")
	f.fetch()
	got := f.change(Changes{Base: base, Removes: []string{"thing"},
		Writes: []FileChange{{Path: "thing/inner.txt", Data: []byte("now a directory\n")}}})
	if body := runGit(t, mirrorOf(f), "show", got.SHA+":thing/inner.txt"); body != "now a directory" {
		t.Errorf("thing/inner.txt holds %q", body)
	}
	if out := runGit(t, mirrorOf(f), "fsck", "--no-dangling", got.SHA); strings.Contains(out, "error") {
		t.Errorf("git fsck does not like the tree:\n%s", out)
	}
}

// A base the mirror has not fetched yet is fetched by its name, under no ref;
// one nobody has is said to be missing.
func TestCommitChangesFetchesABaseTheMirrorLacks(t *testing.T) {
	f := newFixture(t)
	f.fetch()
	mirror := mirrorOf(f)
	refsBefore := runGit(t, mirror, "for-each-ref")
	base := f.up.commit("later.txt", "pushed after the last fetch\n", "later")

	got := f.change(Changes{Base: base, Writes: []FileChange{{Path: "later.txt", Data: []byte("changed\n")}}})
	if parent := runGit(t, mirror, "rev-parse", got.SHA+"^"); parent != base {
		t.Errorf("the commit's parent is %s, want %s", parent, base)
	}
	if after := runGit(t, mirror, "for-each-ref"); after != refsBefore {
		t.Errorf("fetching the base moved the mirror's refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}

	_, err := f.mgr.CommitChanges(context.Background(), f.repo, Changes{
		Base:   "00000000000000000000000000000000000000ff",
		Writes: []FileChange{{Path: "a", Data: []byte("x")}},
	})
	if !errors.Is(err, ErrBaseMissing) {
		t.Fatalf("a base nobody has: %v, want ErrBaseMissing", err)
	}
}

// A machine that has never built the repository gets its mirror first.
func TestCommitChangesMakesTheMirror(t *testing.T) {
	f := newFixture(t)
	base := f.up.commit("a.txt", "a\n", "a")
	got := f.change(Changes{Base: base, Writes: []FileChange{{Path: "a.txt", Data: []byte("b\n")}}})
	if body := runGit(t, mirrorOf(f), "show", got.SHA+":a.txt"); body != "b" {
		t.Errorf("a.txt holds %q", body)
	}
}
