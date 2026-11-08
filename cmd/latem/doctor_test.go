package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// TestDoctorNamesEveryFaultOnABrokenMachine is the acceptance criterion from
// The acceptance criterion: doctor on a deliberately broken machine names
// every fault and
// its fix, and exits non-zero.
func TestDoctorNamesEveryFaultOnABrokenMachine(t *testing.T) {
	dir := privateDir(t)
	cfgErr := errors.New("config /x/config.toml has 1 problem:\n  - no [[runner]] blocks")

	// Nothing on PATH, nothing answering, no config. `defaults` answers the
	// way macOS does for an unset key, so auto-login reads as "not
	// configured" rather than "could not be determined".
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{
		"defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser": {exit: 1},
	}, nil, cfgErr)
	d.sys = newSystemForTest("darwin", dir, map[string]fakeCmd{
		"defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser": {exit: 1},
	}, "defaults")
	d.newDocker = (&fakeDocker{newErr: errors.New("cannot reach the daemon")}).factory()

	rep := d.run(context.Background())
	_, _, failed, _ := rep.counts()
	if failed == 0 {
		t.Fatal("a machine with nothing installed and no config produced no failures")
	}

	for _, id := range []string{"git.present", "config.problem.1"} {
		c, ok := findCheck(rep, id)
		if !ok {
			t.Errorf("check %s did not run", id)
			continue
		}
		if c.Level != levelFail {
			t.Errorf("%s = %v, want fail", id, c.Level)
		}
	}

	// auto-login is named but is deliberately not a failure: it costs the
	// machine its reboot, not its build. It sits with FileVault, which has
	// always been a warning for the same property.
	if c, ok := findCheck(rep, "macos.autologin"); !ok {
		t.Error("check macos.autologin did not run")
	} else if c.Level != levelWarn {
		t.Errorf("macos.autologin = %v, want warn", c.Level)
	}

	// Every failure must carry a repair — a command in Fix or a step in Manual
	// — or a check has told the operator that something is wrong and not what
	// to do, which RUNNER_SPEC §19 says is the entire point of the subcommand.
	// Either counts: auto-login is repaired in System Settings and there is no
	// command for it, which is exactly why Manual exists. The config problems
	// are the one exception: internal/config writes the remediation into its
	// own message, and restating it here in different words would let the two
	// drift.
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			if c.Level != levelFail || strings.HasPrefix(c.ID, "config.problem.") {
				continue
			}
			if len(c.Fix) == 0 && len(c.Manual) == 0 {
				t.Errorf("failing check %q (%s) carries no repair", c.ID, c.Detail)
			}
		}
	}

	// A repair must be visible in the default output, not only under --fix.
	// Splitting Manual out of Fix made auto-login — the check that fails most
	// often on a fresh Mac, and the one with no command — print no repair at
	// all in the view an operator actually reads.
	{
		var out strings.Builder
		printReport(&out, rep, false, false)
		text := out.String()
		for _, s := range rep.Sections {
			for _, c := range s.Checks {
				if c.Level != levelFail || len(c.Manual) == 0 {
					continue
				}
				for _, m := range c.Manual {
					if !strings.Contains(text, m) {
						t.Errorf("check %q names a by-hand repair that plain `doctor` never prints: %q", c.ID, m)
					}
				}
			}
		}
	}

	// And nothing in Fix may be prose: --fix prints that list under a heading
	// saying "review before running", so every line in it has to be something a
	// shell accepts. A line with no command-shaped first word is one an
	// operator pastes and watches fail (F6).
	for _, f := range rep.fixes() {
		first, _, _ := strings.Cut(strings.TrimPrefix(f, "# "), " ")
		if strings.ContainsAny(first, "→…") || first == "install" || first == "or" {
			t.Errorf("fix %q is prose, not a command; it belongs in Manual", f)
		}
	}
}

func TestDoctorHealthyMachinePasses(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			dir := privateDir(t)
			cfg := testConfig(dir)
			// The repo probe would otherwise reach the network.
			cmds := healthyRunner(goos)
			cmds["git ls-remote --heads --end-of-options https://example.invalid/app.git"] = fakeCmd{out: "abc\trefs/heads/main"}
			d := newTestDoctor(goos, dir, cmds, cfg, nil)
			d.newDocker = (&fakeDocker{report: okImageReport(cfg.Runners[0].Docker.DefaultImage)}).factory()

			rep := d.run(context.Background())
			_, _, failed, _ := rep.counts()
			if failed != 0 {
				for _, s := range rep.Sections {
					for _, c := range s.Checks {
						if c.Level == levelFail {
							t.Errorf("unexpected failure %s: %s", c.ID, c.Detail)
						}
					}
				}
			}
		})
	}
}

func TestDoctorGitTooOldIsAFailure(t *testing.T) {
	// Amendment A7: --end-of-options landed in git 2.24, and INV-3's
	// argument-injection defence depends on it.
	tests := []struct {
		name    string
		banner  string
		present bool
		want    level
	}{
		{"new enough", "git version 2.50.1", true, levelPass},
		{"exactly the floor", "git version 2.24.0", true, levelPass},
		{"one release too old", "git version 2.23.0", true, levelFail},
		{"absent", "", false, levelFail},
		{"unreadable banner", "git, the stupid content tracker", true, levelWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := privateDir(t)
			cmds := map[string]fakeCmd{}
			path := []string{}
			if tt.present {
				cmds["git --version"] = fakeCmd{out: tt.banner}
				path = append(path, "git")
			}
			d := newTestDoctor("linux", dir, cmds, nil, errors.New("no config"))
			d.sys = newSystemForTest("linux", dir, cmds, path...)

			got := d.checkGit(context.Background())
			if got.Level != tt.want {
				t.Errorf("git check = %v, want %v (detail %q)", got.Level, tt.want, got.Detail)
			}
			if tt.want == levelFail && len(got.Fix) == 0 {
				t.Error("a failing git check must name the install command")
			}
		})
	}
}

