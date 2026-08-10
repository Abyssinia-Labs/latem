package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestExitCodes pins the contract in RUNNER_SPEC §7.3 and §21.
//
// CI scripts, install.sh, and a service manager's restart policy all branch on
// these numbers, so a code's meaning is as much an API as a flag name. The one
// that earns the table is 3: `latem run --wait` finding no agent is not the
// same event as a build that failed, and a caller has to be able to tell them
// apart without parsing English.
func TestExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		want  int
		usage bool
	}{
		{"success", nil, ExitOK, false},
		{"ordinary failure", errors.New("something broke"), ExitFailure, false},
		{"usage", usageErrorf("no such flag"), ExitUsage, true},
		{"agent down", errAgentDown("~/.latem/agent.sock"), ExitAgentDown, false},
		{"wrapped usage", errors.Join(usageErrorf("bad flag")), ExitUsage, true},
		{"explicit code", &ExitError{Code: ExitFailure, Err: errors.New("x")}, ExitFailure, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode = %d, want %d", got, tt.want)
			}
			if got := isUsage(tt.err); got != tt.usage {
				t.Errorf("isUsage = %v, want %v", got, tt.usage)
			}
		})
	}
}

func TestAgentDownErrorNamesTheCode(t *testing.T) {
	// A machine-readable spelling belongs in the message, because a script
	// that greps for it should not have to know the prose.
	err := errAgentDown("/home/t/.latem/agent.sock")
	if !strings.Contains(err.Error(), string(protocol.ErrAgentDown)) {
		t.Errorf("the error should name E_AGENT_DOWN: %v", err)
	}
	if !strings.Contains(err.Error(), "latem agent") {
		t.Errorf("the error should say how to start one: %v", err)
	}
}

func TestExitErrorUnwrap(t *testing.T) {
	inner := errors.New("inner")
	e := &ExitError{Code: ExitFailure, Err: inner}
	if !errors.Is(e, inner) {
		t.Error("ExitError should unwrap to its cause")
	}
	if got := (&ExitError{Code: 7}).Error(); got != "exit 7" {
		t.Errorf("Error() with no cause = %q", got)
	}
}

// TestEverySpecCommandExists checks the §19 subcommand set is present.
func TestEverySpecCommandExists(t *testing.T) {
	root := newRootCommand()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{
		"agent", "run", "list", "logs", "cancel", "approve", "register", "doctor", "cache", "version",
	} {
		if !have[want] {
			t.Errorf("§19 command %q is missing", want)
		}
	}
	// service is not in §19's listing but §23 requires the capability, and it
	// is the command install.sh points at.
	if !have["service"] {
		t.Error("service is missing")
	}
}

// TestHelpDocumentsExitCodes is a small guard on a large promise: the CLI's own
// help is where an operator learns what 3 means.
func TestHelpDocumentsExitCodes(t *testing.T) {
	root := newRootCommand()
	long := root.Long
	for _, want := range []string{"Exit codes:", "0  success", "2  usage", "3  no agent is listening"} {
		if !strings.Contains(long, want) {
			t.Errorf("root help does not document %q", want)
		}
	}
}

// TestEverySubcommandIsWired.
//
// This replaces TestUnwiredCommandsRefuseClearly, which held the rule for
// commands still owned by a workstream that had not landed: refuse, name the
// workstream, and never print a plausible-looking empty result somebody builds
// a script on. `register` was the last entry in that table and WS-14 wired it,
// so the table is empty and errNotWired has no callers.
//
// What is left is the regression guard: `register` must actually try to reach
// the control plane. A future refactor that stubbed it back out — or that made
// it write a token file before proving the URL — would produce a success, or a
// refusal, instead of a connection failure.
func TestEverySubcommandIsWired(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// A literal loopback address with nothing behind it: no DNS, no waiting.
	root.SetArgs([]string{"register", "--config-dir", dir, "--url", "ws://127.0.0.1:1/agent", "--token", "t"})

	err := root.Execute()
	if err == nil {
		t.Fatal("register exited 0 against a url nothing is listening on")
	}
	if strings.Contains(err.Error(), "not wired") {
		t.Errorf("register has been stubbed out again: %v", err)
	}
	if !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("register did not try to reach the control plane: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("a failed registration must say that nothing was written: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agent.key")); statErr == nil {
		t.Error("a failed registration wrote the machine token anyway")
	}
}

