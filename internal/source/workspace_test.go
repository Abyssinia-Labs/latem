package source

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestF2_WorkspaceSurvivesTheMirrorDisappearing is the regression test for
// finding F2, and it is the reason this package clones instead of adding a
// worktree.
//
// A worktree's .git is a file holding "gitdir: /abs/path/into/the/mirror". The
// executors bind-mount only the workspace, so inside a container that path
// does not exist and every git command a build script runs fails with "not a
// git repository" — and build scripts run `git rev-parse HEAD` and `git
// describe` constantly, for version stamps and build numbers.
//
// The test approximates the container by making the mirror unreachable: first
// moved aside, then deleted outright. If this package ever goes back to
// worktrees, or acquires an alternates file, or hands out a --shared clone,
// every assertion below fails.
func TestF2_WorkspaceSurvivesTheMirrorDisappearing(t *testing.T) {
	f := newFixture(t)
	f.up.annotatedTag("v1.0", "release one")
	f.up.commit("app.go", "package main\n", "second commit")

	ws := f.acquire("run_f2", "refs/heads/main")

	// .git must be a directory. This is the specific bit of shape that
	// distinguishes a clone from a worktree.
	info, err := os.Lstat(filepath.Join(ws.Path, ".git"))
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf(".git is a %s, not a directory — this is the worktree shape F2 rejects", info.Mode())
	}

	// A clone must not depend on the mirror's objects through an alternates
	// file either, which would be self-contained in shape and not in fact.
	if _, err := os.Stat(filepath.Join(ws.Path, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("workspace borrows objects via alternates; it is not self-contained (err=%v)", err)
	}

	mirror := filepath.Join(f.mgr.MirrorDir(), "myapp.git")

	// Step one: the mirror is no longer at the path it was cloned from.
	moved := filepath.Join(f.root, "mirror-moved-away")
	if err := os.Rename(mirror, moved); err != nil {
		t.Fatalf("moving the mirror aside: %v", err)
	}
	assertSelfContained(t, ws, "with the mirror moved aside")

	// Step two: the mirror is gone entirely. Hard-linked objects survive
	// because unlinking one name only decrements a link count.
	if err := os.RemoveAll(moved); err != nil {
		t.Fatalf("deleting the mirror: %v", err)
	}
	assertSelfContained(t, ws, "with the mirror deleted")
}

// assertSelfContained runs the git commands a real build script runs.
func assertSelfContained(t *testing.T, ws *Workspace, when string) {
	t.Helper()

	if got := runGit(t, ws.Path, "rev-parse", "HEAD"); got != ws.SHA {
		t.Errorf("%s: rev-parse HEAD = %s, want %s", when, got, ws.SHA)
	}
	if got := runGit(t, ws.Path, "describe", "--tags"); !strings.HasPrefix(got, "v1.0") {
		t.Errorf("%s: describe --tags = %q, want something derived from v1.0", when, got)
	}
	if got := runGit(t, ws.Path, "status", "--porcelain"); got != "" {
		t.Errorf("%s: status --porcelain = %q, want a clean tree", when, got)
	}
	if got := runGit(t, ws.Path, "rev-parse", "--show-toplevel"); !sameDir(got, ws.Path) {
		t.Errorf("%s: show-toplevel = %q, want %q", when, got, ws.Path)
	}
	if got := runGit(t, ws.Path, "log", "-1", "--format=%H"); got != ws.SHA {
		t.Errorf("%s: log -1 = %s, want %s", when, got, ws.SHA)
	}
}

// sameDir compares two paths through the symlink resolution macOS applies to
// /var vs /private/var, which t.TempDir hands out and git reports resolved.
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// TestF2_WorktreeShapeIsWhatWeAreAvoiding documents what the rejected design
// actually produces, so the assertion above is anchored to a fact rather than
// to a claim in a comment.
func TestF2_WorktreeShapeIsWhatWeAreAvoiding(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch().Path()

	wt := filepath.Join(f.root, "as-a-worktree")
	runGit(t, mirror, "worktree", "add", "--detach", wt, "refs/heads/main")

	info, err := os.Lstat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("stat .git: %v", err)
	}
	if info.IsDir() {
		t.Skip("this git version gives worktrees a .git directory; the F2 contrast does not apply")
	}

	body, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("read .git: %v", err)
	}
	if !strings.HasPrefix(string(body), "gitdir: ") {
		t.Fatalf(".git = %q, expected a gitdir: pointer", body)
	}
	if !strings.Contains(string(body), mirror) {
		t.Errorf(".git pointer %q does not name the mirror; the F2 argument assumed it would", body)
	}
}