func TestDoctorFileVaultNamesBothOptions(t *testing.T) {
	// Finding F13. Both answers are legitimate — a machine holding signing
	// certificates is a reasonable thing to want encrypted — so doctor must
	// present the trade rather than pick a side.
	dir := privateDir(t)
	cmds := healthyRunner("darwin")
	cmds["fdesetup status"] = fakeCmd{out: "FileVault is On."}
	d := newTestDoctor("darwin", dir, cmds, nil, errors.New("no config"))

	got := d.checkFileVault(context.Background())
	if got.Level != levelWarn {
		t.Fatalf("FileVault on = %v, want warn (it is a decision, not a fault)", got.Level)
	}
	joined := strings.Join(append(got.Fix, got.Notes...), "\n")
	for _, want := range []string{"fdesetup authrestart", "fdesetup disable", "pre-boot"} {
		if !strings.Contains(joined, want) {
			t.Errorf("FileVault advice does not mention %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "operator's decision") {
		t.Error("FileVault advice should say the trade-off is the operator's")
	}

	cmds["fdesetup status"] = fakeCmd{out: "FileVault is Off."}
	d = newTestDoctor("darwin", dir, cmds, nil, errors.New("no config"))
	if got := d.checkFileVault(context.Background()); got.Level != levelPass {
		t.Errorf("FileVault off = %v, want pass", got.Level)
	}
}

func TestDoctorLaunchDaemonIsAFailure(t *testing.T) {
	// §23 states this first among its three requirements: codesign needs a
	// user session, and a daemon context fails with errors that point nowhere.
	dir := privateDir(t)
	cmds := healthyRunner("darwin")
	cmds["launchctl managername"] = fakeCmd{out: "System"}
	d := newTestDoctor("darwin", dir, cmds, nil, errors.New("no config"))

	got := d.checkLaunchContext(context.Background())
	if got.Level != levelFail {
		t.Fatalf("a System context = %v, want fail", got.Level)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "codesign") {
		t.Error("the explanation should name codesign, which is what actually breaks")
	}
}

func TestDoctorAutoLoginUserMismatch(t *testing.T) {
	// This looks like a working configuration and is not: the LaunchAgent
	// belongs to a user who is never logged in.
	dir := privateDir(t)
	cmds := healthyRunner("darwin")
	cmds["defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser"] = fakeCmd{out: "someone-else"}
	d := newTestDoctor("darwin", dir, cmds, nil, errors.New("no config"))

	got := d.checkAutoLogin(context.Background())
	if got.Level != levelWarn {
		t.Fatalf("auto-login as another user = %v, want warn", got.Level)
	}
	if !strings.Contains(got.Detail, "someone-else") || !strings.Contains(got.Detail, "tester") {
		t.Errorf("the detail should name both users: %q", got.Detail)
	}
}

func TestDoctorBaseImageContract(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	image := cfg.Runners[0].Docker.DefaultImage

	t.Run("satisfied", func(t *testing.T) {
		d := newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)
		d.newDocker = (&fakeDocker{report: okImageReport(image)}).factory()
		got := d.checkDockerImages(context.Background())
		if len(got) != 1 || got[0].Level != levelPass {
			t.Fatalf("a conforming image = %+v, want one pass", got)
		}
	})

	t.Run("violated", func(t *testing.T) {
		d := newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)
		d.newDocker = (&fakeDocker{report: brokenImageReport(image)}).factory()
		got := d.checkDockerImages(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("a non-conforming image = %+v, want one failure", got)
		}
		notes := strings.Join(got[0].Notes, "\n")
		// Each individual violation is named, not just "the image is wrong".
		for _, want := range []string{"user-1000", "tool-git"} {
			if !strings.Contains(notes, want) {
				t.Errorf("notes do not name the %s check:\n%s", want, notes)
			}
		}
		if len(got[0].Fix) == 0 {
			t.Error("a failing image check must say how to build a conforming one")
		}
	})

	t.Run("daemon unreachable", func(t *testing.T) {
		d := newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)
		d.newDocker = (&fakeDocker{reportErr: errors.New("daemon gone")}).factory()
		got := d.checkDockerImages(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("an unreachable daemon = %+v, want one failure", got)
		}
	})
}

func TestDoctorDockerRequiredOnlyWhenConfigured(t *testing.T) {
	dir := privateDir(t)

	// No docker runner: a missing docker is a warning, not a fault. A doctor
	// that fails for things the operator deliberately does not have is a
	// doctor people stop running.
	noDocker := testConfig(dir)
	noDocker.Runners = nil
	d := newTestDoctor("linux", dir, map[string]fakeCmd{"git --version": {out: "git version 2.50.1"}}, noDocker, nil)
	d.sys = newSystemForTest("linux", dir, nil, "git")
	got := d.checkDocker(context.Background())
	if len(got) != 1 || got[0].Level != levelWarn {
		t.Fatalf("missing docker with no docker runner = %+v, want one warning", got)
	}

	// With a docker runner configured it is a hard failure.
	d = newTestDoctor("linux", dir, nil, testConfig(dir), nil)
	d.sys = newSystemForTest("linux", dir, nil, "git")
	got = d.checkDocker(context.Background())
	if len(got) != 1 || got[0].Level != levelFail {
		t.Fatalf("missing docker with a docker runner = %+v, want one failure", got)
	}
}

func TestDoctorModes(t *testing.T) {
	// §23: ~/.latem is 0700 and the token file is 0600. The agent refuses to
	// start otherwise, so doctor should say so before the operator discovers
	// it from a service that will not come up.
	dir := privateDir(t)
	cfg := testConfig(dir)
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("config.toml", 0o600)
	write("agent.key", 0o644) // world-readable machine token

	d := newTestDoctor("linux", dir, nil, cfg, nil)
	checks := d.checkModes()

	byID := map[string]check{}
	for _, c := range checks {
		byID[c.ID] = c
	}
	if got := byID["modes.dir"]; got.Level != levelPass {
		t.Errorf("a 0700 config dir = %v: %s", got.Level, got.Detail)
	}
	token := byID["modes.token"]
	if token.Level != levelFail {
		t.Errorf("a 0644 token file = %v, want fail", token.Level)
	}
	if len(token.Fix) == 0 || !strings.HasPrefix(token.Fix[0], "chmod 0600 ") {
		t.Errorf("the fix should be a chmod an operator can paste: %v", token.Fix)
	}
}

