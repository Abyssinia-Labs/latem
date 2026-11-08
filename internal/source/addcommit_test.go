package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localCommit makes a commit in a checkout of the fixture's upstream that no
// ref there reaches, the shape `latem try` makes of a dirty tree (A76).
func (f *fixture) localCommit() (checkout, sha string) {
	f.t.Helper()
	checkout = filepath.Join(f.root, "checkout")
	runGit(f.t, f.root, "clone", "-q", "--", f.up.dir, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "new.txt"), []byte("not committed\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	index := filepath.Join(f.root, "try-index")
	runGit(f.t, checkout, "read-tree", "HEAD", "--index-output="+index)
	runGitEnv(f.t, checkout, []string{"GIT_INDEX_FILE=" + index}, "add", "-A")
	tree := runGitEnv(f.t, checkout, []string{"GIT_INDEX_FILE=" + index}, "write-tree")
	sha = runGit(f.t, checkout, "commit-tree", tree, "-p", "HEAD", "-m", "latem try")
	return checkout, sha
}

func runGitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), hermeticGitEnv()...), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A commit on no ref reaches the mirror as objects alone: the mirror is made
// for it when the agent has none, no ref or FETCH_HEAD is written, a workspace
// checks it out, and the mirror's next fetch, which prunes, keeps it.
func TestAddCommit_ObjectsOnlyIntoAMirrorItMakes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	checkout, sha := f.localCommit()

	mirror, err := f.mgr.AddCommit(ctx, f.repo, checkout, sha)
	if err != nil {
		t.Fatalf("AddCommit: %v", err)
	}
	if got, want := runGit(t, mirror.Path(), "for-each-ref", "--format=%(refname)"),
		runGit(t, f.up.dir, "for-each-ref", "--format=%(refname)"); got != want {
		t.Fatalf("the mirror's refs are not the upstream's:\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(mirror.Path(), "FETCH_HEAD")); err == nil {
		t.Error("adding a commit wrote FETCH_HEAD, which the agent's own fetch writes too")
	}
	if has, err := mirror.HasCommit(ctx, sha); err != nil || !has {
		t.Fatalf("HasCommit = %v, %v", has, err)
	}
	ws, err := mirror.Checkout(ctx, "run-try", "refs/heads/main", sha)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws.Path, "new.txt")); err != nil || string(b) != "not committed\n" {
		t.Fatalf("the workspace does not have the untracked file: %q, %v", b, err)
	}

	f.up.commit("README.md", "moved on\n", "upstream moves")
	if _, err := f.mgr.Fetch(ctx, f.repo); err != nil {
		t.Fatal(err)
	}
	if has, _ := mirror.HasCommit(ctx, sha); !has {
		t.Error("the mirror's fetch --prune lost the commit")
	}
	if _, err := f.mgr.AddCommit(ctx, f.repo, checkout, sha); err != nil {
		t.Errorf("adding a commit the mirror has: %v", err)
	}
}

// What AddCommit refuses: a reader, a checkout that is not an absolute path,
// and a name that is not a commit id.
func TestAddCommit_Refusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	checkout, sha := f.localCommit()

	reader, err := NewReader(Config{MirrorDir: filepath.Join(f.root, "mirrors"), Env: hermeticGitEnv()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.AddCommit(ctx, f.repo, checkout, sha); !errors.Is(err, errReader) {
		t.Errorf("a reader added a commit: %v", err)
	}
	if _, err := f.mgr.AddCommit(ctx, f.repo, "checkout", sha); err == nil {
		t.Error("a relative checkout was accepted")
	}
	if _, err := f.mgr.AddCommit(ctx, f.repo, checkout, "--upload-pack=touch /tmp/x"); err == nil {
		t.Error("a name that is not a commit id was accepted")
	}
	if _, err := os.Stat(filepath.Join(f.root, "mirrors", "myapp.git")); err == nil {
		t.Error("a refused AddCommit made a mirror")
	}
}

// Two processes can both be first to a repository — the agent and latem try —
// and the one whose rename loses keeps the other's mirror instead of failing.
func TestCreateMirror_LosingTheRenameKeepsTheWinner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mirror := f.fetch()
	head := runGit(t, mirror.Path(), "rev-parse", "refs/heads/main")

	if err := f.mgr.createMirror(ctx, f.repo, mirror.Path()); err != nil {
		t.Fatalf("a second creation failed instead of keeping the first: %v", err)
	}
	if got := runGit(t, mirror.Path(), "rev-parse", "refs/heads/main"); got != head {
		t.Errorf("the mirror changed under the loser: %s, was %s", got, head)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, "mirrors")); len(entries) != 1 {
		t.Errorf("the loser left its staging directory behind: %v", entries)
	}
}