// TestCheckout_HardlinksTheObjectStore is the cost half of §12: the workspace
// is cheap because objects are linked, not copied. A regression here is
// invisible in correctness tests and shows up as every run on a large repo
// writing gigabytes.
func TestCheckout_HardlinksTheObjectStore(t *testing.T) {
	f := newFixture(t)
	ws := f.acquire("run_links", "refs/heads/main")
	mirror := f.fetch().Path()

	// Loose objects only; a packed mirror has no per-object files to compare.
	// The fixture is small enough that everything is loose.
	linked := 0
	root := filepath.Join(ws.Path, ".git", "objects")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || strings.HasPrefix(rel, "info/") || strings.HasPrefix(rel, "pack/") {
			return relErr
		}
		wsInfo, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		mirrorInfo, statErr := os.Stat(filepath.Join(mirror, "objects", rel))
		if statErr != nil {
			return statErr
		}
		if !os.SameFile(wsInfo, mirrorInfo) {
			t.Errorf("object %s was copied, not hard-linked", rel)
		}
		linked++
		return nil
	})
	if err != nil {
		t.Fatalf("walking objects: %v", err)
	}
	if linked == 0 {
		t.Fatal("no loose objects compared; the test proved nothing")
	}
}

// TestCheckout_ChecksOutExactlyOnce guards the --no-checkout half of §12.
//
// Cloning without --no-checkout checks out the mirror's default branch and
// then the target, doubling I/O on a large repo for nothing. The regression is
// invisible in every behavioural assertion — the workspace ends up identical
// either way — so this checks the argv, which is where the difference lives.
func TestCheckout_ChecksOutExactlyOnce(t *testing.T) {
	f := newFixture(t)
	f.up.commit("second.txt", "two\n", "second commit")
	f.fetch() // build the mirror before recording, so only the run's argv is seen

	var argvs [][]string
	f.mgr.onGit = func(argv []string) { argvs = append(argvs, slices.Clone(argv)) }

	ws := f.acquire("run_once", "refs/heads/main")

	clones, checkouts := 0, 0
	for _, argv := range argvs {
		switch gitVerb(argv) {
		case "clone":
			if slices.Contains(argv, "--mirror") {
				t.Errorf("the mirror was re-cloned during a run: %q", argv)
				continue
			}
			clones++
			if !slices.Contains(argv, "--no-checkout") {
				t.Errorf("clone without --no-checkout, so the tree is written twice: %q", argv)
			}
			if !slices.Contains(argv, "--local") {
				t.Errorf("clone without --local, so objects were copied: %q", argv)
			}
		case "checkout":
			checkouts++
			if !slices.Contains(argv, "--detach") {
				t.Errorf("checkout without --detach: %q", argv)
			}
		}
	}
	if clones != 1 {
		t.Errorf("ran %d workspace clones, want 1", clones)
	}
	if checkouts != 1 {
		t.Errorf("ran %d checkouts, want exactly 1 — the second one is pure I/O", checkouts)
	}

	// And the result is still what it should be.
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != ws.SHA {
		t.Errorf("HEAD = %s, want %s", head, ws.SHA)
	}
}

// gitVerb returns the subcommand from a recorded argv, skipping the leading
// "-C <dir>" global option.
func gitVerb(argv []string) string {
	if len(argv) >= 3 && argv[0] == "-C" {
		return argv[2]
	}
	if len(argv) > 0 {
		return argv[0]
	}
	return ""
}

// TestCheckout_DetachesAndRewritesOrigin covers the two steps that make the
// workspace behave sensibly inside a container: HEAD is detached at the
// resolved commit rather than following a branch, and origin points at the
// real upstream rather than at a host path the container cannot see.
func TestCheckout_DetachesAndRewritesOrigin(t *testing.T) {
	f := newFixture(t)
	f.repo.URL = f.up.dir
	ws := f.acquire("run_detach", "refs/heads/main")

	if code := gitStatus(t, ws.Path, "symbolic-ref", "-q", "HEAD"); code == 0 {
		t.Error("HEAD follows a branch; the run would drift if the branch moved")
	}
	if got := runGit(t, ws.Path, "remote", "get-url", "origin"); got != f.repo.URL {
		t.Errorf("origin = %q, want the configured upstream %q", got, f.repo.URL)
	}
	if got := runGit(t, ws.Path, "config", "--get", "gc.auto"); got != "0" {
		t.Errorf("workspace gc.auto = %q, want 0 — a gc inside a run unpicks the hardlinks", got)
	}
}