func TestDoctorMetricsAddrLoopback(t *testing.T) {
	// INV-1's second line. internal/config rejects a non-loopback value at
	// load; this is what catches one that reached a Config another way.
	dir := privateDir(t)
	tests := []struct {
		name string
		addr string
		want level
	}{
		{"loopback", "127.0.0.1:9838", levelPass},
		{"disabled", "", levelPass},
		{"all interfaces", "0.0.0.0:9838", levelFail},
		{"routable", "192.168.1.5:9838", levelFail},
		{"bare port", ":9838", levelFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(dir)
			cfg.Agent.MetricsAddr = tt.addr
			d := newTestDoctor("linux", dir, nil, cfg, nil)
			got := d.checkMetricsAddr()
			if got.Level != tt.want {
				t.Errorf("metrics_addr %q = %v, want %v (%s)", tt.addr, got.Level, tt.want, got.Detail)
			}
			if tt.want == levelFail && !strings.Contains(strings.Join(got.Notes, " "), "INV-1") {
				t.Error("the failure should name the invariant it protects")
			}
		})
	}
}

func TestDoctorSameFilesystem(t *testing.T) {
	// Finding F2. Across filesystems `git clone --local` silently copies the
	// object store instead of hardlinking it, and there is nothing to point at
	// except a machine that got slower.
	dir := privateDir(t)
	cfg := testConfig(dir)
	d := newTestDoctor("linux", dir, nil, cfg, nil)

	if got := d.checkSameFilesystem(); got.Level != levelPass {
		t.Errorf("same filesystem = %v", got.Level)
	}

	d.sameFS = func(string, string) error { return errors.New("not on one filesystem") }
	got := d.checkSameFilesystem()
	if got.Level != levelFail {
		t.Fatalf("different filesystems = %v, want fail", got.Level)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "hardlink") {
		t.Error("the explanation should say what stops working")
	}
	if len(got.Fix) == 0 {
		t.Error("the failure must name the config key to change")
	}
}

func TestDoctorDiskFloors(t *testing.T) {
	// §14.4: the floor is per runner, not global. A tart runner's 150 GB and a
	// docker runner's 40 GB are different questions about the same disk.
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners = append(cfg.Runners, &config.Runner{
		Name: "ios", Executor: config.ExecutorTart, Concurrency: 1,
		Tart: &config.TartRunner{BaseImage: "latem-xcode-26", MinFreeGB: 1e9},
	})
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)

	rep := d.checkDisk(context.Background())
	var sawTartFailure bool
	for _, c := range rep {
		if c.ID == "disk.free.ios" {
			sawTartFailure = c.Level == levelFail
			if !strings.Contains(c.Detail, "tart") {
				t.Errorf("the tart runner's check should name its executor: %q", c.Detail)
			}
		}
		if c.ID == "disk.free.linux-x64" && c.Level == levelFail {
			t.Errorf("the docker runner's 40 GB floor should pass here: %s", c.Detail)
		}
	}
	if !sawTartFailure {
		t.Error("a 1 EB floor should fail; the per-runner floor is not being applied")
	}
}

func TestDoctorSecretsNamesOnly(t *testing.T) {
	// INV-4: names only, never values. A diagnostic that prints a signing key
	// to a terminal has leaked it into a scrollback buffer and a support
	// ticket.
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Secrets.Commands = map[string]string{
		"MATCH_PASSWORD": "op read op://Private/match/password",
		"SHORT_ONE":      "echo dev",
		"MISSING_ONE":    "op read op://Private/nope",
		"BROKEN_ONE":     "op read op://Private/broken",
	}
	d := newTestDoctor("linux", dir, nil, cfg, nil)
	d.includeSecrets = true
	d.newSecrets = (&fakeSecrets{
		values: map[string]string{
			"MATCH_PASSWORD": "a-long-enough-secret-value",
			"SHORT_ONE":      "dev",
		},
		errs: map[string]error{
			"BROKEN_ONE": errors.New("op: you are not currently signed in"),
		},
	}).factory()

	got := d.checkSecrets(context.Background())
	byID := map[string]check{}
	for _, c := range got {
		byID[c.ID] = c
	}

	if c := byID["secret.MATCH_PASSWORD"]; c.Level != levelPass {
		t.Errorf("a resolvable secret = %v: %s", c.Level, c.Detail)
	}
	if c := byID["secret.SHORT_ONE"]; c.Level != levelWarn {
		t.Errorf("a secret shorter than %d chars = %v, want warn", logstream.MinRedactableLength, c.Level)
	}
	if c := byID["secret.MISSING_ONE"]; c.Level != levelFail {
		t.Errorf("an unresolvable secret = %v, want fail", c.Level)
	}
	if c := byID["secret.BROKEN_ONE"]; c.Level != levelFail {
		t.Errorf("a backend error = %v, want fail", c.Level)
	}

	// No value, of any secret, may appear anywhere in the rendered output.
	var rep report
	rep.section("Secrets", got...)
	var buf strings.Builder
	printReport(&buf, rep, true, false)
	for _, value := range []string{"a-long-enough-secret-value", "dev"} {
		if strings.Contains(buf.String(), value) {
			t.Errorf("secret value %q reached the report:\n%s", value, buf.String())
		}
	}
}

func TestDoctorSecretsOnlyWhenAsked(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Secrets.Commands = map[string]string{"X": "true"}
	d := newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)

	rep := d.run(context.Background())
	for _, s := range rep.Sections {
		if s.Name == "Secrets" {
			t.Error("secrets were resolved without --secrets")
		}
	}
}

