package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
)

// baseConfig is a config.toml with one repo and one runner, the smallest file
// the onboarding commands edit rather than create.
const baseConfig = `schema_version = 1

# The operator's own note, which every edit must leave where it is.
[machine]
name = "test"

[repos.app]
url          = "https://github.com/me/app.git"
allowed_refs = ["main"]

[[runner]]
name     = "linux"
executor = "docker"
labels   = ["linux", "docker"]

  [runner.docker]
  default_image = "ghcr.io/me/ci:latest"
`

// init makes the seams that reach outside the test fail loudly by default. A
// test that wants one says so — fakeReload, fakeLsRemote, fakeStores — and a
// test that forgets cannot signal the machine's real agent, query a real
// remote, or read the Keychain of whoever is running the suite.
func init() {
	signalReload = func(config.Paths, time.Duration) (agent.ReloadStatus, error) {
		panic("a test reached the real agent.SignalReload; use fakeReload or noAgentRunning")
	}
	lsRemote = func(context.Context, string, bool) (string, error) {
		panic("a test reached a real git ls-remote; use fakeLsRemote")
	}
	newSecretStores = func(config.Paths) (secretStores, error) {
		panic("a test reached the real secret stores; use fakeStores")
	}
	originURL = func(context.Context, string) string {
		panic("a test reached a real git remote get-url; use fakeOrigin")
	}
	realReadRepo = readRepo
	readRepo = func(context.Context, *config.Config, string, treeSource) (*repoFiles, error) {
		panic("a test reached the real readRepo, which can clone from GitHub; use fakeRepoFiles")
	}
	// Nothing listens on port 1: a register that forgot --url fails at once,
	// here, instead of introducing itself to the production control plane.
	registerURL = "ws://127.0.0.1:1/agent"
	openBrowser = func(string) error { panic("a test tried to open a browser") }
	newSetupDeps = func() setupDeps {
		panic("a test reached setup's real service manager, Docker or doctor; use fakeSetup")
	}
}

// realReadRepo is readRepo as it ships, for the one test that reads a mirror
// that is not there.
var realReadRepo func(context.Context, *config.Config, string, treeSource) (*repoFiles, error)

// configDirWith is a 0700 config directory holding body as config.toml, or
// nothing when body is empty. It is under /tmp for the socket-length reason
// emptyConfigDir gives.
func configDirWith(t *testing.T, body string) string {
	t.Helper()
	dir := privateTempDir(t)
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// latemIn runs one latem command line against a config directory and returns
// everything it printed.
func latemIn(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append(append([]string{}, args...), "--config-dir", dir))
	err := root.Execute()
	return out.String(), err
}

// fakeReload replaces the SIGHUP path for one test and counts its calls. No
// test in this package may signal a real process: the machine running the
// tests may be running an agent of its own.
func fakeReload(t *testing.T, answer func(config.Paths) (agent.ReloadStatus, error)) *int {
	t.Helper()
	calls := 0
	prev := signalReload
	signalReload = func(p config.Paths, _ time.Duration) (agent.ReloadStatus, error) {
		calls++
		return answer(p)
	}
	t.Cleanup(func() { signalReload = prev })
	return &calls
}

// readConfigFile is config.toml in dir, as text.
func readConfigFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loadConfigIn loads dir's config through the real loader.
func loadConfigIn(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Options{Dir: dir})
	if err != nil {
		t.Fatalf("the config does not load: %v", err)
	}
	return cfg
}

// noAgentRunning is the fake for the common case: nothing to reload.
func noAgentRunning(t *testing.T) *int {
	return fakeReload(t, func(config.Paths) (agent.ReloadStatus, error) {
		return agent.ReloadStatus{}, agent.ErrNoAgent
	})
}
