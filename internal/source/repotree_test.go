package source

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// treeFixture is an upstream with a few kinds of entry, and the agent's
// mirror of it.
func treeFixture(t *testing.T) (*fixture, *Mirror) {
	t.Helper()
	f := newFixture(t)
	f.up.commit(".github/workflows/ci.yml", "on: push\n", "ci")
	f.up.commit("ios/scripts/build.sh", "xcodebuild -scheme App\n", "script")
	if err := os.Symlink("../README.md", filepath.Join(f.up.dir, "ios/link.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.up.dir, "add", "--", "ios/link.md")
	runGit(t, f.up.dir, "commit", "-q", "-m", "link")
	f.fetch()

	reader, err := NewReader(Config{MirrorDir: f.mgr.MirrorDir(), Env: hermeticGitEnv()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := reader.Open(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return f, m
}

// A whole commit, without a checkout: regular files and the directories they
// imply, and no symlink (A57).
func TestTree_ReadsACommitWithoutACheckout(t *testing.T) {
	f, m := treeFixture(t)
	fsys, sha, err := m.Tree(context.Background(), "", DefaultTreeLimits)
	if err != nil {
		t.Fatal(err)
	}
	if want := runGit(t, f.up.dir, "rev-parse", "HEAD"); sha != want {
		t.Errorf("sha = %s, want the default branch's %s", sha, want)
	}
	if err := fstest.TestFS(fsys, "README.md", ".github/workflows/ci.yml", "ios/scripts/build.sh"); err != nil {
		t.Fatal(err)
	}
	var files []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if want := []string{".github/workflows/ci.yml", "README.md", "ios/scripts/build.sh"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v and no symlink", files, want)
	}
	data, err := fs.ReadFile(fsys, "ios/scripts/build.sh")
	if err != nil || string(data) != "xcodebuild -scheme App\n" {
		t.Errorf("build.sh = %q, %v", data, err)
	}
}

// A reader is a command typed beside a running agent: it reads, and the
// mirror is byte for byte what it was.
func TestReader_NeverWritesTheMirror(t *testing.T) {
	f, m := treeFixture(t)
	before := snapshotDir(t, f.mgr.MirrorDir())

	fsys, _, err := m.Tree(context.Background(), "refs/heads/main", DefaultTreeLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(fsys, ".github/workflows/ci.yml"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mgr.Fetch(context.Background(), f.repo); !errors.Is(err, errReader) {
		t.Errorf("a reader fetched: %v", err)
	}
	if _, err := m.Checkout(context.Background(), "r_1", "refs/heads/main", strings.Repeat("a", 40)); !errors.Is(err, errReader) {
		t.Errorf("a reader checked out: %v", err)
	}
	if after := snapshotDir(t, f.mgr.MirrorDir()); !maps2Equal(before, after) {
		t.Error("reading changed the mirror directory")
	}
}

func TestOpen_SaysWhenThereIsNoMirrorYet(t *testing.T) {
	reader, err := NewReader(Config{MirrorDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Open(Repo{Key: "app", URL: "https://github.com/acme/app.git"}); !errors.Is(err, ErrNoMirror) {
		t.Errorf("err = %v, want ErrNoMirror", err)
	}
}

func TestTree_Limits(t *testing.T) {
	_, m := treeFixture(t)
	ctx := context.Background()
	if _, _, err := m.Tree(ctx, "HEAD", TreeLimits{MaxFiles: 2, MaxFileBytes: 1 << 10}); err == nil {
		t.Error("a commit over the file cap was not refused")
	}
	fsys, _, err := m.Tree(ctx, "HEAD", TreeLimits{MaxFiles: 10, MaxFileBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(fsys, "ios/scripts/build.sh"); err == nil || !strings.Contains(err.Error(), "larger than 10 bytes") {
		t.Errorf("an oversized file read as %v", err)
	}
	for _, rev := range []string{"no-such-branch", "--output=/tmp/x", "main\nHEAD"} {
		if _, _, err := m.Tree(ctx, rev, DefaultTreeLimits); err == nil {
			t.Errorf("revision %q was accepted", rev)
		}
	}
}

// Before the agent has a mirror: a blobless, one-commit clone whose files
// arrive as they are read.
func TestNewSnapshot_ReadsWhatItIsAskedFor(t *testing.T) {
	f := newFixture(t)
	f.up.commit(".github/workflows/ci.yml", "on: push\n", "ci")
	runGit(t, f.up.dir, "config", "uploadpack.allowFilter", "true")
	repo := Repo{Key: "myapp", URL: "file://" + f.up.dir}

	m, err := NewSnapshot(context.Background(), Config{Env: hermeticGitEnv()}, repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, m.Path(), "config", "remote.origin.promisor"); got != "true" {
		t.Errorf("remote.origin.promisor = %q: not a partial clone", got)
	}
	fsys, _, err := m.Tree(context.Background(), "", DefaultTreeLimits)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(fsys, ".github/workflows/ci.yml"); err != nil || string(data) != "on: push\n" {
		t.Errorf("ci.yml = %q, %v", data, err)
	}
	if _, err := m.mgr.Fetch(context.Background(), repo); !errors.Is(err, errReader) {
		t.Errorf("a snapshot fetched: %v", err)
	}
}

// snapshotDir records every path under dir with its size and modification
// time.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = info.ModTime().String() + "/" + info.Mode().String() + "/" + string(rune(info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func maps2Equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The cap on git's output holds however it is written to. Embedding
// bytes.Buffer once promoted its ReadFrom, which io.Copy — and so os/exec —
// prefers to Write: nothing was ever refused.
func TestCappedBuffer_HoldsThroughCopy(t *testing.T) {
	c := &cappedBuffer{limit: 10}
	if _, err := io.Copy(c, strings.NewReader(strings.Repeat("x", 23))); err == nil || !c.over {
		t.Fatalf("23 bytes copied into a 10-byte cap: err=%v over=%v", err, c.over)
	}
	if c.Len() > 10 {
		t.Errorf("held %d bytes past a 10-byte cap", c.Len())
	}
	if _, ok := any(c).(io.ReaderFrom); ok {
		t.Error("cappedBuffer is an io.ReaderFrom again, and io.Copy will go around Write")
	}
}
