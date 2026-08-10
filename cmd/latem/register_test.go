package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// A control plane just conformant enough to register against: it checks the
// bearer token, upgrades, and sends a hello carrying the machine id it assigned.
type registrar struct {
	srv       *httptest.Server
	token     string
	machineID string
	message   string
	// seen is the Authorization header of the last handshake, so a test can
	// prove the token actually travelled.
	seen string
}

func newRegistrar(t *testing.T, token, machineID string) *registrar {
	t.Helper()
	r := &registrar{token: token, machineID: machineID}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.seen = req.Header.Get("Authorization")
		if r.token != "" && r.seen != "Bearer "+r.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		body := `{"kind":"hello","protocol_version":"1.0","hello":{"machine_id":"` + r.machineID + `"`
		if r.message != "" {
			body += `,"message":"` + r.message + `"`
		}
		body += `}}`
		_ = conn.Write(req.Context(), websocket.MessageText, []byte(body))

		// Read once so the agent's resume frame does not race the close.
		_, _, _ = conn.Read(req.Context())
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// registerIn runs `latem register` against a config directory and returns its
// output.
func registerIn(t *testing.T, dir string, stdin string, args ...string) (string, error) {
	t.Helper()

	// --config-dir is a persistent flag on the root, and `global` is package
	// state, so it is reset for each run rather than leaking into the next test.
	prev := global.configDir
	t.Cleanup(func() { global.configDir = prev })

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"register", "--config-dir", dir}, args...))
	err := root.Execute()
	return out.String(), err
}

// TestRegister_ProvesTheControlPlaneBeforeItWritesAnything.
//
// This is the ordering the command exists for, and the TODO it replaced said so
// in as many words: a config.toml naming a control plane that rejected us is
// worse than no config at all, because the agent then spends its life
// reconnecting somewhere it is not welcome and there is nothing obvious to undo.
func TestRegister_ProvesTheControlPlaneBeforeItWritesAnything(t *testing.T) {
	t.Run("the control plane refuses the token", func(t *testing.T) {
		dir := privateTempDir(t)
		reg := newRegistrar(t, "right-token", "mch_1")

		out, err := registerIn(t, dir, "", "--url", reg.srv.URL, "--token", "wrong-token")
		if err == nil {
			t.Fatal("register succeeded against a control plane that refused the token")
		}
		if !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("the error should say nothing was written: %v", err)
		}
		assertNothingWritten(t, dir)
		if strings.Contains(out+err.Error(), "wrong-token") {
			t.Error("the token appears in the command's output")
		}
	})

	t.Run("the control plane is unreachable", func(t *testing.T) {
		dir := privateTempDir(t)
		_, err := registerIn(t, dir, "", "--url", "ws://127.0.0.1:1/agent", "--token", "tok")
		if err == nil {
			t.Fatal("register succeeded against a url nothing is listening on")
		}
		assertNothingWritten(t, dir)
	})
}

