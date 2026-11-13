package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// warmFixture is a [fixture] whose manager keeps warm workspaces.
func newWarmFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	up := newUpstream(t, filepath.Join(root, "upstream"))
	mgr, err := New(Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		WarmDir:   filepath.Join(root, "warm"),
		Env:       hermeticGitEnv(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{t: t, mgr: mgr, up: up, repo: Repo{Key: "myapp", URL: up.dir}, root: root}
}

var warmKey = WarmKey{Runner: "linux", Job: "test"}

func (f *fixture) warm(runID, sha string) (*Workspace, WarmReport) {
	f.t.Helper()
	ws, report, err := f.fetch().WarmCheckout(context.Background(), warmKey, runID, "refs/heads/main", sha)
	if err != nil {
		f.t.Fatalf("WarmCheckout(%s, %s): %v", runID, sha, err)
	}
	return ws, report
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

const (
	run1 = "r_000000000000000000000001"
	run2 = "r_000000000000000000000002"
	run3 = "r_000000000000000000000003"
)

// The whole of A90 in one run after another: what the repository ignores is
// kept, a tracked file is the commit's whatever a step did to it, a file that
// did not change is not written, and what is neither tracked nor ignored goes.
func TestWarmCheckoutKeepsWhatIsIgnoredAndWritesOnlyWhatChanged(t *testing.T) {
	f := newWarmFixture(t)
	f.up.commit(".gitignore", "build/\n", "ignore build")
	f.up.commit("same.txt", "same\n", "same")
	f.up.commit("tampered.txt", "as committed\n", "tampered")
	f.up.commit("changes.txt", "one\n", "changes")
	sha1 := f.up.commit("leaves.txt", "going\n", "leaves")

	ws, report := f.warm(run1, sha1)
	if !report.Fresh || !ws.Warm() {
		t.Fatalf("the first run: Fresh=%v Warm=%v, want both", report.Fresh, ws.Warm())
	}
	if got := runGit(t, ws.Path, "rev-parse", "HEAD"); got != sha1 {
		t.Fatalf("HEAD is %s, want %s", got, sha1)
	}

	// What a build leaves: output the repository ignores, a stray file it
	// does not, and an edit to a tracked file.
	write(t, filepath.Join(ws.Path, "build", "out.o"), "object code")
	write(t, filepath.Join(ws.Path, "stray.txt"), "left behind")
	write(t, filepath.Join(ws.Path, "tampered.txt"), "edited by a step\n")
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(ws.Path, "same.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := ws.Remove(); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	if !exists(filepath.Join(ws.Path, "build", "out.o")) {
		t.Fatal("releasing a warm workspace removed its files")
	}

	runGit(t, f.up.dir, "rm", "-q", "leaves.txt")
	sha2 := f.up.commit("changes.txt", "two\n", "second")

	ws2, report := f.warm(run2, sha2)
	if ws2.Path != ws.Path {
		t.Fatalf("the second run built in %s, not the first's %s", ws2.Path, ws.Path)
	}
	if report.Fresh || report.Rebuilt != "" {
		t.Fatalf("the second run did not reuse the workspace: %+v", report)
	}
	if report.PrevSHA != sha1 || report.PrevRun != run1 {
		t.Errorf("report names the last use as %s by %s, want %s by %s", report.PrevSHA, report.PrevRun, sha1, run1)
	}
	if got := read(t, filepath.Join(ws2.Path, "build", "out.o")); got != "object code" {
		t.Errorf("the ignored build output is %q, want it kept", got)
	}
	if exists(filepath.Join(ws2.Path, "stray.txt")) {
		t.Error("an untracked file the repository does not ignore survived")
	}
	if exists(filepath.Join(ws2.Path, "leaves.txt")) {
		t.Error("a file the new commit does not track survived")
	}
	if got := read(t, filepath.Join(ws2.Path, "tampered.txt")); got != "as committed\n" {
		t.Errorf("a tracked file a step edited is %q, want the commit's", got)
	}
	if got := read(t, filepath.Join(ws2.Path, "changes.txt")); got != "two\n" {
		t.Errorf("a file the commit changed is %q", got)
	}
	info, err := os.Stat(filepath.Join(ws2.Path, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("an unchanged file was written again: mtime %s, want %s", info.ModTime(), old)
	}
	// tampered.txt and changes.txt were written; stray.txt and leaves.txt removed.
	if report.Written != 2 || report.Removed != 2 {
		t.Errorf("report says %d written, %d removed; want 2 and 2", report.Written, report.Removed)
	}
	if got := runGit(t, ws2.Path, "rev-parse", "HEAD"); got != sha2 {
		t.Errorf("HEAD is %s, want %s", got, sha2)
	}
	if got := runGit(t, ws2.Path, "status", "--porcelain"); got != "" {
		t.Errorf("git status is not clean:\n%s", got)
	}
	if got := runGit(t, ws2.Path, "remote", "get-url", "origin"); got != f.up.dir {
		t.Errorf("origin is %s, want the repository's URL %s", got, f.up.dir)
	}
	_ = ws2.Remove()
}

// A step that edits a tracked file and puts its size and timestamps back has
// not hidden the edit: the comparison is by content.
func TestWarmCheckoutComparesContentNotTimestamps(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("main.go", "package main // v1\n", "main")
	ws, _ := f.warm(run1, sha)
	path := filepath.Join(ws.Path, "main.go")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "package main // v2\n") // the same length
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	_ = ws.Remove()

	ws, report := f.warm(run2, sha)
	if got := read(t, path); got != "package main // v1\n" {
		t.Errorf("the edited file is %q, want the commit's", got)
	}
	if report.Written != 1 {
		t.Errorf("report says %d written, want 1", report.Written)
	}
	_ = ws.Remove()
}

// The .git a run leaves is never read: a hook or an fsmonitor planted in it
// would otherwise run as the agent at the next run's checkout.
func TestWarmCheckoutNeverReadsTheLastRunsGitDir(t *testing.T) {
	f := newWarmFixture(t)
	sha1 := f.up.commit("a.txt", "a\n", "a")
	ws, _ := f.warm(run1, sha1)

	marker := filepath.Join(f.root, "hook-ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, hook := range []string{"post-checkout", "post-index-change", "reference-transaction"} {
		p := filepath.Join(ws.Path, ".git", "hooks", hook)
		write(t, p, script)
		if err := os.Chmod(p, 0o700); err != nil { // #nosec G302 -- a hook must be executable
			t.Fatal(err)
		}
	}
	fsmonitor := filepath.Join(f.root, "fsmonitor.sh")
	write(t, fsmonitor, script)
	if err := os.Chmod(fsmonitor, 0o700); err != nil { // #nosec G302
		t.Fatal(err)
	}
	runGit(t, ws.Path, "config", "core.fsmonitor", fsmonitor)
	runGit(t, ws.Path, "config", "core.hooksPath", filepath.Join(ws.Path, ".git", "hooks"))
	_ = ws.Remove()

	sha2 := f.up.commit("a.txt", "b\n", "b")
	ws, _ = f.warm(run2, sha2)
	defer func() { _ = ws.Remove() }()
	if exists(marker) {
		t.Fatal("a hook or fsmonitor the last run planted in .git ran at this run's checkout")
	}
	if gitStatus(t, ws.Path, "config", "--get", "core.fsmonitor") == 0 {
		t.Error("the last run's .git/config survived into this run")
	}
	if exists(filepath.Join(ws.Path, ".git", "hooks", "post-checkout")) {
		t.Error("the last run's hook survived into this run")
	}
}

// A .git that is not a directory any more — a step replaced it — is replaced
// like any other.
func TestWarmCheckoutReplacesAGitDirAStepReplaced(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	ws, _ := f.warm(run1, sha)
	outside := filepath.Join(f.root, "elsewhere")
	write(t, filepath.Join(outside, "keep"), "not the workspace's")
	if err := os.RemoveAll(filepath.Join(ws.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	_ = ws.Remove()

	ws, report := f.warm(run2, sha)
	defer func() { _ = ws.Remove() }()
	if report.Fresh {
		t.Error("the workspace was not reused")
	}
	if !exists(filepath.Join(outside, "keep")) {
		t.Fatal("replacing a .git that was a symlink removed what it pointed at")
	}
	if got := runGit(t, ws.Path, "rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD is %s, want %s", got, sha)
	}
}

// Runs of one job take turns, and one that cannot wait any longer says what it
// was waiting for.
func TestWarmCheckoutTakesTurns(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	mirror := f.fetch()
	ws, _ := f.warm(run1, sha)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := mirror.WarmCheckout(ctx, warmKey, run2, "refs/heads/main", sha)
	if err == nil || !strings.Contains(err.Error(), "another run holds") {
		t.Fatalf("a second run got the workspace while the first held it: %v", err)
	}

	// Another job's workspace is another workspace.
	other, _, err := mirror.WarmCheckout(context.Background(), WarmKey{Runner: "linux", Job: "lint"}, run3, "refs/heads/main", sha)
	if err != nil {
		t.Fatalf("another job's warm workspace: %v", err)
	}
	if other.Path == ws.Path {
		t.Fatal("two jobs share one warm workspace")
	}
	_ = other.Remove()

	done := make(chan error, 1)
	go func() {
		w, _, err := mirror.WarmCheckout(context.Background(), warmKey, run2, "refs/heads/main", sha)
		if err == nil {
			err = w.Remove()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the second run did not wait for the first: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := ws.Remove(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second run, once the first released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second run never got its turn")
	}
}

// A workspace git cannot bring to the commit is thrown away and made again,
// and the report says why, so a cold build is not a mystery.
func TestWarmCheckoutRebuildsAWorkspaceItCannotUse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory, so nothing here is unusable")
	}
	f := newWarmFixture(t)
	f.up.commit(".gitignore", "build/\n", "ignore")
	sha := f.up.commit("dir/a.txt", "a\n", "a")
	ws, _ := f.warm(run1, sha)
	write(t, filepath.Join(ws.Path, "build", "out.o"), "object code")
	// A tracked file gone from a directory nothing may write to.
	if err := os.Remove(filepath.Join(ws.Path, "dir", "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws.Path, "dir"), 0o500); err != nil { // #nosec G302
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ws.Path, "dir"), 0o700) }) // #nosec G302
	_ = ws.Remove()

	ws, report := f.warm(run2, sha)
	defer func() { _ = ws.Remove() }()
	if report.Rebuilt == "" {
		t.Fatalf("the workspace was not rebuilt: %+v", report)
	}
	if got := read(t, filepath.Join(ws.Path, "dir", "a.txt")); got != "a\n" {
		t.Errorf("after the rebuild a.txt is %q", got)
	}
	if exists(filepath.Join(ws.Path, "build", "out.o")) {
		t.Error("a rebuilt workspace kept the old one's build output")
	}
}

func TestWarmUnsupported(t *testing.T) {
	f := newWarmFixture(t)
	plain := f.up.commit("a.txt", "a\n", "a")
	lfs := f.up.commit(".gitattributes", "# media\n*.png filter=lfs diff=lfs merge=lfs -text\n", "lfs")
	runGit(t, f.up.dir, "rm", "-q", ".gitattributes")
	modules := f.up.commit(".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = ../x\n", "modules")
	mirror := f.fetch()

	for _, tc := range []struct{ sha, want string }{
		{plain, ""},
		{lfs, "git-lfs"},
		{modules, "submodules"},
	} {
		got, err := mirror.WarmUnsupported(context.Background(), tc.sha)
		if err != nil {
			t.Fatalf("WarmUnsupported(%s): %v", tc.sha, err)
		}
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("WarmUnsupported(%s) = %q, want it to mention %q", tc.sha, got, tc.want)
		}
	}
}

