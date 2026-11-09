package source

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// runSwitch runs [SwitchScript] in dir as an executor would: `sh -e`, the
// workspace root, the switch in the environment. extra overrides the hermetic
// git environment — an operator's own ~/.gitconfig, for the tests about one.
func runSwitch(t *testing.T, dir string, sw Switch, extra ...string) (string, error) {
	t.Helper()
	env, err := sw.Env()
	if err != nil {
		t.Fatalf("Switch.Env: %v", err)
	}
	cmd := exec.Command("sh", "-e", "-c", SwitchScript)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), hermeticGitEnv()...), extra...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitWith runs git with extra environment over the hermetic one, reporting
// failure rather than failing: the push tests are about which pushes fail.
func gitWith(dir string, extra []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), hermeticGitEnv()...), extra...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// A53: the mirror is asked only about the policy's prefixes, and a tag is
// peeled to the commit it names.
func TestContainingRefs(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	base := runGit(t, f.up.dir, "rev-parse", "HEAD")
	f.up.annotatedTag("v1", "one")
	runGit(t, f.up.dir, "checkout", "-q", "-b", "feature/x")
	side := f.up.commit("side.txt", "x\n", "only on a branch")
	runGit(t, f.up.dir, "checkout", "-q", "main")
	mirror := f.fetch()

	policy := []string{"refs/heads/main", "refs/tags/"}
	got, err := mirror.ContainingRefs(ctx, base, policy)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"refs/heads/main", "refs/tags/v1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("refs holding main's commit = %q, want %q", got, want)
	}
	if got, err := mirror.ContainingRefs(ctx, side, policy); err != nil || len(got) != 0 {
		t.Errorf("a commit only on feature/x = %q, %v; want none under the policy's prefixes", got, err)
	}
	if got, _ := mirror.ContainingRefs(ctx, side, []string{"refs/heads/"}); !slices.Equal(got, []string{"refs/heads/feature/x"}) {
		t.Errorf("under refs/heads/ = %q", got)
	}
	if got, err := mirror.ContainingRefs(ctx, base, nil); err != nil || got != nil {
		t.Errorf("no prefixes = %q, %v; want nothing asked", got, err)
	}
	if _, err := mirror.ContainingRefs(ctx, base, []string{"main"}); err == nil {
		t.Error("a short prefix was accepted")
	}
}