// TestRegister_WritesTheTokenTheConfigAndNothingElse.
func TestRegister_WritesTheTokenTheConfigAndNothingElse(t *testing.T) {
	dir := privateTempDir(t)
	reg := newRegistrar(t, "tok-live", "mch_01HXYZ")
	reg.message = "welcome to the fleet"

	out, err := registerIn(t, dir, "", "--url", reg.srv.URL, "--token", "tok-live",
		"--name", "mac-mini", "--label", "macos", "--label", "arm64")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	// The token travelled as a bearer credential and landed at 0600.
	if reg.seen != "Bearer tok-live" {
		t.Errorf("the control plane saw Authorization %q", reg.seen)
	}
	keyPath := filepath.Join(dir, "agent.key")
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("agent.key: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("agent.key is mode %04o, want 0600", perm)
	}

	// And it is nowhere in what the operator saw, nor in config.toml — which is
	// the file people paste into issues, and the whole reason §8 keeps the token
	// out of it.
	if strings.Contains(out, "tok-live") {
		t.Errorf("the token appears in the command's output:\n%s", out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml: %v", err)
	}
	if strings.Contains(string(body), "tok-live") {
		t.Errorf("the token appears in config.toml:\n%s", body)
	}
	if !strings.Contains(out, "mch_01HXYZ") {
		t.Errorf("the assigned machine id was not reported:\n%s", out)
	}
	if !strings.Contains(out, "welcome to the fleet") {
		t.Errorf("the control plane's message was not shown:\n%s", out)
	}

	// A machine with no config.toml gets one that is deliberately incomplete:
	// §8 requires a [[runner]] and a [repos.*] and this command cannot know what
	// this machine builds. Refusing to write anything would leave a new user
	// with a verified token, a proven URL, and nothing on disk. So it is written
	// with a warning that names the gap and points at doctor.
	if !strings.Contains(out, "latem doctor") {
		t.Errorf("an incomplete config was written without saying what is still missing:\n%s", out)
	}
	if !strings.Contains(out, "[[runner]]") {
		t.Errorf("the warning does not name the missing runner block:\n%s", out)
	}

	// And once the operator supplies those two things, the file loads and
	// carries everything register put in it.
	complete := string(body) + `
[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "linux-x64"
executor    = "docker"
labels      = ["linux"]
concurrency = 1
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(complete), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Dir: dir, Home: dir})
	if err != nil {
		t.Fatalf("the config register wrote does not load once a runner is added: %v\n%s", err, complete)
	}
	if !cfg.Broker.Remote.Enabled || cfg.Broker.Remote.URL != wsURL(reg.srv.URL) {
		t.Errorf("broker.remote = %+v, want url %s", cfg.Broker.Remote, wsURL(reg.srv.URL))
	}
	if cfg.Machine.ID != "mch_01HXYZ" {
		t.Errorf("machine.id = %q", cfg.Machine.ID)
	}
	if cfg.Machine.Name != "mac-mini" {
		t.Errorf("machine.name = %q", cfg.Machine.Name)
	}
	if strings.Join(cfg.Machine.Labels, ",") != "macos,arm64" {
		t.Errorf("machine.labels = %v", cfg.Machine.Labels)
	}
}

// wsURL is what `latem register` stores for an http:// endpoint: a control
// plane's dashboard shows https://, people paste that, and wss:// is the
// spelling of the same endpoint internal/config accepts.
func wsURL(httpURL string) string {
	return strings.Replace(strings.Replace(httpURL, "https://", "wss://", 1), "http://", "ws://", 1)
}

// TestRegister_PreservesAnExistingConfig.
//
// config.toml is a file people write by hand, with their comments and their
// ordering in it. Re-encoding it through a struct would destroy all of that, and
// the first anyone would know is the next time they went looking for the note
// they left themselves about why a runner has a strange label.
func TestRegister_PreservesAnExistingConfig(t *testing.T) {
	dir := privateTempDir(t)
	existing := `schema_version = 1

[machine]
name = "old-name"
id   = "mch_old"

[agent]
# 4am builds need a long grace: the archive step alone is 25 minutes.
shutdown_grace = "45m"

[broker.local]
enabled = true

[broker.remote]
enabled    = false
url        = "wss://old.example.com/agent"
token_file = "/nowhere/agent.key"

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "linux-x64"
executor    = "docker"
labels      = ["linux", "amd64"]
concurrency = 2

  [runner.docker]
  default_image = "ghcr.io/me/ci-base:latest"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := newRegistrar(t, "tok", "mch_new")
	if out, err := registerIn(t, dir, "", "--url", reg.srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	body, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	for _, keep := range []string{
		"# 4am builds need a long grace",
		`shutdown_grace = "45m"`,
		`default_image = "ghcr.io/me/ci-base:latest"`,
		`name        = "linux-x64"`,
		"[repos.app]",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("register destroyed %q:\n%s", keep, got)
		}
	}
	if strings.Contains(got, "old.example.com") {
		t.Errorf("the old broker.remote survived:\n%s", got)
	}
	if strings.Count(got, "[broker.remote]") != 1 {
		t.Errorf("there are %d [broker.remote] tables:\n%s", strings.Count(got, "[broker.remote]"), got)
	}
	if strings.Count(got, "[machine]") != 1 {
		t.Errorf("there are %d [machine] tables:\n%s", strings.Count(got, "[machine]"), got)
	}

	cfg, err := config.Load(config.Options{Dir: dir, Home: dir})
	if err != nil {
		t.Fatalf("the edited config does not load: %v\n%s", err, got)
	}
	if cfg.Machine.ID != "mch_new" {
		t.Errorf("machine.id = %q, want the newly assigned one", cfg.Machine.ID)
	}
	if cfg.Machine.Name != "old-name" {
		t.Errorf("machine.name = %q; register was not asked to rename the machine", cfg.Machine.Name)
	}
	if !cfg.Broker.Remote.Enabled || cfg.Broker.Remote.URL != wsURL(reg.srv.URL) {
		t.Errorf("broker.remote = %+v, want url %s", cfg.Broker.Remote, wsURL(reg.srv.URL))
	}
	if len(cfg.Runners) != 1 || cfg.Runners[0].Concurrency != 2 {
		t.Errorf("the runner block did not survive: %+v", cfg.Runners)
	}
}

// TestRegister_ReadsTheTokenFromStdinAndFromAFile. §8 and the command's own
// help push people away from --token, so the alternatives have to work.
func TestRegister_ReadsTheTokenFromStdinAndFromAFile(t *testing.T) {
	t.Run("stdin", func(t *testing.T) {
		dir := privateTempDir(t)
		reg := newRegistrar(t, "piped-token", "mch_p")
		if out, err := registerIn(t, dir, "piped-token\n", "--url", reg.srv.URL, "--token-file", "-"); err != nil {
			t.Fatalf("register: %v\n%s", err, out)
		}
		if reg.seen != "Bearer piped-token" {
			t.Errorf("the control plane saw %q", reg.seen)
		}
	})

	t.Run("a file", func(t *testing.T) {
		dir := privateTempDir(t)
		src := filepath.Join(privateTempDir(t), "issued.key")
		if err := os.WriteFile(src, []byte("filed-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		reg := newRegistrar(t, "filed-token", "mch_f")
		if out, err := registerIn(t, dir, "", "--url", reg.srv.URL, "--token-file", src); err != nil {
			t.Fatalf("register: %v\n%s", err, out)
		}
		if reg.seen != "Bearer filed-token" {
			t.Errorf("the control plane saw %q", reg.seen)
		}
	})

	t.Run("a world-readable file is refused before anything is written", func(t *testing.T) {
		dir := privateTempDir(t)
		src := filepath.Join(privateTempDir(t), "issued.key")
		if err := os.WriteFile(src, []byte("leaky-token\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		reg := newRegistrar(t, "leaky-token", "mch_l")
		_, err := registerIn(t, dir, "", "--url", reg.srv.URL, "--token-file", src)
		if err == nil {
			t.Fatal("register accepted a token file readable by everybody")
		}
		assertNothingWritten(t, dir)
	})
}

// TestRegister_UsageErrorsExitTwo. The flags and exit codes were declared final
// before the wiring existed, so they are pinned here.
func TestRegister_UsageErrorsExitTwo(t *testing.T) {
	tests := map[string][]string{
		"no url":                {"--token", "x"},
		"no token":              {"--url", "wss://x/agent"},
		"both token forms":      {"--url", "wss://x/agent", "--token", "x", "--token-file", "y"},
		"a positional argument": {"--url", "wss://x/agent", "--token", "x", "stray"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			dir := privateTempDir(t)
			_, err := registerIn(t, dir, "", args...)
			if got := exitCode(err); got != ExitUsage {
				t.Errorf("exit = %d, want %d (err %v)", got, ExitUsage, err)
			}
			assertNothingWritten(t, dir)
		})
	}
}

// TestApplyRegistration_EditsOnlyTheTablesItOwns is the unit-level half.
//
// The table editor is the risky part of this command — it rewrites somebody's
// config — so the cases that would corrupt a file are enumerated rather than
// left to the end-to-end test to stumble over.
func TestApplyRegistration_EditsOnlyTheTablesItOwns(t *testing.T) {
	r := registration{URL: "wss://cp/agent", TokenFile: "/h/.latem/agent.key", MachineID: "mch_x"}

	tests := map[string]struct {
		in       string
		contains []string
		absent   []string
	}{
		"an empty file becomes a whole config": {
			in:       "   \n",
			contains: []string{"schema_version = 1", "[broker.remote]", "[broker.local]", `id     = "mch_x"`},
		},
		"an indented subtable is not mistaken for the end of a table": {
			in: "[broker.remote]\nenabled = false\n\n[[runner]]\nname = \"r\"\n\n  [runner.docker]\n  default_image = \"x\"\n",
			contains: []string{
				`default_image = "x"`,
				`[[runner]]`,
				`url        = "wss://cp/agent"`,
			},
			absent: []string{"enabled = false"},
		},
		"a missing broker.remote is appended": {
			in:       "schema_version = 1\n\n[broker.local]\nenabled = true\n",
			contains: []string{"[broker.local]", "[broker.remote]"},
		},
		"a commented key is not mistaken for an assignment": {
			in:       "[machine]\n# id = \"do not use this\"\nname = \"keep\"\n",
			contains: []string{`# id = "do not use this"`, `name = "keep"`, `id = "mch_x"`},
		},
		"a trailing broker.remote at end of file is replaced whole": {
			in:       "schema_version = 1\n\n[broker.remote]\nenabled = false\nurl = \"wss://old/agent\"\n",
			contains: []string{`url        = "wss://cp/agent"`},
			absent:   []string{"wss://old/agent", "enabled = false"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := applyRegistration(tc.in, r)
			if err != nil {
				t.Fatalf("applyRegistration: %v", err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("result is missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("result still contains %q:\n%s", bad, got)
				}
			}
		})
	}
}

// TestWriteRegistration_RefusesToInstallAConfigThatWouldNotLoad.
//
// The safety net, and the rule it actually enforces: this command must never
// make a *loadable* config unloadable. A config that already did not load is a
// different case — see the incomplete-file path above — so the original here is
// a complete, valid one, and the edit is a URL internal/config rejects.
func TestWriteRegistration_RefusesToInstallAConfigThatWouldNotLoad(t *testing.T) {
	dir := privateTempDir(t)
	paths := config.DerivePaths(dir)

	original := `schema_version = 1

[broker.local]
enabled = true

[repos.app]
url          = "git@github.com:me/app.git"
allowed_refs = ["main"]

[[runner]]
name        = "linux-x64"
executor    = "docker"
labels      = ["linux"]
concurrency = 1
`
	if err := os.WriteFile(paths.File, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// Plaintext ws:// to a host that is not loopback: the machine token would
	// ride in a header readable by anyone on the path, and the loader refuses it.
	_, err := writeRegistration(paths, registration{
		URL:       "ws://cp.example.com/agent",
		TokenFile: paths.TokenFile,
	})
	if err == nil {
		t.Fatal("writeRegistration installed a config that will not load")
	}
	if !strings.Contains(err.Error(), "[broker.remote]") {
		t.Errorf("the error should print the block to add by hand: %v", err)
	}

	got, rerr := os.ReadFile(paths.File)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != original {
		t.Errorf("the original config was modified:\n%s", got)
	}
	// And no temporary file was left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the config directory holds %d entries after a refused write", len(entries))
	}
}

// TestWriteRegistration_InstallsIntoAConfigThatAlreadyDidNotLoad.
//
// The other side of the same rule. A machine whose config.toml is already
// broken is exactly the machine somebody is trying to fix, and refusing to
// register it would mean the operator has to repair the file before they are
// allowed to do the thing they came to do.
func TestWriteRegistration_InstallsIntoAConfigThatAlreadyDidNotLoad(t *testing.T) {
	dir := privateTempDir(t)
	paths := config.DerivePaths(dir)

	original := "schema_version = 1\n\n[broker.local]\nenabled = true\n"
	if err := os.WriteFile(paths.File, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	warning, err := writeRegistration(paths, registration{
		URL:       "wss://cp.example.com/agent",
		TokenFile: paths.TokenFile,
	})
	if err != nil {
		t.Fatalf("writeRegistration refused a config that was already broken: %v", err)
	}
	if warning == "" {
		t.Error("no warning was returned for a config that still does not load")
	}
	body, rerr := os.ReadFile(paths.File)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "wss://cp.example.com/agent") {
		t.Errorf("the registration was not written:\n%s", body)
	}
}

// assertNothingWritten is the invariant every failure path in this command
// shares.
func assertNothingWritten(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"agent.key", "config.toml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written despite the registration failing", name)
		}
	}
}

// privateTempDir is a 0700 directory, which is what internal/config requires of
// a config directory before it will read anything in it.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "latreg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
