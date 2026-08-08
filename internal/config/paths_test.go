package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §8's relocation promise: --config-dir or LATEM_CONFIG_DIR moves the lock,
// the socket, the sqlite database, the work directory, and the caches
// together. A test that only checked the config file path would pass while
// two agents quietly shared a work directory.
func TestPaths_RelocateTogether(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	alt := filepath.Join(home, "second")
	if err := os.MkdirAll(alt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alt, FileName), []byte(minimalValid), 0o600); err != nil {
		t.Fatal(err)
	}

	sources := []struct {
		name string
		opts Options
	}{
		{
			name: "--config-dir",
			opts: Options{Home: home, Dir: alt, Getenv: func(string) string { return "" }, LookupIP: testLookupIP},
		},
		{
			name: EnvConfigDir,
			opts: Options{Home: home, Getenv: func(k string) string {
				if k == EnvConfigDir {
					return alt
				}
				return ""
			}, LookupIP: testLookupIP},
		},
	}

	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			cfg, err := Load(src.opts)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			paths := map[string]string{
				"Dir":        cfg.Paths.Dir,
				"File":       cfg.Paths.File,
				"Lock":       cfg.Paths.Lock,
				"Socket":     cfg.Paths.Socket,
				"DB":         cfg.Paths.DB,
				"Work":       cfg.Paths.Work,
				"Caches":     cfg.Paths.Caches,
				"Mirrors":    cfg.Paths.Mirrors,
				"Logs":       cfg.Paths.Logs,
				"Artifacts":  cfg.Paths.Artifacts,
				"SecretsEnv": cfg.Paths.SecretsEnv,
				"TokenFile":  cfg.Paths.TokenFile,
			}
			for name, p := range paths {
				if !within(p, alt) {
					t.Errorf("Paths.%s = %s, which is outside the relocated config dir %s", name, p, alt)
				}
			}

			if cfg.Agent.WorkDir != cfg.Paths.Work {
				t.Errorf("agent.work_dir (%s) and Paths.Work (%s) disagree", cfg.Agent.WorkDir, cfg.Paths.Work)
			}
			if got, want := cfg.Paths.RunnerCacheDir("linux"), filepath.Join(alt, cachesName, "linux"); got != want {
				t.Errorf("RunnerCacheDir = %s, want %s", got, want)
			}
			if got, want := cfg.Paths.MirrorDir("app"), filepath.Join(alt, mirrorsName, "app.git"); got != want {
				t.Errorf("MirrorDir = %s, want %s", got, want)
			}
		})
	}
}