// TestCheckout_RefusesRunIDsThatEscapeTheWorkDir is amendment A6: a run id is
// a path component, and "../../../../tmp/x" from a control plane writes
// wherever the agent's user can write.
func TestCheckout_RefusesRunIDsThatEscapeTheWorkDir(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	sha, err := mirror.Resolve(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	hostile := map[string]string{
		"parent traversal":  "../escape",
		"deep traversal":    "../../../../tmp/x",
		"absolute":          "/tmp/x",
		"dot":               ".",
		"dot dot":           "..",
		"embedded slash":    "runs/one",
		"leading dot":       ".hidden",
		"empty":             "",
		"newline":           "run\nid",
		"null byte":         "run\x00id",
		"shell separator":   "run; touch pwned",
		"command expansion": "$(touch pwned)",
	}

	for name, runID := range hostile {
		t.Run(name, func(t *testing.T) {
			ws, err := mirror.Checkout(context.Background(), runID, "refs/heads/main", sha)
			if err == nil {
				t.Fatalf("accepted run id %q, workspace at %s", runID, ws.Path)
			}
			if got := CodeOf(err); got != protocol.ErrSource {
				t.Errorf("code = %s, want %s", got, protocol.ErrSource)
			}
		})
	}

	// Nothing may have been created outside WorkDir, and WorkDir itself must
	// still be empty.
	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("work dir contains %v after only hostile run ids", names)
	}
	if _, err := os.Stat(filepath.Join(f.root, "escape")); !os.IsNotExist(err) {
		t.Errorf("a traversal escaped the work dir (err=%v)", err)
	}
}

// TestCheckout_RefusesAnExistingWorkspace: reaching gate 10 with a directory
// already in place means reconciliation missed something, and silently
// rm -rf-ing a stranger's workspace on a guess is worse than refusing.
func TestCheckout_RefusesAnExistingWorkspace(t *testing.T) {
	f := newFixture(t)
	f.acquire("run_dup", "refs/heads/main")

	mirror := f.fetch()
	if _, err := mirror.Acquire(context.Background(), "run_dup", "refs/heads/main"); err == nil {
		t.Fatal("a second acquisition reused an existing workspace")
	}
}

// TestCheckout_LeavesNothingBehindWhenItFails. A partial workspace would trip
// the "already exists" refusal above on the retry, turning a transient git
// failure into a permanently stuck run id.
func TestCheckout_LeavesNothingBehindWhenItFails(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	// A well-formed SHA that names no object: the clone succeeds and the
	// checkout fails, which is exactly the half-built case.
	absent := strings.Repeat("0", 39) + "1"
	if _, err := mirror.Checkout(context.Background(), "run_partial", "refs/heads/main", absent); err == nil {
		t.Fatal("checked out a commit that does not exist")
	}

	if _, err := os.Stat(filepath.Join(f.mgr.WorkDir(), "run_partial")); !os.IsNotExist(err) {
		t.Errorf("a partial workspace was left behind (err=%v)", err)
	}
}

func TestCheckout_RejectsUnresolvedSHAs(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()

	for _, sha := range []string{
		"",
		"HEAD",
		"main",
		"deadbeef",                       // abbreviated
		strings.Repeat("g", 40),          // not hex
		strings.Repeat("A", 40),          // uppercase; git writes lowercase
		"--upload-pack=/bin/sh",          //
		strings.Repeat("0", 40) + "^{}",  // peel syntax
		strings.Repeat("0", 40) + "\nx",  // second argument smuggled in
		strings.Repeat("0", 40) + " -x",  //
		strings.Repeat("0", 41),          // one too long
		strings.Repeat("0", 39),          // one too short
		strings.Repeat("0", 63),          // near SHA-256
		"HEAD@{0}",                       //
		"$(touch " + f.root + "/pwned)",  //
		"; touch " + f.root + "/pwned;",  //
		"`touch " + f.root + "/pwned`",   //
		"| touch " + f.root + "/pwned",   //
		"&& touch " + f.root + "/pwned2", //
	} {
		if _, err := mirror.Checkout(context.Background(), "run_sha", "refs/heads/main", sha); err == nil {
			t.Errorf("accepted %q as a resolved SHA", sha)
		}
	}
	assertNoSentinel(t, f.root)
}