// The deploy shape: an earlier job pushed a version commit, the mirror has
// fetched it, the workspace was cloned before it existed. The pack carries
// it, and the switch lands on main with origin/main as its upstream, so a
// bare `git push` goes where GitHub's would.
func TestSwitchCarriesWhatTheMirrorFetchedSince(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	ws := f.acquire("run-1", "refs/heads/main")
	pinned := ws.SHA
	next := f.up.commit("VERSION", "1.0.1\n", "release: 1.0.1")
	mirror := f.fetch()

	if pack, err := mirror.PackSince(ctx, pinned, next); err != nil || pack != nil {
		t.Fatalf("PackSince to an ancestor = %d bytes, %v; the workspace already has it", len(pack), err)
	}
	pack, err := mirror.PackSince(ctx, next, pinned)
	if err != nil || len(pack) == 0 {
		t.Fatalf("PackSince = %d bytes, %v", len(pack), err)
	}
	if gitStatus(t, ws.Path, "cat-file", "-e", next+"^{commit}") == 0 {
		t.Fatal("the workspace already had the new commit; the test proves nothing")
	}
	packPath := filepath.Join(t.TempDir(), "checkout.pack")
	if err := os.WriteFile(packPath, pack, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runSwitch(t, ws.Path, Switch{SHA: next, Branch: "main", Tracking: true, Pack: packPath, Clean: true})
	if err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != next {
		t.Errorf("HEAD = %s, want %s", head, next)
	}
	if b := runGit(t, ws.Path, "symbolic-ref", "HEAD"); b != "refs/heads/main" {
		t.Errorf("HEAD is %s, want the branch main", b)
	}
	if up := runGit(t, ws.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/main" {
		t.Errorf("upstream = %s", up)
	}
	if tip := runGit(t, ws.Path, "rev-parse", "origin/main"); tip != next {
		t.Errorf("origin/main = %s, want the commit just resolved", tip)
	}
	if !strings.Contains(out, "HEAD is now at") {
		t.Errorf("the log does not say where HEAD is:\n%s", out)
	}

	// A tag or a commit detaches.
	if out, err := runSwitch(t, ws.Path, Switch{SHA: pinned}); err != nil {
		t.Fatalf("switch back: %v\n%s", err, out)
	}
	if gitStatus(t, ws.Path, "symbolic-ref", "-q", "HEAD") == 0 {
		t.Error("a commit checkout left HEAD on a branch")
	}
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != pinned {
		t.Errorf("HEAD = %s, want %s", head, pinned)
	}
}

// clean is GitHub's `git clean -ffdx` and `reset --hard`; without it, what
// earlier steps left stays.
func TestSwitchClean(t *testing.T) {
	f := newFixture(t)
	ws := f.acquire("run-1", "refs/heads/main")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(ws.Path, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("node_modules/pkg/index.js", "installed\n")
	write("dist/out.js", "built\n")
	write("README.md", "edited\n")

	if out, err := runSwitch(t, ws.Path, Switch{SHA: ws.SHA, Clean: true}); err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	for _, gone := range []string{"node_modules", "dist"} {
		if _, err := os.Stat(filepath.Join(ws.Path, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived a clean: %v", gone, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Path, "README.md")); string(b) != "hello\n" {
		t.Errorf("README.md = %q, want the commit's", b)
	}

	write("dist/out.js", "built\n")
	if out, err := runSwitch(t, ws.Path, Switch{SHA: ws.SHA}); err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "dist/out.js")); err != nil {
		t.Errorf("clean: false removed an untracked file: %v", err)
	}
}

// treeFiles is every file in a workspace's working tree, outside .git,
// slash-separated and sorted.
func treeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// A83: a sparse checkout leaves only what its patterns match, as GitHub's
// `sparse-checkout:` does — sokie's one file, not in cone mode, on a commit
// the workspace was cloned before; a directory in cone mode, which keeps the
// root's own files as cone mode always does; and, once a checkout names no
// patterns, the whole tree again, with no sparse config left behind.
func TestSwitchSparse(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t)
	f.up.commit("packages/cli/package.json", `{"version":"1.0.0"}`+"\n", "cli")
	f.up.commit("packages/cli/index.js", "cli\n", "cli code")
	f.up.commit("src/app.go", "package app\n", "app")
	ws := f.acquire("run-1", "refs/heads/main")
	pinned := ws.SHA
	all := []string{"README.md", "packages/cli/index.js", "packages/cli/package.json", "src/app.go"}
	if got := treeFiles(t, ws.Path); !slices.Equal(got, all) {
		t.Fatalf("the workspace holds %q before any checkout, want %q", got, all)
	}

	// The commit an earlier job pushed adds a file the patterns do not
	// match, and the checkout must not write it.
	f.up.commit("src/new.go", "package app\n", "more app")
	next := f.up.commit("packages/cli/package.json", `{"version":"1.0.1"}`+"\n", "release: 1.0.1")
	pack, err := f.fetch().PackSince(ctx, next, pinned)
	if err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(t.TempDir(), "checkout.pack")
	if err := os.WriteFile(packPath, pack, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runSwitch(t, ws.Path, Switch{SHA: next, Pack: packPath, Clean: true,
		Sparse: []string{"packages/cli/package.json"}})
	if err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if got := treeFiles(t, ws.Path); !slices.Equal(got, []string{"packages/cli/package.json"}) {
		t.Errorf("not in cone mode, the tree holds %q, want only packages/cli/package.json", got)
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Path, "packages/cli/package.json")); !strings.Contains(string(b), "1.0.1") {
		t.Errorf("package.json = %q, want the checked-out commit's", b)
	}
	if head := runGit(t, ws.Path, "rev-parse", "HEAD"); head != next {
		t.Errorf("HEAD = %s, want %s", head, next)
	}
	if st := runGit(t, ws.Path, "status", "--porcelain"); st != "" {
		t.Errorf("the sparse tree is not clean:\n%s", st)
	}

	out, err = runSwitch(t, ws.Path, Switch{SHA: pinned, Clean: true, Sparse: []string{"src"}, SparseCone: true})
	if err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if got, want := treeFiles(t, ws.Path), []string{"README.md", "src/app.go"}; !slices.Equal(got, want) {
		t.Errorf("in cone mode, the tree holds %q, want %q", got, want)
	}

	out, err = runSwitch(t, ws.Path, Switch{SHA: pinned, Clean: true})
	if err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if got := treeFiles(t, ws.Path); !slices.Equal(got, all) {
		t.Errorf("with no patterns, the tree holds %q, want all of %q", got, all)
	}
	if v, _ := gitWith(ws.Path, nil, "config", "--bool", "core.sparseCheckout"); v == "true" {
		t.Error("core.sparseCheckout is still true")
	}
	if code := gitStatus(t, ws.Path, "config", "--get", "extensions.worktreeConfig"); code == 0 {
		t.Error("the worktree-config extension sparse-checkout set was left behind")
	}
}

// gitServer is `git http-backend` behind a check for one job token, standing
// in for github.com. It counts the requests the token came with and the ones
// it did not.
type gitServer struct {
	*httptest.Server
	authed, refused atomic.Int32
}

func newGitServer(t *testing.T, root, token string) *gitServer {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  append([]string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}, hermeticGitEnv()...),
	}
	want := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	s := &gitServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, cred, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "basic") || cred != want {
			s.refused.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="latem test"`)
			http.Error(w, "a job token is required", http.StatusUnauthorized)
			return
		}
		s.authed.Add(1)
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// The version job's push, end to end: the job's token from a file outside the
// workspace, origin pinned against an operator config that rewrites every
// URL to ssh and keeps a credential helper, a bare `git push` that arrives
// with the token — and none once persist-credentials is false.
func TestSwitchPushesAsTheJobToken(t *testing.T) {
	f := newFixture(t)
	served := filepath.Join(f.root, "served")
	runGit(t, f.root, "clone", "-q", "--bare", f.up.dir, filepath.Join(served, "app.git"))
	runGit(t, filepath.Join(served, "app.git"), "config", "http.receivepack", "true")
	const token = "ghs_job0token0for0this0job0only"
	srv := newGitServer(t, served, token)
	remote := srv.URL + "/app.git"

	helperRan := filepath.Join(f.root, "helper-ran")
	global := filepath.Join(f.root, "operator.gitconfig")
	if err := os.WriteFile(global, []byte(`[url "ssh://git@example.invalid/"]
	insteadOf = `+srv.URL+`/
	pushInsteadOf = `+srv.URL+`/
[credential]
	helper = "!f() { touch '`+helperRan+`'; }; f"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := []string{"GIT_CONFIG_GLOBAL=" + global, "GIT_TERMINAL_PROMPT=0"}

	ws := f.acquire("run-1", "refs/heads/main")
	creds := filepath.Join(t.TempDir(), "git-credentials.config")
	cfg, err := CredentialConfig(remote, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runSwitch(t, ws.Path, Switch{SHA: ws.SHA, Branch: "main", Remote: remote, Include: creds}, operator...); err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(ws.Path, "VERSION"), []byte("1.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "VERSION"}, {"commit", "-q", "-m", "release: 1.0.1"}} {
		if out, err := gitWith(ws.Path, operator, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if out, err := gitWith(ws.Path, operator, "push"); err != nil {
		t.Fatalf("a bare git push failed: %v\n%s", err, out)
	}
	head := runGit(t, ws.Path, "rev-parse", "HEAD")
	if got := runGit(t, filepath.Join(served, "app.git"), "rev-parse", "main"); got != head {
		t.Errorf("served main = %s, want the pushed %s", got, head)
	}
	if srv.authed.Load() == 0 {
		t.Error("no request carried the job's token")
	}
	if _, err := os.Stat(helperRan); !os.IsNotExist(err) {
		t.Error("the operator's credential helper was consulted")
	}
	local, err := os.ReadFile(filepath.Join(ws.Path, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range CredentialSecrets(token) {
		if strings.Contains(string(local), form) {
			t.Errorf(".git/config holds the token (as %q…)", form[:8])
		}
	}

	// persist-credentials: false on a later checkout takes it away.
	if out, err := runSwitch(t, ws.Path, Switch{SHA: head, Branch: "main", Remote: remote}, operator...); err != nil {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "VERSION"), []byte("1.0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := gitWith(ws.Path, operator, "commit", "-qam", "release: 1.0.2"); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	refusedBefore := srv.refused.Load()
	if out, err := gitWith(ws.Path, operator, "push"); err == nil {
		t.Fatalf("a push without a credential succeeded:\n%s", out)
	}
	if srv.refused.Load() == refusedBefore {
		t.Error("the push without a credential never reached the server")
	}
}

// When the operator's config rewrites origin in a way a self-mapping cannot
// outrank — the same URL, read first — the step fails and leaves no
// credential behind, rather than letting a push go out as somebody else.
func TestSwitchRefusesAPushURLItCannotPin(t *testing.T) {
	f := newFixture(t)
	const remote = "https://github.com/acme/app.git"
	global := filepath.Join(f.root, "operator.gitconfig")
	if err := os.WriteFile(global, []byte("[url \"ssh://git@example.invalid/app.git\"]\n\tinsteadOf = "+remote+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := f.acquire("run-1", "refs/heads/main")
	creds := filepath.Join(t.TempDir(), "git-credentials.config")
	out, err := runSwitch(t, ws.Path, Switch{SHA: ws.SHA, Branch: "main", Remote: remote, Include: creds}, "GIT_CONFIG_GLOBAL="+global)
	if err == nil {
		t.Fatalf("the switch succeeded with origin pushing elsewhere:\n%s", out)
	}
	if !strings.Contains(out, "ssh://git@example.invalid/app.git") {
		t.Errorf("the refusal does not name where origin pushes:\n%s", out)
	}
	if code := gitStatus(t, ws.Path, "config", "--get", "include.path"); code == 0 {
		t.Error("the credential's include was left in place")
	}
}

func TestSwitchEnvRefusesWhatTheScriptCannotCarry(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for name, sw := range map[string]Switch{
		"a ref for a commit":     {SHA: "main"},
		"an ssh remote":          {SHA: sha, Remote: "git@github.com:acme/app.git"},
		"a remote with userinfo": {SHA: sha, Remote: "https://x:y@github.com/acme/app.git"},
		"a pattern of two lines": {SHA: sha, Sparse: []string{"src\ndocs"}},
		"an empty pattern":       {SHA: sha, Sparse: []string{""}},
	} {
		if _, err := sw.Env(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The file is git config, and what git reads out of it is the header GitHub's
// checkout sends, scoped to the host.
func TestCredentialConfig(t *testing.T) {
	const token = "ghs_abc"
	cfg, err := CredentialConfig("https://github.com/acme/app.git", token)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(file, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	got := runGit(t, t.TempDir(), "config", "--file", file, "--get", "http.https://github.com/.extraheader")
	want := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	if got != want {
		t.Errorf("extraheader = %q, want %q", got, want)
	}
	if forms := CredentialSecrets(token); !slices.Contains(forms, token) || !slices.Contains(forms, want[len("AUTHORIZATION: basic "):]) {
		t.Errorf("CredentialSecrets = %q, want the token and its header form", forms)
	}
	if _, err := CredentialConfig("git@github.com:acme/app.git", token); err == nil {
		t.Error("an ssh remote was given an http header")
	}
}
