package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// likeMiniConfig is a machine that builds what setupRepoFiles asks for: the
// action allowed, the label aliased, and tags accepted. Its runners,
// repositories and control plane are its own, and some of its paths do not
// exist here.
const likeMiniConfig = `[machine]
name = "mini"
id = "m-123"

[broker.remote]
enabled = true
url = "wss://gw.latem.dev/agent"
token_file = "/Users/someone-else/.latem/token"

[actions]
allowed = ["oven-sh/setup-bun", "dorny/paths-filter"]
allow_unisolated = true
require_pinned = false

[label_aliases]
"blacksmith-4vcpu-ubuntu-2404" = ["linux"]
"macos-15" = ["macos"]

[repos.app]
url = "https://github.com/me/app.git"
allowed_refs = ["main"]
`

func writeLikeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(privateTempDir(t), "mini.toml")
	if err := os.WriteFile(path, []byte(likeMiniConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSSHConfig answers ssh with body, or with refusal on stderr when it is
// set, and records each argv.
func fakeSSHConfig(t *testing.T, body, refusal string) *[][]string {
	t.Helper()
	var calls [][]string
	prev := runSSH
	runSSH = func(_ context.Context, argv []string, _ io.Reader, stdout, stderr io.Writer) error {
		calls = append(calls, argv)
		if refusal != "" {
			_, _ = io.WriteString(stderr, refusal)
			return errExit255
		}
		_, _ = io.WriteString(stdout, body)
		return nil
	}
	t.Cleanup(func() { runSSH = prev })
	return &calls
}

var errExit255 = &ExitError{Code: 255}

// Off a terminal, --like writes the other machine's allowlist and nothing a
// person has to agree to: the aliases and the two settings are named, with
// the line that asks for them at a terminal (LAT-25).
func TestSetupLike_OffATerminalWritesTheAllowlistAndLeavesTheRest(t *testing.T) {
	fakeSetup(t, true)
	fakeLsRemote(t, readable("main"))
	fakeStores(t, false)
	noAgentRunning(t)
	fakeRepoFiles(t, setupRepoFiles)
	dir := filepath.Join(privateTempDir(t), "fresh")
	like := writeLikeFile(t)

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--like", like, "--repo", "me/app")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	t.Logf("setup said:\n%s", out)
	cfg := loadConfigIn(t, dir)
	if !slices.Equal(cfg.Actions.Allowed, []string{"oven-sh/setup-bun", "dorny/paths-filter"}) {
		t.Errorf("allowed = %v", cfg.Actions.Allowed)
	}
	if len(cfg.LabelAliases) != 0 || cfg.Actions.AllowUnisolated || !cfg.Actions.PinningRequired() {
		t.Errorf("aliases %v, allow_unisolated %v, pinning required %v: written with nobody asked",
			cfg.LabelAliases, cfg.Actions.AllowUnisolated, cfg.Actions.PinningRequired())
	}
	if cfg.Machine.Name == "mini" || cfg.Broker.Remote.Enabled || len(cfg.Repos) != 1 {
		t.Errorf("took more than --like copies: machine %q, remote %v, repos %v", cfg.Machine.Name, cfg.Broker.Remote.Enabled, cfg.Repos)
	}
	for _, want := range []string{
		"like " + like + ": its config.toml, read from " + like,
		"[actions] allowed: added oven-sh/setup-bun, dorny/paths-filter",
		"left for a person at a terminal, because neither stdin nor stdout is one",
		`[label_aliases] blacksmith-4vcpu-ubuntu-2404 = ["linux"]`,
		`[label_aliases] macos-15 = ["macos"]`,
		"[actions] require_pinned = false",
		"[actions] allow_unisolated = true",
		"latem setup --like " + like,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if next := out[strings.LastIndex(out, "next"):]; !strings.Contains(next, "latem setup --like") {
		t.Errorf("next does not say what is left:\n%s", next)
	}

	// Again: nothing is written twice, and the same is still left.
	before := readConfigFile(t, dir)
	out, err = latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--like", like)
	if err != nil {
		t.Fatalf("setup again: %v\n%s", err, out)
	}
	if readConfigFile(t, dir) != before || !strings.Contains(out, "has all 2 of") {
		t.Errorf("a second --like changed the file, or did not say it had all:\n%s", out)
	}
}

// A host is read over ssh, with no latem on the other side: its
// config.toml, from where its latem reads it by default.
func TestSetupLike_ReadsAHostOverSSH(t *testing.T) {
	fakeSetup(t, true)
	noAgentRunning(t)
	calls := fakeSSHConfig(t, likeMiniConfig, "")
	dir := filepath.Join(privateTempDir(t), "fresh")

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--like", "mini")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if len(*calls) != 1 {
		t.Fatalf("ssh ran %d times", len(*calls))
	}
	argv := (*calls)[0]
	if i := slices.Index(argv, "--"); i < 0 || argv[i+1] != "mini" ||
		argv[i+2] != `cat "${LATEM_CONFIG_DIR:-$HOME/.latem}/config.toml"` {
		t.Errorf("argv = %q", argv)
	}
	if !strings.Contains(out, "like mini: its config.toml, read over ssh") ||
		!slices.Contains(loadConfigIn(t, dir).Actions.Allowed, "dorny/paths-filter") {
		t.Errorf("output:\n%s", out)
	}
}

// A machine that cannot be read stops setup before it writes anything.
func TestSetupLike_AMachineItCannotReadStopsSetupFirst(t *testing.T) {
	rec := fakeSetup(t, true)
	fakeSSHConfig(t, "", "ssh: Could not resolve hostname studio: nodename nor servname provided")
	dir := filepath.Join(privateTempDir(t), "fresh")

	_, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--like", "studio")
	if err == nil || !strings.Contains(err.Error(), "Could not resolve hostname studio") ||
		!strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "--like ./studio.toml") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(statErr) || rec.doctors != 0 {
		t.Errorf("setup went on: config %v, doctor ran %d times", statErr, rec.doctors)
	}

	fakeSSHConfig(t, "[actions\nallowed = 7", "")
	if _, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--like", "studio"); err == nil ||
		!strings.Contains(err.Error(), "its config.toml does not read") {
		t.Errorf("a config that does not parse: %v", err)
	}

	if _, err := latemIn(t, dir, "", "setup", "--json", "--like", "studio"); exitCode(err) != ExitUsage {
		t.Errorf("--json --like: exit %d (%v), want usage", exitCode(err), err)
	}
}

// At a terminal the person is asked once about the aliases and once about
// each setting, each shown as it will be written and with no as the
// default, and what they say no to is left out. The repository's job, which
// needs all three of the other machine's entries, can then run here.
func TestSetupLike_AtATerminalAsksForTheAliasesAndEachSetting(t *testing.T) {
	fakeSetup(t, true)
	fakeLsRemote(t, readable("main"))
	fakeStores(t, false)
	noAgentRunning(t)
	fakeRepoFiles(t, setupRepoFiles)
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })
	dir := filepath.Join(privateTempDir(t), "fresh")
	like := writeLikeFile(t)
	const asked = "Write this to config.toml? [y/N]"

	// aliases yes, require_pinned yes, allow_unisolated no.
	screen, stderr, err := atTerminal(t, terminalSession{prompt: asked, answer: "y", then: []string{"y", ""}},
		"setup", "--plain", "--no-register", "--no-service", "--like", like, "--repo", "me/app", "--config-dir", dir)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, screen, stderr)
	}
	t.Logf("screen:\n%s", screen)
	cfg := loadConfigIn(t, dir)
	if !slices.Equal(cfg.LabelAliases["blacksmith-4vcpu-ubuntu-2404"], []string{"linux"}) || len(cfg.LabelAliases) != 2 {
		t.Errorf("aliases = %v", cfg.LabelAliases)
	}
	if cfg.Actions.PinningRequired() || cfg.Actions.AllowUnisolated {
		t.Errorf("pinning required %v, allow_unisolated %v: want the yes written and the no left out",
			cfg.Actions.PinningRequired(), cfg.Actions.AllowUnisolated)
	}
	if n := strings.Count(screen, asked); n != 3 {
		t.Errorf("asked %d times, want 3", n)
	}
	for _, want := range []string{"An alias sends this machine every job", "A tag can be moved", "They run as you",
		"added 2 label aliases", "wrote [actions] require_pinned = false", "left [actions] allow_unisolated = true out",
		"1 of 1 jobs"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q", want)
		}
	}
	if strings.Contains(screen, "left for a person") {
		t.Error("said something was left for a person, at a terminal")
	}
}
