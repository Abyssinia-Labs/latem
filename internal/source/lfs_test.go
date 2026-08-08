package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// git-lfs is not installed on most machines that run this suite, and the LFS
// path is two commands in two different repositories with an ordering
// requirement between them. These tests put a shim in front of git that
// answers `git lfs …` and forwards everything else to the real binary, so the
// ordering and the conditionality are testable without a git-lfs dependency.
//
// What is deliberately NOT tested here is git-lfs itself: whether `lfs fetch
// --all` into a bare mirror followed by `lfs checkout` in a hard-linked clone
// actually materialises content. That needs git-lfs and a real LFS-backed
// repository, and belongs in test/integration (WS-16) rather than here.

// lfsShim writes an executable that intercepts `git lfs`.
//
// It records "<dir>\t<args>" per interception, so a test can assert both which
// commands ran and which repository each ran in — the whole point of the LFS
// sequence being that the fetch happens in the mirror and the checkout in the
// workspace.
func lfsShim(t *testing.T, exitCode int) (shim, logPath string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}

	dir := t.TempDir()
	shim = filepath.Join(dir, "git-shim")
	logPath = filepath.Join(dir, "lfs.log")

	script := `#!/bin/sh
dir=""
if [ "$1" = "-C" ]; then dir="$2"; shift 2; fi
if [ "$1" = "lfs" ]; then
  printf '%s\t%s\n' "$dir" "$*" >> "$LATEM_TEST_LFS_LOG"
  exit ` + strconv.Itoa(exitCode) + `
fi
if [ -n "$dir" ]; then exec "$LATEM_TEST_REAL_GIT" -C "$dir" "$@"; fi
exec "$LATEM_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil { // #nosec G306 -- test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("LATEM_TEST_REAL_GIT", realGit)
	t.Setenv("LATEM_TEST_LFS_LOG", logPath)
	return shim, logPath
}

// lfsFixture is newFixture with the shim installed as the git binary.
func lfsFixture(t *testing.T, exitCode int) (*fixture, string) {
	t.Helper()
	shim, logPath := lfsShim(t, exitCode)

	root := t.TempDir()
	up := newUpstream(t, filepath.Join(root, "upstream"))
	mgr, err := New(Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		GitBinary: shim,
		Env:       hermeticGitEnv(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{t: t, mgr: mgr, up: up, repo: Repo{Key: "myapp", URL: up.dir}, root: root}, logPath
}

func readLFSLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(data)) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestLFS_NotInvokedWithoutAttributes. `git lfs fetch --all` is a no-op on a
// repo that does not use LFS but is potentially tens of gigabytes on one that
// does; running it unconditionally makes every run on every repo pay for a
// feature two of them use.
func TestLFS_NotInvokedWithoutAttributes(t *testing.T) {
	f, logPath := lfsFixture(t, 0)
	f.acquire("run_nolfs", "refs/heads/main")

	if lines := readLFSLog(t, logPath); len(lines) != 0 {
		t.Errorf("git lfs ran on a repo with no LFS attributes: %v", lines)
	}
}

// TestLFS_NotInvokedForAnUnrelatedGitattributes guards the predicate from the
// other side: a .gitattributes exists, but nothing in it routes through LFS.
func TestLFS_NotInvokedForAnUnrelatedGitattributes(t *testing.T) {
	f, logPath := lfsFixture(t, 0)
	f.up.commit(".gitattributes", "*.go text eol=lf\n# filter=lfs was removed in 2024\n", "text attributes")
	f.acquire("run_textattrs", "refs/heads/main")

	if lines := readLFSLog(t, logPath); len(lines) != 0 {
		t.Errorf("git lfs ran for a .gitattributes with no LFS filter: %v", lines)
	}
}

// TestLFS_FetchesInTheMirrorAndChecksOutInTheWorkspace is §12's sequence. The
// order is not cosmetic: the checkout can only find objects the mirror fetch
// has already brought down.
func TestLFS_FetchesInTheMirrorAndChecksOutInTheWorkspace(t *testing.T) {
	f, logPath := lfsFixture(t, 0)
	f.up.commit(".gitattributes", "*.psd filter=lfs diff=lfs merge=lfs -text\n", "use lfs")

	ws := f.acquire("run_lfs", "refs/heads/main")

	lines := readLFSLog(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("git lfs ran %d times, want 2 (fetch then checkout): %v", len(lines), lines)
	}

	mirror := filepath.Join(f.mgr.MirrorDir(), "myapp.git")
	if want := mirror + "\tlfs fetch --all"; lines[0] != want {
		t.Errorf("first lfs command = %q, want %q", lines[0], want)
	}
	if want := ws.Path + "\tlfs checkout"; lines[1] != want {
		t.Errorf("second lfs command = %q, want %q", lines[1], want)
	}
}

// TestLFS_FailureFailsTheRun. A repo that needs LFS on a machine without
// git-lfs must fail here, not produce a workspace full of 130-byte pointer
// files and a build that dies much later with an unrecognisable error.
func TestLFS_FailureFailsTheRun(t *testing.T) {
	f, _ := lfsFixture(t, 1)
	f.up.commit(".gitattributes", "*.psd filter=lfs -text\n", "use lfs")

	mirror, err := f.mgr.Fetch(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Acquire(context.Background(), "run_lfsfail", "refs/heads/main"); err == nil {
		t.Fatal("acquisition succeeded despite git lfs failing")
	}

	// And the half-built workspace is gone, so the run id is not stuck.
	if _, err := os.Stat(filepath.Join(f.mgr.WorkDir(), "run_lfsfail")); !os.IsNotExist(err) {
		t.Errorf("a workspace survived an LFS failure (err=%v)", err)
	}
}