func TestWarmCheckoutWithoutAWarmDir(t *testing.T) {
	f := newFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	_, _, err := f.fetch().WarmCheckout(context.Background(), warmKey, run1, "refs/heads/main", sha)
	if !errors.Is(err, ErrWarmUnavailable) {
		t.Fatalf("a manager with no warm dir answered %v, want ErrWarmUnavailable", err)
	}
}

func TestWarmCheckoutRefusesNamesThatAreNotDirectories(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	for _, key := range []WarmKey{{Runner: "..", Job: "test"}, {Runner: "linux", Job: "a/b"}, {Runner: "linux", Job: ""}} {
		if _, _, err := f.fetch().WarmCheckout(context.Background(), key, run1, "refs/heads/main", sha); err == nil {
			t.Errorf("WarmCheckout took %+v as a directory name", key)
		}
	}
}

// The daily sweep removes what nobody used, and never what a run holds.
func TestListAndPruneWarm(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	mirror := f.fetch()

	idle, _, err := mirror.WarmCheckout(context.Background(), WarmKey{Runner: "linux", Job: "idle"}, run1, "refs/heads/main", sha)
	if err != nil {
		t.Fatal(err)
	}
	_ = idle.Remove()
	busy, _, err := mirror.WarmCheckout(context.Background(), WarmKey{Runner: "linux", Job: "busy"}, run2, "refs/heads/main", sha)
	if err != nil {
		t.Fatal(err)
	}

	all, err := f.mgr.ListWarm()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListWarm found %d workspaces, want 2: %+v", len(all), all)
	}
	for _, w := range all {
		if w.Repo != "myapp" || w.SHA != sha || w.InUse != (w.Job == "busy") {
			t.Errorf("unexpected listing %+v", w)
		}
	}

	// Nothing is older than an hour ago.
	gone, err := f.mgr.PruneWarm(time.Now().Add(-time.Hour))
	if err != nil || len(gone) != 0 {
		t.Fatalf("PruneWarm removed %+v (%v), want nothing", gone, err)
	}
	// Everything is older than an hour from now, and one is held.
	gone, err = f.mgr.PruneWarm(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Job != "idle" {
		t.Fatalf("PruneWarm removed %+v, want only the idle workspace", gone)
	}
	if exists(filepath.Dir(idle.Path)) {
		t.Error("the idle workspace is still on disk")
	}
	if !exists(busy.Path) {
		t.Fatal("PruneWarm removed a workspace a run holds")
	}
	_ = busy.Remove()

	// A zero cutoff is everything not in use.
	gone, err = f.mgr.PruneWarm(time.Time{})
	if err != nil || len(gone) != 1 {
		t.Fatalf("PruneWarm with no cutoff removed %+v (%v), want the one left", gone, err)
	}
	if exists(filepath.Join(f.root, "warm", "myapp")) {
		t.Error("an empty repository directory was left under warm/")
	}
}

