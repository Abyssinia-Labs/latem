package main

import (
	"bytes"
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