func TestWorkspace_EnvIsWhatTheSpecPromises(t *testing.T) {
	f := newFixture(t)
	f.up.annotatedTag("v2.0", "release two")
	ws := f.acquire("run_env", "refs/tags/v2.0")

	env := ws.Env()
	want := map[string]string{
		"LATEM_SHA":    ws.SHA,
		"LATEM_REF":    "refs/tags/v2.0",
		"LATEM_REPO":   "myapp",
		"LATEM_RUN_ID": "run_env",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("Env() has %d keys, want %d: %v", len(env), len(want), env)
	}

	// LATEM_REF is the normalized name, so a job logging it records something
	// reproducible rather than whatever short form the broker sent.
	if !strings.HasPrefix(env["LATEM_REF"], "refs/") {
		t.Errorf("LATEM_REF = %q, want a fully-qualified ref", env["LATEM_REF"])
	}
}

// TestReconciliation_WorkspaceRemovalIsAPlainDelete covers RUNNER_SPEC §6: a
// crashed agent's leftovers are removable with no administrative state to
// prune in the mirror, and the mirror is untouched afterwards.
func TestReconciliation_WorkspaceRemovalIsAPlainDelete(t *testing.T) {
	f := newFixture(t)
	ws := f.acquire("run_gone", "refs/heads/main")
	mirror := f.fetch()

	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "run_gone" {
		t.Fatalf("ListWorkspaces = %v, want [run_gone]", names)
	}

	if err := ws.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := ws.Remove(); err != nil {
		t.Errorf("second Remove: %v (reconciliation must be able to run twice)", err)
	}

	// A worktree would have left administrative state under
	// mirror/worktrees/<id>; a clone leaves nothing.
	if entries, err := os.ReadDir(filepath.Join(mirror.Path(), "worktrees")); err == nil {
		t.Errorf("mirror has worktree admin state after cleanup: %d entries", len(entries))
	}
	// The mirror still resolves the ref the deleted run was using.
	if _, err := mirror.Resolve(context.Background(), "refs/heads/main"); err != nil {
		t.Errorf("deleting a workspace disturbed the mirror: %v", err)
	}

	names, err = f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("ListWorkspaces = %v after removal, want empty", names)
	}
}

func TestRemoveWorkspace_ByRunID(t *testing.T) {
	f := newFixture(t)
	f.acquire("run_byid", "refs/heads/main")

	if err := f.mgr.RemoveWorkspace("run_byid"); err != nil {
		t.Fatalf("RemoveWorkspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.mgr.WorkDir(), "run_byid")); !os.IsNotExist(err) {
		t.Errorf("workspace still present (err=%v)", err)
	}
	// Absent is not an error: reconciliation runs at every startup.
	if err := f.mgr.RemoveWorkspace("run_byid"); err != nil {
		t.Errorf("removing an absent workspace: %v", err)
	}
	// And it will not be talked into removing something else.
	if err := f.mgr.RemoveWorkspace("../mirrors"); err == nil {
		t.Error("RemoveWorkspace accepted a traversal")
	}
	if _, err := os.Stat(f.mgr.MirrorDir()); err != nil {
		t.Errorf("mirror directory was harmed: %v", err)
	}
}

// TestListWorkspaces_ReportsJunkItCannotRemove. Hiding an unrecognised
// directory would hide a leak; refusing to rm -rf it is deliberate, because
// this package did not create it and cannot vouch for what it is.
func TestListWorkspaces_ReportsJunkItCannotRemove(t *testing.T) {
	f := newFixture(t)
	junk := filepath.Join(f.mgr.WorkDir(), "not.a.run.id")
	if err := os.MkdirAll(junk, 0o700); err != nil {
		t.Fatal(err)
	}

	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "not.a.run.id" {
		t.Fatalf("ListWorkspaces = %v, want the junk entry reported", names)
	}
	if err := f.mgr.RemoveWorkspace("not.a.run.id"); err == nil {
		t.Error("RemoveWorkspace deleted a directory whose name is not a valid run id")
	}
}

func TestListWorkspaces_MissingDirIsEmpty(t *testing.T) {
	f := newFixture(t)
	if err := os.RemoveAll(f.mgr.WorkDir()); err != nil {
		t.Fatal(err)
	}
	names, err := f.mgr.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces on a missing dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("got %v, want empty", names)
	}
}