func TestKnownSecretNames(t *testing.T) {
	write := func(t *testing.T, body string) *doctor {
		t.Helper()
		dir := privateDir(t)
		cfg := testConfig(dir)
		if body != "" {
			if err := os.WriteFile(cfg.Paths.SecretsEnv, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)
	}

	t.Run("names from the file and from [secrets.commands]", func(t *testing.T) {
		d := write(t, "# a comment\n\nAPI_TOKEN=super-secret\nOTHER=value\n")
		d.cfg.Secrets.Commands = map[string]string{"FROM_COMMAND": "echo x"}
		got, err := d.knownSecretNames()
		if err != nil {
			t.Fatalf("a well-formed file should not error: %v", err)
		}
		want := []string{"API_TOKEN", "FROM_COMMAND", "OTHER"}
		if !slices.Equal(got, want) {
			t.Errorf("names = %v, want %v", got, want)
		}
	})

	// The whole of F15: a quoted multi-line value's interior lines contain "="
	// and were reported as secret names, printing a PEM's own base64 to the
	// terminal under a flag that promises "names only, never values".
	t.Run("a file that does not parse yields an error and no names", func(t *testing.T) {
		d := write(t, "TEST_SECRET=abcdef123456\nTEST_PEM=\"-----BEGIN PRIVATE KEY-----\n"+
			"QQQQINTERIORLINETWOQQQQ=\n-----END PRIVATE KEY-----\"\n")
		got, err := d.knownSecretNames()
		if err == nil {
			t.Fatal("an unterminated quote should be reported, not parsed around")
		}
		if len(got) != 0 {
			t.Errorf("names = %v, want none from a file that does not parse", got)
		}
		if strings.Contains(err.Error(), "QQQQINTERIORLINETWOQQQQ") {
			t.Errorf("the error carries part of a value: %q", err)
		}
	})

	t.Run("an absent file is not an error", func(t *testing.T) {
		d := write(t, "")
		d.cfg.Secrets.Commands = map[string]string{"ONLY_COMMAND": "echo x"}
		got, err := d.knownSecretNames()
		if err != nil {
			t.Fatalf("an absent secrets.env should not be an error: %v", err)
		}
		if !slices.Equal(got, []string{"ONLY_COMMAND"}) {
			t.Errorf("names = %v", got)
		}
	})
}

func TestDoctorRepoReachability(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	const probe = "git ls-remote --heads --end-of-options https://example.invalid/app.git"

	t.Run("reachable", func(t *testing.T) {
		cmds := healthyRunner("linux")
		cmds[probe] = fakeCmd{out: "abc\trefs/heads/main"}
		d := newTestDoctor("linux", dir, cmds, cfg, nil)
		got := d.checkRepos(context.Background())
		if len(got) != 1 || got[0].Level != levelPass {
			t.Fatalf("a reachable repo = %+v", got)
		}
	})

	// A Mac over ssh, with gh's login in a keychain the session cannot open.
	// git says what it says on a machine with no login, and the fix for that
	// one, gh auth login, can leave a token in a plain file on this one.
	t.Run("a login out of this session's reach", func(t *testing.T) {
		const noAgent = "env -u SSH_AUTH_SOCK " + probe
		const noLogin = "fatal: could not read Username for 'https://example.invalid': terminal prompts disabled"
		for name, tt := range map[string]struct {
			goos, session string
			want          level
		}{
			"over ssh":          {"darwin", "Background", levelWarn},
			"at the Mac itself": {"darwin", "Aqua", levelFail},
			"a daemon":          {"darwin", "System", levelFail},
			"on Linux":          {"linux", "", levelFail},
		} {
			t.Run(name, func(t *testing.T) {
				cmds := healthyRunner(tt.goos)
				cmds[noAgent] = fakeCmd{exit: 128, out: noLogin}
				cmds[probe] = fakeCmd{exit: 128, out: noLogin}
				if tt.session != "" {
					cmds["launchctl managername"] = fakeCmd{out: tt.session}
				}
				d := newTestDoctor(tt.goos, dir, cmds, cfg, nil)

				got := d.checkRepos(context.Background())
				if len(got) != 1 || got[0].Level != tt.want {
					t.Fatalf("got %+v, want level %v", got, tt.want)
				}
				if tt.want != levelWarn {
					return
				}
				if len(got[0].Fix) != 0 {
					t.Errorf("a login that may be fine should get no fix: %q", got[0].Fix)
				}
				if !strings.Contains(strings.Join(got[0].Manual, " "), "on the Mac itself") {
					t.Errorf("should send the operator to the Mac: %q", got[0].Manual)
				}
			})
		}
	})

	// The failure a plain `git ls-remote` cannot see, because the operator's
	// own shell is the thing hiding it. `gh auth login --git-protocol ssh`
	// produces exactly this state: a passphrase-protected key held by an
	// interactive ssh-agent, which launchd does not inherit. Before the probe
	// dropped SSH_AUTH_SOCK, doctor reported this machine as healthy and the
	// first real run failed E_SOURCE.
	t.Run("readable only with the operator's ssh agent", func(t *testing.T) {
		const noAgent = "env -u SSH_AUTH_SOCK " + probe
		cmds := healthyRunner("linux")
		cmds[noAgent] = fakeCmd{exit: 128, out: "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository."}
		cmds[probe] = fakeCmd{out: "abc\trefs/heads/main"}
		d := newTestDoctor("linux", dir, cmds, cfg, nil)

		got := d.checkRepos(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("a repo readable only from the shell = %+v, want a failure", got)
		}
		if !strings.Contains(got[0].Detail, "not from a service") {
			t.Errorf("the detail should name the service, not the credential: %q", got[0].Detail)
		}
		notes := strings.Join(got[0].Notes, " ")
		if !strings.Contains(notes, "SSH_AUTH_SOCK") {
			t.Errorf("the note should name the variable that differs: %q", notes)
		}
		// The remedy must be usable with no terminal. Suggesting
		// `gh auth login --git-protocol ssh` here is what produced the state.
		fixes := strings.Join(got[0].Fix, " ")
		if strings.Contains(fixes, "--git-protocol ssh") {
			t.Errorf("the fix must not be the command that causes this: %q", fixes)
		}
		if !strings.Contains(fixes, "-N ''") && !strings.Contains(fixes, "apple-use-keychain") {
			t.Errorf("the fix should offer a credential a service can use: %q", fixes)
		}
	})

	// A42/A37: the publickey remedy is the highest-traffic fix string in
	// doctor, and it used to name the one command the README documents as the
	// trap. A comment did not keep it right; this does.
	t.Run("the publickey remedy is not the trap", func(t *testing.T) {
		_, fixes := repoFailureAdvice(
			"git@github.com:me/app.git",
			"git@github.com: Permission denied (publickey).",
		)
		joined := strings.Join(fixes, " ")
		if strings.Contains(joined, "--git-protocol ssh") {
			t.Errorf("doctor must not recommend the passphrase-protected form: %q", joined)
		}
		if !strings.Contains(joined, "--git-protocol https") {
			t.Errorf("the shortest working route should be offered: %q", joined)
		}
	})

	t.Run("credentials rejected", func(t *testing.T) {
		cmds := healthyRunner("linux")
		cmds[probe] = fakeCmd{exit: 128, out: "remote: Repository not found.\nfatal: could not read"}
		d := newTestDoctor("linux", dir, cmds, cfg, nil)
		got := d.checkRepos(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("an unreachable repo = %+v, want a failure", got)
		}
		if !strings.Contains(got[0].Detail, "Repository not found") {
			t.Errorf("the detail should carry git's own words: %q", got[0].Detail)
		}
		// And NOT the ssh-agent paragraph, which is the wrong answer here: the
		// credential worked. This assertion used to require it.
		if strings.Contains(strings.Join(got[0].Notes, " "), "ssh agent") {
			t.Errorf("a repository that answered should not be explained as an ssh-agent problem: %q", got[0].Notes)
		}
	})

	// The line ssh writes on success is the first line of stderr on a machine
	// seeing a host for the first time, which is precisely the machine this
	// check exists for. Reporting it as the reason sent pass 1 of the M7
	// acceptance test looking at launchd for a key that did not exist.
	t.Run("ssh's known-hosts warning is not the error", func(t *testing.T) {
		cmds := healthyRunner("linux")
		cmds[probe] = fakeCmd{exit: 128, out: "Warning: Permanently added 'example.invalid' (ED25519) to the list of known hosts.\r\n" +
			"git@example.invalid: Permission denied (publickey).\n" +
			"fatal: Could not read from remote repository."}
		d := newTestDoctor("linux", dir, cmds, cfg, nil)
		got := d.checkRepos(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("an unreachable repo = %+v, want a failure", got)
		}
		if strings.Contains(got[0].Detail, "Permanently added") {
			t.Errorf("ssh's success warning was reported as the failure: %q", got[0].Detail)
		}
		if !strings.Contains(got[0].Detail, "Permission denied (publickey)") {
			t.Errorf("the detail should name the real cause: %q", got[0].Detail)
		}
		// And the repair should be a repair, not another diagnostic.
		if !strings.Contains(strings.Join(got[0].Fix, " "), "gh auth login") {
			t.Errorf("a missing key should be answered with how to install one: %q", got[0].Fix)
		}
		if !strings.Contains(strings.Join(got[0].Notes, " "), "no ssh key") {
			t.Errorf("the note should say the machine has no key: %q", got[0].Notes)
		}
	})

	t.Run("each cause gets its own explanation", func(t *testing.T) {
		tests := []struct {
			name, out, wantNote string
		}{
			{"host key", "Host key verification failed.", "known_hosts"},
			{"dns", "ssh: Could not resolve hostname nope.invalid", "Name resolution"},
			{"https auth", "fatal: Authentication failed for 'https://example.invalid/app.git'", "helper"},
			{"network", "ssh: connect to host example.invalid port 22: Connection timed out", "firewall"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				cmds := healthyRunner("linux")
				cmds[probe] = fakeCmd{exit: 128, out: tc.out}
				d := newTestDoctor("linux", dir, cmds, cfg, nil)
				got := d.checkRepos(context.Background())
				if len(got) != 1 || got[0].Level != levelFail {
					t.Fatalf("got %+v, want a failure", got)
				}
				if !strings.Contains(strings.Join(got[0].Notes, " "), tc.wantNote) {
					t.Errorf("notes = %q, want something about %q", got[0].Notes, tc.wantNote)
				}
			})
		}
	})

	t.Run("no repos configured", func(t *testing.T) {
		empty := testConfig(dir)
		empty.Repos = nil
		d := newTestDoctor("linux", dir, healthyRunner("linux"), empty, nil)
		got := d.checkRepos(context.Background())
		if len(got) != 1 || got[0].Level != levelWarn {
			t.Fatalf("no repos = %+v, want one warning", got)
		}
	})
}