// TestWiredCommandsFailOnAMissingConfig is the other half of the rule above.
//
// Every command WS-11 wired now needs a config, and the failure it produces on
// a machine that has none has to be the config error — not a refusal naming a
// workstream, and emphatically not an empty list and exit 0. An operator who
// has not run `latem register` should be sent to `latem register`, and a
// script that greps for "not wired" should stop matching the moment the wiring
// lands.
func TestWiredCommandsFailOnAMissingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LATEM_CONFIG_DIR", filepath.Join(home, "nothing-here"))
	t.Cleanup(func() { global.configDir = "" })

	for _, tt := range []struct {
		name string
		args []string
	}{
		{"agent", []string{"agent"}},
		{"run", []string{"run", "myapp", "test"}},
		{"list", []string{"list"}},
		{"logs", []string{"logs", "run-1"}},
		{"cancel", []string{"cancel", "run-1"}},
		{"approve", []string{"approve", "run-1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tt.args)

			err := root.Execute()
			if err == nil {
				t.Fatalf("%v exited 0 with no config", tt.args)
			}
			if strings.Contains(err.Error(), "not wired up yet") {
				t.Errorf("%v still returns the unwired refusal: %v", tt.args, err)
			}
			if got := exitCode(err); got != ExitFailure {
				t.Errorf("exit code = %d, want %d (err %v)", got, ExitFailure, err)
			}
			if !strings.Contains(err.Error(), "config") {
				t.Errorf("the failure should name the config: %v", err)
			}
		})
	}
}

func TestArgumentValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"missing run arguments", []string{"run", "myapp"}, ExitUsage},
		{"too many run arguments", []string{"run", "a", "b", "c"}, ExitUsage},
		{"cancel without an id", []string{"cancel"}, ExitUsage},
		// Amendment A6: a run id becomes work/<id>, logs/<id>.log, and the
		// container name. A traversal here is a path traversal.
		{"cancel with a traversal id", []string{"cancel", "../../etc/passwd"}, ExitUsage},
		{"cancel with a dotted id", []string{"cancel", "a.b"}, ExitUsage},
		{"version takes no arguments", []string{"version", "extra"}, ExitUsage},
		{"unknown flag", []string{"doctor", "--nope"}, ExitUsage},
		{"unknown state", []string{"list", "--state", "wat"}, ExitUsage},
		{"malformed input", []string{"run", "a", "b", "--input", "novalue"}, ExitUsage},
		{"duplicate input", []string{"run", "a", "b", "--input", "k=1", "--input", "k=2"}, ExitUsage},
		{"register with no url", []string{"register", "--token", "t"}, ExitUsage},
		{"register with both token forms", []string{"register", "--url", "wss://x", "--token", "t", "--token-file", "f"}, ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newRootCommand()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(tt.args)
			err := root.Execute()
			if got := exitCode(err); got != tt.want {
				t.Errorf("%v exited %d, want %d (err %v)", tt.args, got, tt.want, err)
			}
		})
	}
}