// TestSubmodules_OnlyWhenGitmodulesExists covers both halves of the §12 rule.
func TestSubmodules_OnlyWhenGitmodulesExists(t *testing.T) {
	f := newFixture(t)

	// Without .gitmodules there is nothing to do and nothing to slow down.
	plain := f.acquire("run_nosub", "refs/heads/main")
	if _, err := os.Stat(filepath.Join(plain.Path, ".gitmodules")); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has submodules (err=%v)", err)
	}

	// Now add one and prove it is populated.
	sub := newUpstream(t, filepath.Join(f.root, "sub"))
	sub.commit("lib.txt", "library\n", "sub commit")
	runGit(t, f.up.dir, "-c", "protocol.file.allow=always",
		"submodule", "add", "--", sub.dir, "vendor/lib")
	runGit(t, f.up.dir, "commit", "-q", "-m", "add submodule")

	ws := f.acquire("run_sub", "refs/heads/main")
	content, err := os.ReadFile(filepath.Join(ws.Path, "vendor", "lib", "lib.txt"))
	if err != nil {
		t.Fatalf("submodule content missing: %v", err)
	}
	if string(content) != "library\n" {
		t.Errorf("submodule content = %q", content)
	}
}

// TestUsesLFS is the guard on `git lfs fetch --all`, which is a no-op on a
// repo that does not use LFS and potentially tens of gigabytes on one that
// does. Getting the predicate wrong in either direction is expensive.
func TestUsesLFS(t *testing.T) {
	cases := map[string]struct {
		content string
		want    bool
	}{
		"absent":                {"", false},
		"unrelated attributes":  {"*.go text eol=lf\n", false},
		"canonical lfs line":    {"*.psd filter=lfs diff=lfs merge=lfs -text\n", true},
		"uppercase":             {"*.PSD FILTER=LFS DIFF=LFS -text\n", true},
		"lfs only in a comment": {"# we removed filter=lfs in 2024\n*.go text\n", false},
		"directory named lfs":   {"lfs/** binary\n", false},
		"lfs later in file":     {"*.go text\n\n*.bin filter=lfs -text\n", true},
		"leading whitespace":    {"   *.bin filter=lfs\n", true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".gitattributes")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := usesLFS(path)
			if err != nil {
				t.Fatalf("usesLFS: %v", err)
			}
			if got != tc.want {
				t.Errorf("usesLFS(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestUsesLFS_ReportsAnUnreadableAttributesFile. A .gitattributes that is a
// directory is what a repo looks like after someone commits one by accident;
// it must be an error, not silently "no LFS here".
func TestUsesLFS_ReportsAnUnreadableAttributesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitattributes")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := usesLFS(path); err == nil {
		t.Error("read a .gitattributes that is a directory")
	}
}

// TestCheckout_ReportsAnUnusableWorkspacePath covers the stat branch that is
// neither "there" nor "not there".
func TestCheckout_ReportsAnUnusableWorkspacePath(t *testing.T) {
	f := newFixture(t)
	mirror := f.fetch()
	blocker := filepath.Join(f.mgr.WorkDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.Acquire(context.Background(), "blocker", "refs/heads/main"); err == nil {
		t.Fatal("cloned over a regular file")
	}
}

// TestSubmodules_FailureFailsTheRun. A submodule that cannot be fetched means
// the workspace is missing code the build needs; carrying on produces a
// compile error nobody can trace back to here.
func TestSubmodules_FailureFailsTheRun(t *testing.T) {
	f := newFixture(t)
	sub := newUpstream(t, filepath.Join(f.root, "sub"))
	sub.commit("lib.txt", "library\n", "sub commit")
	runGit(t, f.up.dir, "-c", "protocol.file.allow=always",
		"submodule", "add", "--", sub.dir, "vendor/lib")
	runGit(t, f.up.dir, "commit", "-q", "-m", "add submodule")

	mirror := f.fetch()

	// The submodule's own repository goes away between mirror and checkout.
	if err := os.RemoveAll(sub.dir); err != nil {
		t.Fatal(err)
	}

	if _, err := mirror.Acquire(context.Background(), "run_badsub", "refs/heads/main"); err == nil {
		t.Fatal("acquired a workspace whose submodule cannot be fetched")
	}
	if _, err := os.Stat(filepath.Join(f.mgr.WorkDir(), "run_badsub")); !os.IsNotExist(err) {
		t.Errorf("a workspace survived a submodule failure (err=%v)", err)
	}
}

func TestUsesLFS_RefusesAnAbsurdlyLargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitattributes")
	if err := os.WriteFile(path, make([]byte, gitattributesLimit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := usesLFS(path); err == nil {
		t.Error("read a .gitattributes larger than the cap")
	}
}