func TestDoctorTartImage(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{{
		Name: "ios", Executor: config.ExecutorTart, Concurrency: 1,
		Tart: &config.TartRunner{BaseImage: "latem-xcode-26", MinFreeGB: 150},
	}}

	t.Run("not built", func(t *testing.T) {
		d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)
		got := d.checkTartImages(context.Background())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("an unbuilt image = %+v, want a failure", got)
		}
	})

	t.Run("built and measured", func(t *testing.T) {
		vm := filepath.Join(dir, ".tart", "vms", "latem-xcode-26")
		if err := os.MkdirAll(vm, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vm, "disk.img"), make([]byte, 64<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)
		got := d.checkTartImages(context.Background())
		if len(got) != 1 || got[0].Level != levelPass {
			t.Fatalf("a built image within the floor = %+v, want a pass", got)
		}
		// §13.2: doctor reports the image's real size rather than assuming.
		if !strings.Contains(got[0].Detail, "occupies") {
			t.Errorf("the detail should report a measured size: %q", got[0].Detail)
		}
	})
}

func TestDoctorTartRequiredButAbsent(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{{
		Name: "ios", Executor: config.ExecutorTart, Concurrency: 1,
		Tart: &config.TartRunner{BaseImage: "img"},
	}}
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{}, cfg, nil)
	d.sys = newSystemForTest("darwin", dir, nil) // nothing on PATH

	got := d.checkTart(context.Background())
	if got.Level != levelFail {
		t.Fatalf("a tart runner with no tart = %v, want fail", got.Level)
	}
	if len(got.Fix) == 0 || !strings.Contains(got.Fix[0], "brew install") {
		t.Errorf("the fix should be the install command: %v", got.Fix)
	}

	// On Linux a tart runner is a configuration that cannot ever work.
	d = newTestDoctor("linux", dir, nil, cfg, nil)
	if got := d.checkTart(context.Background()); got.Level != levelFail {
		t.Errorf("a tart runner on linux = %v, want fail", got.Level)
	}
}

// The default macOS deployment since amendment A31: a native runner, and no
// tart anywhere. This is the branch every stock install takes, and until A31 it
// was the only one with no test — it warned, describing the shipped default as
// a fallback that "has no isolation".
//
// A warning here is worse than merely wrong. `doctor` earns its exit code by
// only speaking when something is actionable, and a line that fires on every
// correctly-configured Mac is how an operator learns to skim past the ones that
// matter. Whether native is right for a given machine is §13.0's question about
// who can influence its refs, which is not knowable from the machine.
func TestDoctorTartAbsentAndUnusedIsNotAWarning(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{{
		Name: "ios", Executor: config.ExecutorNative, Concurrency: 1,
		Native: &config.NativeRunner{MinFreeGB: 60},
	}}
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{}, cfg, nil)
	d.sys = newSystemForTest("darwin", dir, nil) // nothing on PATH

	got := d.checkTart(context.Background())
	if got.Level != levelSkip {
		t.Fatalf("tart absent with no tart runner = %v, want skip", got.Level)
	}
	if len(got.Fix) != 0 {
		t.Errorf("nothing is broken, so there is nothing to fix: %v", got.Fix)
	}
}

