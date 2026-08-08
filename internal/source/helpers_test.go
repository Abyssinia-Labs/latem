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

// hermeticGitEnv pins everything about git that a developer's machine would
// otherwise supply.
//
// Without it these tests read ~/.gitconfig, so a machine with commit.gpgsign,
// init.defaultBranch = trunk, or a url.insteadOf rewrite produces failures
// that reproduce nowhere else. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM
// pointing at /dev/null is the supported way to say "no ambient config".
//
// protocol.file.allow is the one entry that is a test artefact rather than
// hygiene. Git has refused the local transport for submodules since
// CVE-2022-39253, which is correct and which production relies on; the
// fixtures here have nowhere but the local filesystem to put a submodule.
func hermeticGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=latem test",
		"GIT_AUTHOR_EMAIL=test@latem.invalid",
		"GIT_COMMITTER_NAME=latem test",
		"GIT_COMMITTER_EMAIL=test@latem.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=protocol.file.allow",
		"GIT_CONFIG_VALUE_0=always",
	}
}

// runGit runs git outside the Manager, for building fixtures and for asserting
// on the result. It deliberately does not go through [Manager.run]: a test that
// verifies the code under test using the code under test verifies nothing.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hermeticGitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitStatus runs git and reports the exit code instead of failing.
func gitStatus(t *testing.T, dir string, args ...string) int {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hermeticGitEnv()...)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return 0
}

// upstream is a real git repository standing in for a remote.
type upstream struct {
	t   *testing.T
	dir string
}

// newUpstream creates a repository with one commit on main.
func newUpstream(t *testing.T, dir string) *upstream {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	u := &upstream{t: t, dir: dir}
	u.commit("README.md", "hello\n", "first commit")
	return u
}

// commit writes a file and commits it, returning the new SHA.
func (u *upstream) commit(name, content, msg string) string {
	u.t.Helper()
	path := filepath.Join(u.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		u.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		u.t.Fatal(err)
	}
	runGit(u.t, u.dir, "add", "--", name)
	runGit(u.t, u.dir, "commit", "-q", "-m", msg)
	return runGit(u.t, u.dir, "rev-parse", "HEAD")
}

// annotatedTag creates a tag object, which is the case ${REF}^{commit} exists
// for: refs/tags/<name> then names a tag, not a commit.
func (u *upstream) annotatedTag(name, msg string) {
	u.t.Helper()
	runGit(u.t, u.dir, "tag", "-a", name, "-m", msg)
}

func (u *upstream) branch(name string) {
	u.t.Helper()
	runGit(u.t, u.dir, "branch", name)
}

// forcePush rewrites main to a brand new root commit, discarding the previous
// history entirely. This is the shape of the accident gc.auto 0 exists for:
// after the mirror prunes, the old tip is unreachable.
func (u *upstream) forcePush(content string) string {
	u.t.Helper()
	runGit(u.t, u.dir, "checkout", "-q", "--orphan", "rewritten")
	runGit(u.t, u.dir, "rm", "-rqf", "--ignore-unmatch", ".")
	if err := os.WriteFile(filepath.Join(u.dir, "README.md"), []byte(content), 0o600); err != nil {
		u.t.Fatal(err)
	}
	runGit(u.t, u.dir, "add", "--", "README.md")
	runGit(u.t, u.dir, "commit", "-q", "-m", "rewritten history")
	runGit(u.t, u.dir, "branch", "-qM", "main")
	return runGit(u.t, u.dir, "rev-parse", "HEAD")
}

// fixture is a Manager over fresh directories plus one upstream repo.
type fixture struct {
	t    *testing.T
	mgr  *Manager
	up   *upstream
	repo Repo
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	up := newUpstream(t, filepath.Join(root, "upstream"))
	mgr := newManager(t, root)
	return &fixture{
		t:    t,
		mgr:  mgr,
		up:   up,
		repo: Repo{Key: "myapp", URL: up.dir},
		root: root,
	}
}

// newManager builds a Manager rooted at root. Mirrors and work sit under one
// t.TempDir so they share a filesystem, which is what New insists on.
func newManager(t *testing.T, root string) *Manager {
	t.Helper()
	mgr, err := New(Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		Env:       hermeticGitEnv(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mgr
}

// acquire runs the whole of gates 6, 9 and 10 for a ref already known to be
// normalized.
func (f *fixture) acquire(runID, ref string) *Workspace {
	f.t.Helper()
	mirror, err := f.mgr.Fetch(context.Background(), f.repo)
	if err != nil {
		f.t.Fatalf("Fetch: %v", err)
	}
	ws, err := mirror.Acquire(context.Background(), runID, ref)
	if err != nil {
		f.t.Fatalf("Acquire(%s, %s): %v", runID, ref, err)
	}
	return ws
}

func (f *fixture) fetch() *Mirror {
	f.t.Helper()
	mirror, err := f.mgr.Fetch(context.Background(), f.repo)
	if err != nil {
		f.t.Fatalf("Fetch: %v", err)
	}
	return mirror
}