// `latem cache prune` is another process with its own manager. The lock on
// the workspace is what tells it a run in the agent is building there.
func TestPruneWarmFromAnotherProcessLeavesWhatARunHolds(t *testing.T) {
	f := newWarmFixture(t)
	sha := f.up.commit("a.txt", "a\n", "a")
	ws, _ := f.warm(run1, sha)

	other, err := New(Config{
		MirrorDir: filepath.Join(f.root, "mirrors"),
		WorkDir:   filepath.Join(f.root, "work"),
		WarmDir:   filepath.Join(f.root, "warm"),
		Env:       hermeticGitEnv(),
	})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := other.ListWarm()
	if err != nil || len(listed) != 1 || !listed[0].InUse {
		t.Fatalf("the other process lists %+v (%v), want one workspace, in use", listed, err)
	}
	gone, err := other.PruneWarm(time.Time{})
	if err != nil || len(gone) != 0 {
		t.Fatalf("the other process removed %+v (%v) while a run held it", gone, err)
	}
	if !exists(filepath.Join(ws.Path, "a.txt")) {
		t.Fatal("the held workspace's files are gone")
	}

	_ = ws.Remove()
	gone, err = other.PruneWarm(time.Time{})
	if err != nil || len(gone) != 1 {
		t.Fatalf("once released, the other process removed %+v (%v), want the one", gone, err)
	}
}