func TestDoctorClockSkew(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)

	t.Run("no remote broker", func(t *testing.T) {
		d := newTestDoctor("linux", dir, nil, cfg, nil)
		got := d.checkBroker(context.Background())
		if len(got) != 1 || got[0].Level != levelSkip {
			t.Fatalf("no remote broker = %+v, want a skip", got)
		}
	})

	t.Run("unreachable broker warns rather than fails", func(t *testing.T) {
		// A control plane that is down is not this machine's fault, and a
		// doctor that fails for it cannot be used as an install gate offline.
		withBroker := testConfig(dir)
		withBroker.Broker.Remote = config.RemoteBroker{Enabled: true, URL: "wss://127.0.0.1:1/agent"}
		d := newTestDoctor("linux", dir, nil, withBroker, nil)
		got := d.checkBroker(context.Background())
		if len(got) != 1 || got[0].Level != levelWarn {
			t.Fatalf("an unreachable broker = %+v, want a warning", got)
		}
	})
}

func TestDoctorLinuxLingerAndDockerGroup(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cmds := healthyRunner("linux")
	cmds["id -nG"] = fakeCmd{out: "tester sudo"}                                    // not in docker
	cmds["loginctl show-user tester --property=Linger"] = fakeCmd{out: "Linger=no"} // no linger
	d := newTestDoctor("linux", dir, cmds, cfg, nil)

	group := d.checkDocker(context.Background())
	var groupCheck check
	for _, c := range group {
		if c.ID == "docker.group" {
			groupCheck = c
		}
	}
	if groupCheck.Level != levelFail {
		t.Errorf("a user outside the docker group = %v, want fail", groupCheck.Level)
	}
	if !strings.Contains(strings.Join(groupCheck.Fix, " "), "usermod -aG docker tester") {
		t.Errorf("the fix should name the user: %v", groupCheck.Fix)
	}

	svc := d.checkService(context.Background())
	var linger check
	for _, c := range svc {
		if c.ID == "service.linger" {
			linger = c
		}
	}
	if linger.Level != levelFail {
		t.Errorf("linger disabled = %v, want fail", linger.Level)
	}
	if !strings.Contains(strings.Join(linger.Fix, " "), "loginctl enable-linger tester") {
		t.Errorf("the fix should be the exact command: %v", linger.Fix)
	}
}

// A LaunchAgent an older latem wrote is named, with the command that
// rewrites it: nothing else does, and before revision 2 it was a job macOS
// throttled.
func TestDoctor_AnOutdatedServiceDefinition(t *testing.T) {
	dir := privateDir(t)
	cmds := healthyRunner("darwin")
	cmds["launchctl print gui/501/com.latem.agent"] = fakeCmd{
		out: "com.latem.agent = {\n\tstate = running\n\tpid = 2566\n}\n"}
	d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)
	path := system.LaunchAgentPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("<plist><dict><key>ProcessType</key><string>Standard</string></dict></plist>")
	got := checkByID(d.checkService(t.Context()), "service.definition")
	if got == nil || got.Level != levelWarn {
		t.Fatalf("check = %+v, want a warning", got)
	}
	if !strings.Contains(got.Detail, "revision 1; this latem writes revision 2") ||
		!strings.Contains(strings.Join(got.Fix, " "), "latem service install") {
		t.Errorf("detail %q, fix %q", got.Detail, got.Fix)
	}

	write("<plist>\n<!-- latem service definition, revision 2 -->\n<dict/></plist>")
	if got := checkByID(d.checkService(t.Context()), "service.definition"); got != nil {
		t.Errorf("a current definition was named: %+v", got)
	}
}

func TestReportCountsAndFixes(t *testing.T) {
	var rep report
	rep.section("A",
		pass("a.1", "one", "fine"),
		warn("a.2", "two", "hmm", "fix-a"),
		fail("a.3", "three", "no", "fix-b", "fix-a"),
		skip("a.4", "four", "n/a"),
	)
	rep.section("Empty") // sections with no checks are dropped

	passed, warned, failed, skipped := rep.counts()
	if passed != 1 || warned != 1 || failed != 1 || skipped != 1 {
		t.Errorf("counts = %d/%d/%d/%d", passed, warned, failed, skipped)
	}
	if len(rep.Sections) != 1 {
		t.Errorf("an empty section was kept: %d sections", len(rep.Sections))
	}
	// Deduplicated, in encounter order, so a consolidated list can be pasted.
	fixes := rep.fixes()
	if len(fixes) != 2 || fixes[0] != "fix-a" || fixes[1] != "fix-b" {
		t.Errorf("fixes = %v, want [fix-a fix-b]", fixes)
	}
}

func TestPrintReport(t *testing.T) {
	var rep report
	rep.section("Tooling",
		pass("t.1", "git", "git version 2.50.1"),
		fail("t.2", "docker", "not on PATH", "brew install --cask docker"),
	)

	var buf strings.Builder
	printReport(&buf, rep, false, false)
	out := buf.String()
	for _, want := range []string{"[ ok ]", "[FAIL]", "fix: brew install --cask docker", "1 passed", "1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("colour escapes leaked into a non-terminal destination")
	}

	buf.Reset()
	printReport(&buf, rep, true, false)
	if !strings.Contains(buf.String(), "does not run these for you") {
		t.Error("--fix must say it does not run the commands")
	}
}

func TestWrapLines(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		lines int
	}{
		{"empty", "", 20, 0},
		{"one short line", "hello world", 20, 1},
		{"wraps on words", strings.Repeat("word ", 20), 20, 5},
		{"a single long word is not broken", "supercalifragilistic", 5, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapLines(tt.in, tt.width)
			if len(got) != tt.lines {
				t.Errorf("wrapLines(%q, %d) produced %d lines, want %d: %q", tt.in, tt.width, len(got), tt.lines, got)
			}
			for _, l := range got {
				if strings.HasPrefix(l, " ") || strings.HasSuffix(l, " ") {
					t.Errorf("line %q has stray whitespace", l)
				}
			}
		})
	}
}

func TestDoctorCommandExitCode(t *testing.T) {
	// The acceptance criterion: doctor exits non-zero when a check fails.
	// It is asserted through the exported error type rather than by running
	// the process, so it works the same on a machine with everything and a
	// machine with nothing.
	err := &ExitError{Code: ExitFailure, Err: fmt.Errorf("3 check(s) failed")}
	if exitCode(err) != ExitFailure {
		t.Errorf("a failing doctor should exit %d", ExitFailure)
	}
}

func TestFirstLineOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"single", "single"},
		{"first\nsecond", "first"},
		{"  padded  \nnext", "padded"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := firstLineOf(tt.in); got != tt.want {
			t.Errorf("firstLineOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEnvName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"MATCH_PASSWORD", "MATCH_PASSWORD"},
		{"match-password", "MATCH_PASSWORD"},
		{"app.store.key", "APP_STORE_KEY"},
	}
	for _, tt := range tests {
		if got := envName(tt.in); got != tt.want {
			t.Errorf("envName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// secretsErrorsAreDistinguished documents that a name nobody configured and a
// backend that cannot be reached are different diagnoses (amendment A16).
func TestSecretsBackendErrorCarriesDiagnostic(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Secrets.Commands = map[string]string{"X": "op read op://x"}
	d := newTestDoctor("linux", dir, nil, cfg, nil)
	d.includeSecrets = true
	d.newSecrets = (&fakeSecrets{errs: map[string]error{
		"X": &secrets.CommandError{Program: "op", ExitCode: 1, Stderr: []byte("op: you are not currently signed in")},
	}}).factory()

	got := d.checkSecrets(context.Background())
	var x check
	for _, c := range got {
		if c.ID == "secret.X" {
			x = c
		}
	}
	if x.Level != levelFail {
		t.Fatalf("a backend failure = %v, want fail", x.Level)
	}
	// The backend's own words are the whole answer to a support question, and
	// are deliberately absent from the error string that can reach a broker.
	if !strings.Contains(strings.Join(x.Notes, " "), "not currently signed in") {
		t.Errorf("the backend's diagnostic was dropped: %+v", x)
	}
}

// TestDoctorConfigLoadedByAgent is finding 7 of the M7 acceptance test.
//
// `doctor` read config.toml while the agent ran on what it had loaded at
// start, so adding a repo by hand produced a green `[ ok ] repo / <name>`
// followed immediately by a run rejected E_UNKNOWN_REPO. A diagnostic whose
// purpose is catching drift before a run does must not be blind to drift in
// latem's own state.
func TestDoctorConfigLoadedByAgent(t *testing.T) {
	// `ps -o lstart=` for the pid launchctl reports, in the format both
	// platforms print.
	psKey := "ps -o lstart= -p 2566"

	newDoctor := func(t *testing.T, cmds map[string]fakeCmd, configModTime time.Time) *doctor {
		t.Helper()
		dir := privateDir(t)
		cfg := testConfig(dir)
		if err := os.WriteFile(cfg.Paths.File, []byte("# config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(cfg.Paths.File, configModTime, configModTime); err != nil {
			t.Fatal(err)
		}
		d := newTestDoctor("darwin", dir, cmds, cfg, nil)
		d.file = cfg.Paths.File
		return d
	}

	// The plist only has to exist for ServiceStatus to report Installed; its
	// contents are launchd's business and this check never reads them.
	installed := func(t *testing.T, dir string) {
		t.Helper()
		path := system.LaunchAgentPath(dir)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	agentStarted := time.Now().Add(-1 * time.Hour)
	psOut := agentStarted.Format("Mon Jan  2 15:04:05 2006")

	t.Run("config edited after the agent started", func(t *testing.T) {
		cmds := healthyRunner("darwin")
		cmds["launchctl print gui/501/com.latem.agent"] = fakeCmd{
			out: "com.latem.agent = {\n\tstate = running\n\tpid = 2566\n}\n"}
		cmds[psKey] = fakeCmd{out: psOut}
		d := newDoctor(t, cmds, time.Now()) // edited just now, an hour after start
		installed(t, d.dir)

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil {
			t.Fatal("no config.loaded check")
		}
		if got.Level != levelWarn {
			t.Errorf("level = %v, want a warning; detail = %q", got.Level, got.Detail)
		}
		if !strings.Contains(strings.Join(got.Fix, " "), "latem service restart") {
			t.Errorf("fix = %q, want it to name `latem service restart`", got.Fix)
		}
	})

	t.Run("agent started after the last edit", func(t *testing.T) {
		cmds := healthyRunner("darwin")
		cmds["launchctl print gui/501/com.latem.agent"] = fakeCmd{
			out: "com.latem.agent = {\n\tstate = running\n\tpid = 2566\n}\n"}
		cmds[psKey] = fakeCmd{out: psOut}
		d := newDoctor(t, cmds, agentStarted.Add(-10*time.Minute))
		installed(t, d.dir)

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelPass {
			t.Errorf("check = %+v, want a pass", got)
		}
	})

	// A54: an agent that reloads says when it last read the file.
	reloaded := func(t *testing.T, d *doctor, pid int, at time.Time, refused string) {
		t.Helper()
		st := agent.ReloadStatus{PID: pid, Started: agentStarted, Reloads: 1, At: at, Error: refused}
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.DerivePaths(d.dir).ReloadStatus, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	running := func() map[string]fakeCmd {
		cmds := healthyRunner("darwin")
		cmds["launchctl print gui/501/com.latem.agent"] = fakeCmd{
			out: "com.latem.agent = {\n\tstate = running\n\tpid = 2566\n}\n"}
		cmds[psKey] = fakeCmd{out: psOut}
		return cmds
	}

	t.Run("config edited, then reloaded", func(t *testing.T) {
		edited := time.Now().Add(-5 * time.Minute)
		d := newDoctor(t, running(), edited)
		installed(t, d.dir)
		reloaded(t, d, 2566, edited.Add(time.Minute), "")

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelPass {
			t.Errorf("check = %+v, want a pass", got)
		}
	})

	t.Run("config edited after the last reload", func(t *testing.T) {
		d := newDoctor(t, running(), time.Now())
		installed(t, d.dir)
		reloaded(t, d, 2566, time.Now().Add(-10*time.Minute), "")

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn {
			t.Fatalf("check = %+v, want a warning", got)
		}
		if !strings.Contains(strings.Join(got.Fix, " "), "latem service reload") {
			t.Errorf("fix = %q; an agent that reloads should be told to", got.Fix)
		}
	})

	t.Run("a reload status from another process vouches for nothing", func(t *testing.T) {
		d := newDoctor(t, running(), time.Now())
		installed(t, d.dir)
		reloaded(t, d, 1, time.Now().Add(time.Minute), "")

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn || !strings.Contains(strings.Join(got.Fix, " "), "restart") {
			t.Errorf("check = %+v, want a warning naming a restart", got)
		}
	})

	t.Run("the agent refused the last reload", func(t *testing.T) {
		d := newDoctor(t, running(), time.Now().Add(-time.Minute))
		installed(t, d.dir)
		reloaded(t, d, 2566, time.Now(), `unknown key "repos.app.urll"`)

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn || !strings.Contains(got.Detail, "refused") {
			t.Errorf("check = %+v, want a warning that the reload was refused", got)
		}
	})

	// A reload that read a runner change applied the rest and deferred that:
	// the agent is not running the file, whatever the times say.
	t.Run("a reload that deferred a change", func(t *testing.T) {
		edited := time.Now().Add(-5 * time.Minute)
		d := newDoctor(t, running(), edited)
		installed(t, d.dir)
		st := agent.ReloadStatus{PID: 2566, Started: agentStarted, Reloads: 1, At: edited.Add(time.Minute),
			Deferred: []string{`runner "linux" was changed`}}
		b, _ := json.Marshal(st)
		if err := os.WriteFile(config.DerivePaths(d.dir).ReloadStatus, b, 0o600); err != nil {
			t.Fatal(err)
		}
		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn || !strings.Contains(got.Detail, `runner "linux"`) ||
			!strings.Contains(strings.Join(got.Fix, " "), "latem service restart") {
			t.Errorf("check = %+v, want a warning naming the deferred change and a restart", got)
		}
	})

	// The first status is written when the agent is ready, minutes after it
	// read the file; an edit in between is in no config it has.
	t.Run("an edit during startup", func(t *testing.T) {
		edited := agentStarted.Add(time.Minute)
		d := newDoctor(t, running(), edited)
		installed(t, d.dir)
		st := agent.ReloadStatus{PID: 2566, Started: agentStarted.Add(3 * time.Minute), At: agentStarted.Add(3 * time.Minute)}
		b, _ := json.Marshal(st)
		if err := os.WriteFile(config.DerivePaths(d.dir).ReloadStatus, b, 0o600); err != nil {
			t.Fatal(err)
		}
		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn {
			t.Errorf("check = %+v, want a warning: the ready status is not a read of the file", got)
		}
	})

	// A80: SIGUSR2 and `latem update` re-execute the agent in place (A56).
	// The kernel still dates the pid from the first start, and the new
	// binary read config.toml as it started, at the time its status gives.
	switched := func(t *testing.T, d *doctor, pid int, loaded time.Time) {
		t.Helper()
		ready := loaded.Add(2 * time.Minute)
		b, err := json.Marshal(agent.ReloadStatus{PID: pid, Started: ready, Loaded: loaded, At: ready, Version: "v0.4.0"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.DerivePaths(d.dir).ReloadStatus, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("config edited, then the agent switched in place", func(t *testing.T) {
		d := newDoctor(t, running(), agentStarted.Add(30*time.Minute))
		installed(t, d.dir)
		switched(t, d, 2566, agentStarted.Add(50*time.Minute))

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelPass || !strings.Contains(got.Detail, "started after") {
			t.Errorf("check = %+v, want a pass: the switched agent read the file after the edit", got)
		}
	})

	t.Run("config edited after the agent switched in place", func(t *testing.T) {
		loaded := agentStarted.Add(30 * time.Minute)
		d := newDoctor(t, running(), agentStarted.Add(50*time.Minute))
		installed(t, d.dir)
		switched(t, d, 2566, loaded)

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn {
			t.Fatalf("check = %+v, want a warning", got)
		}
		if want := "20m0s after"; !strings.Contains(got.Detail, want) || !strings.Contains(got.Detail, loaded.Format("15:04:05")) {
			t.Errorf("detail = %q, want %q and the switch's read time", got.Detail, want)
		}
		if !strings.Contains(strings.Join(got.Fix, " "), "latem service reload") {
			t.Errorf("fix = %q, want `latem service reload`", got.Fix)
		}
	})

	// The time a switched agent read the file is still before it was ready.
	t.Run("an edit while the switched agent started", func(t *testing.T) {
		loaded := agentStarted.Add(30 * time.Minute)
		d := newDoctor(t, running(), loaded.Add(time.Minute))
		installed(t, d.dir)
		switched(t, d, 2566, loaded)

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn {
			t.Errorf("check = %+v, want a warning: the edit came after the read", got)
		}
	})

	t.Run("another process's read time vouches for nothing", func(t *testing.T) {
		d := newDoctor(t, running(), agentStarted.Add(30*time.Minute))
		installed(t, d.dir)
		switched(t, d, 1, agentStarted.Add(50*time.Minute))

		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelWarn || !strings.Contains(strings.Join(got.Fix, " "), "restart") {
			t.Errorf("check = %+v, want a warning naming a restart", got)
		}
	})

	t.Run("no service installed is not a finding", func(t *testing.T) {
		d := newDoctor(t, healthyRunner("darwin"), time.Now())
		got := checkByID(d.checkConfiguration(t.Context()), "config.loaded")
		if got == nil || got.Level != levelSkip {
			t.Errorf("check = %+v, want a skip", got)
		}
	})
}

func checkByID(checks []check, id string) *check {
	for i := range checks {
		if checks[i].ID == id {
			return &checks[i]
		}
	}
	return nil
}

// A runner set to take more jobs at once than the machine has room for is a
// warning naming the number; one at or under it passes, and says the room.
func TestDoctor_Capacity(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{
		{Name: "macos", Executor: config.ExecutorNative, Concurrency: 1},
		{Name: "linux", Executor: config.ExecutorDocker, Concurrency: 3},
	}
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), cfg, nil)
	d.machine = func(context.Context) capacity.Machine {
		return capacity.Machine{CPUs: 8, MemoryGB: 8, Docker: &capacity.Docker{CPUs: 6, MemoryGB: 3.8}}
	}

	checks := d.checkCapacity(t.Context())
	native, linux := checkByID(checks, "capacity.macos"), checkByID(checks, "capacity.linux")
	if native == nil || native.Level != levelPass {
		t.Errorf("native = %+v, want a pass", native)
	}
	if linux == nil || linux.Level != levelWarn || !strings.Contains(linux.Detail, "concurrency 3; this machine has room for 1") ||
		!strings.Contains(strings.Join(linux.Fix, " "), "concurrency = 1") {
		t.Errorf("linux = %+v, want a warning with the number and the fix", linux)
	}

	d.machine = func(context.Context) capacity.Machine { return capacity.Machine{} }
	if got := checkByID(d.checkCapacity(t.Context()), "capacity"); got == nil || got.Level != levelSkip {
		t.Errorf("an unmeasured machine = %+v, want a skip", got)
	}
}
