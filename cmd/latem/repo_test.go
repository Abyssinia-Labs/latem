package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// lsRemoteCall is one probe the fake saw.
type lsRemoteCall struct {
	url       string
	keepAgent bool
}

// fakeLsRemote answers `git ls-remote` without a network. answer is given
// whether the probe kept the ssh agent, which is the distinction verifyRepo
// draws.
func fakeLsRemote(t *testing.T, answer func(keepAgent bool) (string, error)) *[]lsRemoteCall {
	t.Helper()
	var calls []lsRemoteCall
	prev := lsRemote
	lsRemote = func(_ context.Context, url string, keepAgent bool) (string, error) {
		calls = append(calls, lsRemoteCall{url, keepAgent})
		return answer(keepAgent)
	}
	t.Cleanup(func() { lsRemote = prev })
	return &calls
}

func readable(head string) func(bool) (string, error) {
	return func(bool) (string, error) {
		return "ref: refs/heads/" + head + "\tHEAD\n0123456789abcdef0123456789abcdef01234567\tHEAD\n", nil
	}
}

func TestRepoAdd_WritesTheTableAndKeepsTheRestOfTheFile(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	calls := fakeLsRemote(t, readable("trunk"))
	reloads := noAgentRunning(t)

	out, err := latemIn(t, dir, "", "repo", "add", "Me/Latem-Cloud")
	if err != nil {
		t.Fatalf("repo add: %v\n%s", err, out)
	}
	if want := []lsRemoteCall{{"https://github.com/Me/Latem-Cloud.git", false}}; !slices.Equal(*calls, want) {
		t.Errorf("probes = %v, want one without the ssh agent: %v", *calls, want)
	}
	if *reloads != 1 {
		t.Errorf("asked the agent to reload %d times", *reloads)
	}

	body := readConfigFile(t, dir)
	if !strings.Contains(body, "# The operator's own note") {
		t.Error("the operator's comment was lost")
	}
	cfg := loadConfigIn(t, dir)
	r, ok := cfg.Repos["latem-cloud"]
	if !ok {
		t.Fatalf("no repos.latem-cloud:\n%s", body)
	}
	if r.URL != "https://github.com/Me/Latem-Cloud.git" {
		t.Errorf("url = %q", r.URL)
	}
	// The default branch the remote named, not a guessed "main".
	if want := []string{"trunk", "refs/pull/*/merge"}; !slices.Equal(r.AllowedRefs, want) {
		t.Errorf("allowed_refs = %v, want %v", r.AllowedRefs, want)
	}
	if strings.Index(body, "[repos.latem-cloud]") > strings.Index(body, "[[runner]]") {
		t.Errorf("the new repo was not filed with the others:\n%s", body)
	}
	for _, want := range []string{"added repos.latem-cloud", "no agent is running", "latem workflows check", "latem secrets set --repo latem-cloud"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The state `latem setup` leaves a machine in: a runner, and no repo yet.
func TestRepoAdd_IntoAConfigWithNoRepos(t *testing.T) {
	dir := configDirWith(t, "schema_version = 1\n\n[[runner]]\nname = \"linux\"\nexecutor = \"docker\"\nlabels = [\"linux\"]\n")
	fakeLsRemote(t, readable("main"))
	noAgentRunning(t)

	if out, err := latemIn(t, dir, "", "repo", "add", "me/app", "--key", "web", "--require-approval"); err != nil {
		t.Fatalf("repo add: %v\n%s", err, out)
	}
	r := loadConfigIn(t, dir).Repos["web"]
	if r == nil || !r.RequireApprovalOtherwise {
		t.Fatalf("repos.web = %+v", r)
	}
}

// A repository your shell can read and a service cannot is refused, because
// that is exactly the one that fails every run at fetch while every check
// typed by hand says it is fine.
func TestRepoAdd_RefusesWhatOnlyYourShellCanRead(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeLsRemote(t, func(keepAgent bool) (string, error) {
		if keepAgent {
			return "", nil
		}
		return "git@github.com: Permission denied (publickey).\n", errors.New("exit status 128")
	})
	calls := noAgentRunning(t)

	_, err := latemIn(t, dir, "", "repo", "add", "me/private")
	if err == nil || !strings.Contains(err.Error(), "readable from your shell and not from a service") {
		t.Fatalf("want the shell-only diagnosis, got %v", err)
	}
	if readConfigFile(t, dir) != baseConfig {
		t.Error("a refused repo was written anyway")
	}
	if *calls != 0 {
		t.Error("the agent was asked to reload after nothing changed")
	}
}

func TestRepoAdd_RefusesAnUnreadableRepository(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeLsRemote(t, func(bool) (string, error) {
		return "remote: Repository not found.\nfatal: repository 'https://github.com/me/nope.git/' not found\n",
			errors.New("exit status 128")
	})
	noAgentRunning(t)

	_, err := latemIn(t, dir, "", "repo", "add", "me/nope")
	if err == nil || !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "--no-verify") {
		t.Fatalf("want a refusal naming --no-verify, got %v", err)
	}
	if readConfigFile(t, dir) != baseConfig {
		t.Error("an unreadable repo was written anyway")
	}
}

func TestRepoAdd_NoVerifyAndExplicitRefs(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	calls := fakeLsRemote(t, readable("main"))
	noAgentRunning(t)

	if out, err := latemIn(t, dir, "", "repo", "add", "https://github.com/me/site.git", "--no-verify",
		"--allowed-refs", "main,release/*"); err != nil {
		t.Fatalf("repo add: %v\n%s", err, out)
	}
	if len(*calls) != 0 {
		t.Errorf("--no-verify still probed: %v", *calls)
	}
	if got := loadConfigIn(t, dir).Repos["site"].AllowedRefs; !slices.Equal(got, []string{"main", "release/*"}) {
		t.Errorf("allowed_refs = %v", got)
	}
}

func TestRepoAdd_RefusesADuplicate(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeLsRemote(t, readable("main"))
	noAgentRunning(t)

	if _, err := latemIn(t, dir, "", "repo", "add", "other/app"); err == nil || !strings.Contains(err.Error(), "already in") {
		t.Errorf("a second repos.app: %v", err)
	}
	if _, err := latemIn(t, dir, "", "repo", "add", "ME/APP", "--key", "again"); err == nil ||
		!strings.Contains(err.Error(), "already allowed, as repos.app") {
		t.Errorf("the same repository under a second key: %v", err)
	}
	if readConfigFile(t, dir) != baseConfig {
		t.Error("a refused duplicate changed the file")
	}
}

func TestRepoAdd_UsageErrors(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	for _, args := range [][]string{{"repo", "add"}, {"repo", "add", "not-a-slug"}, {"repo", "add", "https://gitlab.com/a/b.git"}} {
		if _, err := latemIn(t, dir, "", args...); exitCode(err) != ExitUsage {
			t.Errorf("%v: exit %d, want usage (%v)", args, exitCode(err), err)
		}
	}
}

func TestRepoList(t *testing.T) {
	dir := configDirWith(t, baseConfig+"\n[repos.local]\nurl = \"/srv/git/local.git\"\nallowed_refs = [\"main\"]\nrequire_approval_otherwise = true\n")
	out, err := latemIn(t, dir, "", "repo", "list", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	want := "KEY\tREPOSITORY\tALLOWED REFS\tURL\n" +
		"app\tme/app\tmain\thttps://github.com/me/app.git\n" +
		"local\t-\tmain (+approval)\t/srv/git/local.git\n"
	if out != want {
		t.Errorf("repo list =\n%s\nwant\n%s", out, want)
	}
}

func TestRepoRemove(t *testing.T) {
	body := baseConfig + `
[repos.site]
url          = "https://github.com/me/site.git"
allowed_refs = ["main"]

[repos.site.environments.production.vars]
URL = "https://example.com"
`
	t.Run("by owner/name, with its environments", func(t *testing.T) {
		dir := configDirWith(t, body)
		calls := noAgentRunning(t)
		out, err := latemIn(t, dir, "", "repo", "remove", "me/site")
		if err != nil {
			t.Fatalf("repo remove: %v\n%s", err, out)
		}
		got := readConfigFile(t, dir)
		if strings.Contains(got, "site") {
			t.Errorf("repos.site survived:\n%s", got)
		}
		if !strings.Contains(got, "# The operator's own note") || loadConfigIn(t, dir).Repos["app"] == nil {
			t.Errorf("the rest of the file did not survive:\n%s", got)
		}
		if *calls != 1 {
			t.Errorf("reloads = %d", *calls)
		}
	})
	t.Run("an unknown repo", func(t *testing.T) {
		dir := configDirWith(t, body)
		noAgentRunning(t)
		_, err := latemIn(t, dir, "", "repo", "remove", "nope")
		if err == nil || !strings.Contains(err.Error(), "it has app, site") {
			t.Errorf("want the keys named, got %v", err)
		}
	})
}

func TestParseGitHubSlugAndRepoKey(t *testing.T) {
	for in, want := range map[string]string{
		"me/app":                        "me/app",
		"me/app.git":                    "me/app",
		"https://github.com/me/app":     "me/app",
		"git@github.com:me/Next.js.git": "me/Next.js",
	} {
		if got, err := parseGitHubSlug(in); err != nil || got != want {
			t.Errorf("parseGitHubSlug(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"me/App":          "app",
		"me/Next.js":      "next.js",
		"me/my_repo":      "my_repo",
		"me/.dotfiles":    "dotfiles",
		"me/start-convex": "start-convex",
	} {
		if got := repoKeyFor(in); got != want {
			t.Errorf("repoKeyFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// The probe is the agent's git, not the operator's shell: no ssh agent, no
// prompt, and none of the variables that point git at another repository.
func TestServiceGitEnv(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GIT_DIR", "/elsewhere/.git")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	env := serviceGitEnv(false)
	for _, kv := range env {
		for _, banned := range []string{"SSH_AUTH_SOCK=", "GIT_DIR=", "GIT_TERMINAL_PROMPT=1"} {
			if strings.HasPrefix(kv, banned) {
				t.Errorf("the service environment carries %s", kv)
			}
		}
	}
	if !slices.Contains(env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("prompting is not turned off")
	}
	if !slices.Contains(serviceGitEnv(true), "SSH_AUTH_SOCK=/tmp/agent.sock") {
		t.Error("keepAgent did not keep the agent")
	}
}
