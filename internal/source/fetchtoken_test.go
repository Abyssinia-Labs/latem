package source

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// served is the fixture's repository behind [gitServer], which reads the
// token and refuses anything else.
func (f *fixture) served(t *testing.T, token string) *gitServer {
	t.Helper()
	dir := filepath.Join(f.root, "served")
	runGit(t, f.root, "clone", "-q", "--bare", f.up.dir, filepath.Join(dir, "app.git"))
	return newGitServer(t, dir, token)
}

// A machine with no GitHub login, which is what an agent is at boot when its
// login is in a keyring nobody has opened. The token the work came with is all
// it has, and it is enough: for the clone, for the fetch after it and for a
// commit named outright. git prompts for nothing, and it is all in one process's
// environment: not on a command line, and not in the mirror's config.
func TestFetchWithTheTokenTheWorkBrought(t *testing.T) {
	f := newFixture(t)
	const token = "ghs_read0only0for0one0repository"
	srv := f.served(t, token)
	repo := Repo{Key: "app", URL: srv.URL + "/app.git", Token: token}

	var argv [][]string
	f.mgr.onGit = func(a []string) { argv = append(argv, slices.Clone(a)) }

	ctx := t.Context()
	if _, err := f.mgr.Fetch(ctx, repo); err != nil {
		t.Fatalf("the clone: %v", err)
	}
	head := f.up.commit("b.txt", "b\n", "second")
	runGit(t, f.up.dir, "push", "-q", filepath.Join(f.root, "served", "app.git"), "main")
	mirror, err := f.mgr.Fetch(ctx, repo)
	if err != nil {
		t.Fatalf("the fetch: %v", err)
	}
	has, err := mirror.HasCommit(ctx, head)
	if err != nil || !has {
		t.Fatalf("the fetch did not bring %s: %v", head, err)
	}
	if err := mirror.FetchCommit(ctx, head); err != nil {
		t.Fatalf("a commit by name: %v", err)
	}
	if srv.authed.Load() == 0 {
		t.Fatal("no request reached the repository with the token")
	}

	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	for _, a := range argv {
		line := strings.Join(a, " ")
		if strings.Contains(line, token) || strings.Contains(line, header) {
			t.Errorf("a token on a command line: %s", line)
		}
	}
	config, err := os.ReadFile(filepath.Join(mirror.Path(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), token) || strings.Contains(string(config), header) {
		t.Errorf("a token in the mirror's config:\n%s", config)
	}

	// And the same machine without it is the failure this exists to end.
	repo.Token = ""
	if _, err := f.mgr.Fetch(ctx, repo); CodeOf(err) != protocol.ErrSource {
		t.Fatalf("with no token and no login: %v", err)
	}
}

// A token can expire under a long job, or come for a repository the machine's
// own login reaches and it does not. That login still gets its turn.
func TestFetchFallsBackToTheMachinesLogin(t *testing.T) {
	f := newFixture(t)
	const login = "ghs_this0machines0own0login"
	srv := f.served(t, login)

	global := filepath.Join(f.root, "machine.gitconfig")
	helper := `!f() { echo username=x-access-token; echo password=` + login + `; }; f`
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = \""+helper+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := New(Config{
		MirrorDir: filepath.Join(f.root, "machine", "mirrors"),
		WorkDir:   filepath.Join(f.root, "machine", "work"),
		Env:       append(hermeticGitEnv(), "GIT_CONFIG_GLOBAL="+global),
	})
	if err != nil {
		t.Fatal(err)
	}

	repo := Repo{Key: "app", URL: srv.URL + "/app.git", Token: "ghs_expired0an0hour0ago"}
	if _, err := mgr.Fetch(t.Context(), repo); err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if _, err := mgr.Fetch(t.Context(), repo); err != nil {
		t.Fatalf("the fetch: %v", err)
	}
	if srv.refused.Load() == 0 {
		t.Error("the token was never tried")
	}
}

// An ssh remote takes no header, so it is the machine's own however the work
// arrived. A host gets the token only in git config scoped to that host.
func TestTokenEnv(t *testing.T) {
	const token = "ghs_abc"
	if env, ok := tokenEnv(Repo{URL: "https://github.com/acme/app.git"}); ok || env != nil {
		t.Errorf("no token: %q", env)
	}
	for _, url := range []string{"git@github.com:acme/app.git", "ssh://git@github.com/acme/app.git", "/srv/app.git"} {
		if env, ok := tokenEnv(Repo{URL: url, Token: token}); ok || env != nil {
			t.Errorf("%s: %q", url, env)
		}
	}
	env, ok := tokenEnv(Repo{URL: "https://github.com/acme/app.git", Token: token})
	if !ok {
		t.Fatal("an https remote takes the token")
	}
	want := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + basicAuth(token),
	}
	if !slices.Equal(env, want) {
		t.Errorf("got %q", env)
	}
}