func TestParseInputs(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[string]string
		wantErr bool
	}{
		{"none", nil, map[string]string{}, false},
		{"one", []string{"channel=beta"}, map[string]string{"channel": "beta"}, false},
		{"empty value is a value", []string{"channel="}, map[string]string{"channel": ""}, false},
		{"value containing =", []string{"expr=a=b"}, map[string]string{"expr": "a=b"}, false},
		{"whitespace around the key", []string{" channel =beta"}, map[string]string{"channel": "beta"}, false},

		{"no equals", []string{"channel"}, nil, true},
		{"empty key", []string{"=beta"}, nil, true},
		// Silently keeping the last one is how somebody spends an afternoon on
		// a build that used a value they thought they had overridden.
		{"duplicate", []string{"k=1", "k=2"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInputs(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseInputs(%v) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				if exitCode(err) != ExitUsage {
					t.Errorf("a malformed input should be a usage error, got exit %d", exitCode(err))
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseInputs(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("[%s] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	if strings.Join(got, "") != "abc" {
		t.Errorf("sortedKeys = %v, want [a b c]", got)
	}
}

func TestVersionCommand(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"version"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !strings.Contains(got, "latem ") {
			t.Errorf("version output missing the agent version:\n%s", got)
		}
		// The protocol version is what makes a refused connection diagnosable
		// rather than mysterious (amendment A4).
		if !strings.Contains(got, "protocol "+protocol.Version) {
			t.Errorf("version output missing the protocol version:\n%s", got)
		}
	})

	t.Run("short", func(t *testing.T) {
		root := newRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{"version", "--short"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != version {
			t.Errorf("--short = %q, want %q", out.String(), version)
		}
	})
}

func TestRunIDArgAcceptsLegalIDs(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	for _, id := range []string{"run-1", "R", "a_b-c", strings.Repeat("a", 64)} {
		if err := runIDArg(cmd, []string{id}); err != nil {
			t.Errorf("runIDArg(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", "-leading", "a/b", "..", "a.b", strings.Repeat("a", 65)} {
		if err := runIDArg(cmd, []string{id}); err == nil {
			t.Errorf("runIDArg(%q) accepted an illegal id", id)
		}
	}
}

func TestNoArgs(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	if err := noArgs(cmd, nil); err != nil {
		t.Errorf("noArgs with none = %v", err)
	}
	if err := noArgs(cmd, []string{"extra"}); exitCode(err) != ExitUsage {
		t.Errorf("noArgs with one = %v", err)
	}
}

func TestRangeArgs(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	v := rangeArgs(1, 2)
	if err := v(cmd, []string{"a"}); err != nil {
		t.Errorf("one arg = %v", err)
	}
	if err := v(cmd, []string{"a", "b"}); err != nil {
		t.Errorf("two args = %v", err)
	}
	if err := v(cmd, nil); exitCode(err) != ExitUsage {
		t.Errorf("zero args = %v", err)
	}
	if err := v(cmd, []string{"a", "b", "c"}); exitCode(err) != ExitUsage {
		t.Errorf("three args = %v", err)
	}
}

func TestBareCommandPrintsHelp(t *testing.T) {
	// A bare `latem` should say what to do next, not fail.
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(nil)
	if err := root.Execute(); err != nil {
		t.Fatalf("bare latem = %v", err)
	}
	if !strings.Contains(out.String(), "latem doctor") {
		t.Errorf("the bare invocation should point at doctor:\n%s", out.String())
	}
}

func TestConfigDirResolution(t *testing.T) {
	// The flag beats the environment variable, which beats ~/.latem — the
	// ordering people expect when they are debugging a second agent by hand.
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Cleanup(func() { global.configDir = "" })

	global.configDir = ""
	t.Setenv("LATEM_CONFIG_DIR", "")
	if got := configDir(); got != filepath.Join(home, ".latem") {
		t.Errorf("default = %q, want %s/.latem", got, home)
	}

	t.Setenv("LATEM_CONFIG_DIR", filepath.Join(home, "from-env"))
	if got := configDir(); got != filepath.Join(home, "from-env") {
		t.Errorf("env = %q", got)
	}

	global.configDir = filepath.Join(home, "from-flag")
	if got := configDir(); got != filepath.Join(home, "from-flag") {
		t.Errorf("flag = %q", got)
	}
	if got := configFile(); got != filepath.Join(home, "from-flag", "config.toml") {
		t.Errorf("configFile = %q", got)
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct{ in, want string }{
		{"~", home},
		{"~/.latem", filepath.Join(home, ".latem")},
		{"/etc/latem", "/etc/latem"},
		{"~other", "~other"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := expandHome(tt.in)
			if tt.in == "~other" {
				// Not a home reference; it becomes absolute against the cwd
				// rather than being rewritten to another user's home.
				if strings.HasPrefix(got, home) && got != filepath.Join(home, "other") {
					return
				}
				if !filepath.IsAbs(got) {
					t.Errorf("expandHome(%q) = %q, want an absolute path", tt.in, got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("expandHome(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		// -1 is "unknown", not zero. A non-local volume driver reports it, and
		// "unknown" and "empty" lead to opposite eviction decisions.
		{-1, "unknown"},
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 30, "1.0 GiB"},
		{90 << 30, "90.0 GiB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := formatBytes(tt.in); got != tt.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero", time.Time{}, "never"},
		{"seconds", now.Add(-10 * time.Second), "just now"},
		{"minutes", now.Add(-30 * time.Minute), "30m ago"},
		{"hours", now.Add(-5 * time.Hour), "5h ago"},
		{"days", now.Add(-72 * time.Hour), "3d ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatAge(tt.in); got != tt.want {
				t.Errorf("formatAge = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestYesNo(t *testing.T) {
	if yesNo(true) != "yes" || yesNo(false) != "no" {
		t.Error("yesNo is wrong")
	}
}

func TestPortOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"127.0.0.1:9838", "9838"},
		{"[::1]:9838", "9838"},
		{"nonsense", "9838"},
	}
	for _, tt := range tests {
		if got := portOf(tt.in); got != tt.want {
			t.Errorf("portOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUseColorOffForNonTerminal(t *testing.T) {
	if useColor(&bytes.Buffer{}) {
		t.Error("colour should be off for a buffer")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if useColor(f) {
		t.Error("colour should be off for a regular file")
	}

	t.Setenv("NO_COLOR", "1")
	if useColor(os.Stdout) {
		t.Error("NO_COLOR should disable colour")
	}
}