// --config-dir must beat LATEM_CONFIG_DIR. When someone is debugging a second
// agent by hand, the flag they just typed is the one they mean.
func TestPaths_FlagBeatsEnvironment(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	flagDir := filepath.Join(home, "flag")
	if err := os.MkdirAll(flagDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flagDir, FileName), []byte(minimalValid), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(Options{
		Home: home,
		Dir:  flagDir,
		Getenv: func(k string) string {
			if k == EnvConfigDir {
				return filepath.Join(home, "env")
			}
			return ""
		},
		LookupIP: testLookupIP,
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Paths.Dir != flagDir {
		t.Errorf("Paths.Dir = %s, want the --config-dir value %s", cfg.Paths.Dir, flagDir)
	}
}

// The failure this guards against is mundane and total: someone copies a
// config.toml — including the `work_dir = "~/.latem/work"` §8's own example
// shows — and points a second agent at it with --config-dir. Without this
// check, nothing complains and two agents share a work directory, a socket,
// and a token.
func TestPaths_RelocationGuardCatchesCopiedConfig(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	alt := filepath.Join(home, "second")
	if err := os.MkdirAll(alt, 0o700); err != nil {
		t.Fatal(err)
	}
	body := minimalValid + `
[agent]
work_dir = "~/.latem/work"

[broker.local]
socket = "~/.latem/agent.sock"
`
	if err := os.WriteFile(filepath.Join(alt, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Load(Options{Home: home, Dir: alt, Getenv: func(string) string { return "" }, LookupIP: testLookupIP})
	if err == nil {
		t.Fatal("a relocated agent pointing at the default config dir's state loaded successfully")
	}
	msg := err.Error()
	for _, want := range []string{"agent.work_dir", "broker.local.socket", "corrupt each other"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error omits %q:\n%v", want, msg)
		}
	}
}

// The same paths under the default config dir are fine — that is the spec's
// own example, and it must keep working.
func TestPaths_DefaultDirAcceptsExplicitTildePaths(t *testing.T) {
	f := newFixture(t, minimalValid+`
[agent]
work_dir = "~/.latem/work"

[broker.local]
socket = "~/.latem/agent.sock"
`)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join(f.Dir, workName); cfg.Paths.Work != want {
		t.Errorf("Paths.Work = %s, want %s", cfg.Paths.Work, want)
	}
}

// A relative path follows the config directory rather than the process working
// directory, which is the only reading that keeps the relocation promise.
func TestPaths_RelativeOverrideResolvesAgainstConfigDir(t *testing.T) {
	f := newFixture(t, minimalValid+`
[agent]
work_dir = "scratch"
`)
	cfg, err := Load(f.Opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join(f.Dir, "scratch"); cfg.Paths.Work != want {
		t.Errorf("Paths.Work = %s, want %s", cfg.Paths.Work, want)
	}
}

func TestPaths_TildeUserIsRejected(t *testing.T) {
	f := newFixture(t, minimalValid+`
[agent]
work_dir = "~build/work"
`)
	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("~user expanded silently")
	}
	if !strings.Contains(err.Error(), "~user expansion is not supported") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestExpandPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		bad  bool
	}{
		{in: "", want: ""},
		{in: "~", want: "/home/me"},
		{in: "~/x/y", want: "/home/me/x/y"},
		{in: "/abs", want: "/abs"},
		{in: "rel/path", want: "rel/path"},
		{in: "~other/x", bad: true},
	}
	for _, tt := range tests {
		got, err := expandPath(tt.in, "/home/me")
		if tt.bad {
			if err == nil {
				t.Errorf("expandPath(%q) should have failed", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("expandPath(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("expandPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A relocated config directory a few levels deep produces a socket path the
// kernel rejects with a bare "invalid argument", which is not a message anyone
// traces back to the length of a directory name.
func TestPaths_SocketPathLengthCap(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	deep := filepath.Join(home, strings.Repeat("a", 60), strings.Repeat("b", 60))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, FileName), []byte(minimalValid), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Load(Options{Home: home, Dir: deep, Getenv: func(string) string { return "" }, LookupIP: testLookupIP})
	if err == nil {
		t.Fatal("a socket path past the kernel cap loaded successfully")
	}
	if !strings.Contains(err.Error(), "Unix socket path") {
		t.Errorf("error does not explain the cap: %v", err)
	}
}

// §8: "A world-readable config directory holding secrets.env and a machine
// token is the whole threat model failing quietly."
func TestLoad_ModeChecks(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f fixture)
		want  string
	}{
		{
			name: "group-readable config directory",
			setup: func(t *testing.T, f fixture) {
				if err := os.Chmod(f.Dir, 0o750); err != nil {
					t.Fatal(err)
				}
			},
			want: "must not be reachable by group or other",
		},
		{
			name: "config directory the agent cannot enter",
			setup: func(t *testing.T, f fixture) {
				if err := os.Chmod(f.Dir, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "cannot enter and write its own state directory",
		},
		{
			name: "world-readable config file",
			setup: func(t *testing.T, f fixture) {
				if err := os.Chmod(filepath.Join(f.Dir, FileName), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "config.toml",
		},
		{
			name: "world-readable secrets.env",
			setup: func(t *testing.T, f fixture) {
				f.writeFile(t, secretsEnvName, 0o644)
			},
			want: "secrets.env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, minimalValid)
			tt.setup(t, f)
			// Restore so the temp dir can be cleaned up.
			t.Cleanup(func() { _ = os.Chmod(f.Dir, 0o700) })

			_, err := Load(f.Opts)
			if err == nil {
				t.Fatal("loaded successfully")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("want substring %q, got: %v", tt.want, err)
			}
			if !strings.Contains(err.Error(), "chmod") {
				t.Errorf("a mode error must print the command that fixes it: %v", err)
			}
		})
	}
}

// A 0400 token file is stricter than 0600 and must pass; the rule is "nothing
// for group or other", not "exactly 0600".
func TestLoad_TokenFileModes(t *testing.T) {
	remote := `
[broker.remote]
enabled = true
url = "wss://api.example.com/agent"
`
	t.Run("read-only token is accepted", func(t *testing.T) {
		f := newFixture(t, minimalValid+remote)
		f.writeFile(t, tokenName, 0o400)
		if _, err := Load(f.Opts); err != nil {
			t.Fatalf("a 0400 token file was rejected: %v", err)
		}
	})

	t.Run("group-readable token is rejected", func(t *testing.T) {
		f := newFixture(t, minimalValid+remote)
		f.writeFile(t, tokenName, 0o640)
		_, err := Load(f.Opts)
		if err == nil || !strings.Contains(err.Error(), "token_file") {
			t.Fatalf("a group-readable machine token was accepted: %v", err)
		}
	})

	t.Run("missing token is rejected only when the remote broker is on", func(t *testing.T) {
		f := newFixture(t, minimalValid+remote)
		_, err := Load(f.Opts)
		if err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("want a missing-token error, got: %v", err)
		}

		// The same config with the remote broker off must load: an
		// unregistered machine still runs local builds (§7.3).
		g := newFixture(t, minimalValid)
		if _, err := Load(g.Opts); err != nil {
			t.Fatalf("an unregistered machine cannot start: %v", err)
		}
	})
}

// §12: across filesystems `git clone --local` silently stops hardlinking and
// every run gets slower in a way nobody attributes to the right cause.
func TestCheckSameFilesystem(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	mirrors := filepath.Join(home, mirrorsName)
	work := filepath.Join(home, workName)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatal(err)
	}

	// Only one directory exists: nothing to compare, and a fresh install must
	// not fail here.
	if err := checkSameFilesystem(mirrors, work); err != nil {
		t.Errorf("comparison against a missing directory reported a problem: %v", err)
	}

	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkSameFilesystem(mirrors, work); err != nil {
		t.Errorf("two directories on one filesystem were reported as split: %v", err)
	}

	// A path that is not a directory at all still must not fail the load.
	notADir := filepath.Join(home, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSameFilesystem(mirrors, notADir); err != nil {
		t.Errorf("unexpected problem: %v", err)
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/a/b/c", "/a", true},
		{"/ab", "/a", false},
		{"/b", "/a", false},
		{"/a/../b", "/a", false},
	}
	for _, tt := range tests {
		if got := within(tt.path, tt.dir); got != tt.want {
			t.Errorf("within(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

// Paths is derived, never decoded. If the toml:"-" tag stopped being honoured,
// a config file could silently override the socket or the work directory
// through a [paths] table and bypass the relocation guard entirely.
func TestPaths_AreNotDecodable(t *testing.T) {
	f := newFixture(t, minimalValid+`
[paths]
socket = "/tmp/anywhere.sock"
`)
	_, err := Load(f.Opts)
	if err == nil {
		t.Fatal("a [paths] table was accepted")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("want an unknown-key error, got: %v", err)
	}
}

// The zero Options must work: real environment, real home, real resolver. Every
// other test injects all three, so without this the production path is untested.
func TestLoad_ZeroOptionsUsesTheRealEnvironment(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	dir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(minimalValid), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfigDir, dir)

	cfg, err := Load(Options{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Paths.Dir != dir {
		t.Errorf("Paths.Dir = %s, want %s", cfg.Paths.Dir, dir)
	}
}

func TestLoad_MissingConfigDirectory(t *testing.T) {
	home, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	_, err = Load(Options{
		Home:     home,
		Dir:      filepath.Join(home, "absent"),
		Getenv:   func(string) string { return "" },
		LookupIP: testLookupIP,
	})
	if err == nil {
		t.Fatal("loaded from a directory that does not exist")
	}
	if !strings.Contains(err.Error(), "mkdir -p") {
		t.Errorf("a missing config directory should print the command that creates it: %v", err)
	}
}

// ws:// is permitted to loopback because the control plane is frequently run
// locally during development and there is no path to tap.
func TestValidateBrokerURL_LoopbackMayBePlaintext(t *testing.T) {
	for _, raw := range []string{
		"ws://localhost:3210/agent",
		"ws://127.0.0.1:3210/agent",
		"ws://[::1]:3210/agent",
		"wss://api.example.com/agent",
	} {
		if err := validateBrokerURL(raw); err != nil {
			t.Errorf("validateBrokerURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{
		"ws://api.example.com/agent",
		"ws://192.168.1.20:3210/agent",
		"://nope",
	} {
		if err := validateBrokerURL(raw); err == nil {
			t.Errorf("validateBrokerURL(%q) accepted a URL it should not have", raw)
		}
	}
}
