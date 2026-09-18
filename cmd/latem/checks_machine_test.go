package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// The checks in this file are the ones an operator reaches for when a build is
// already failing, and they were the least covered in the package: a check that
// is wrong misleads at the worst possible moment, and one that is untested is
// only known to compile.
//
// Each case here is a machine state that cannot be arranged on the machine
// running the tests — no /dev/kvm, a keg-only Ruby shadowed by macOS's 2.6, an
// SDK with no platform-tools — which is the whole reason doctor's probes are
// injected rather than called.

func gpuRunnerConfig(dir string) *config.Config {
	cfg := testConfig(dir)
	cfg.Runners = []*config.Runner{{
		Name:        "linux-gpu",
		Executor:    config.ExecutorDocker,
		Concurrency: 1,
		Docker:      &config.DockerRunner{DefaultImage: "ghcr.io/me/ci:latest", GPU: true, MinFreeGB: 40},
	}}
	return cfg
}

func TestDoctorGPU(t *testing.T) {
	t.Run("a GPU runner whose toolkit works", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), gpuRunnerConfig(dir), nil)
		d.newDocker = (&fakeDocker{}).factory()

		got := d.checkGPU(t.Context())
		if len(got) != 1 || got[0].Level != levelPass {
			t.Fatalf("got %v, want one pass", got)
		}
	})

	t.Run("a GPU runner whose toolkit is missing", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), gpuRunnerConfig(dir), nil)
		d.newDocker = (&fakeDocker{preflightErr: errors.New("could not select device driver")}).factory()

		got := d.checkGPU(t.Context())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("got %v, want one failure", got)
		}
		// §20: a runner that fails preflight accepts no work, rather than
		// failing a build at minute forty. The fix has to be here, because the
		// error the build would show names a device driver.
		if len(got[0].Fix) == 0 {
			t.Error("the failure carries no fix")
		}
		if !strings.Contains(strings.Join(got[0].Notes, " "), "minute forty") {
			t.Errorf("the note does not say why preflight matters: %v", got[0].Notes)
		}
	})

	t.Run("no daemon at all", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), gpuRunnerConfig(dir), nil)
		d.newDocker = (&fakeDocker{newErr: errors.New("cannot reach the daemon")}).factory()

		got := d.checkGPU(t.Context())
		if len(got) != 1 || got[0].Level != levelFail {
			t.Fatalf("got %v, want one failure", got)
		}
	})

	// The check is per-runner and opt-in: a docker runner without `gpu = true`
	// is not asked about, because reporting "no NVIDIA toolkit" on a machine
	// that never wanted one is noise an operator learns to skip past.
	t.Run("a docker runner that did not ask for a GPU is not asked about", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.newDocker = (&fakeDocker{preflightErr: errors.New("no driver")}).factory()

		if got := d.checkGPU(t.Context()); len(got) != 0 {
			t.Fatalf("got %v, want nothing", got)
		}
	})
}

func TestDoctorKVM(t *testing.T) {
	cases := map[string]struct {
		kvm  system.KVM
		want level
		says string
	}{
		"usable":     {system.KVM{Present: true, Usable: true}, levelPass, ""},
		"absent":     {system.KVM{}, levelWarn, "absent"},
		"not usable": {system.KVM{Present: true}, levelWarn, "cannot open it read-write"},
		"unreadable": {system.KVM{Err: errors.New("permission denied")}, levelWarn, "could not be inspected"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := privateDir(t)
			d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
			d.kvm = func() system.KVM { return tc.kvm }

			got := d.checkKVM()
			if got.Level != tc.want {
				t.Fatalf("level = %v, want %v (detail %q)", got.Level, tc.want, got.Detail)
			}
			if tc.says != "" && !strings.Contains(got.Detail, tc.says) {
				t.Errorf("detail %q does not mention %q", got.Detail, tc.says)
			}
		})
	}

	// The read-write distinction is the point of the check: a device the user
	// can only read passes a naive existence test and the emulator still
	// fails. The note has to say so, because the fix is a group membership
	// that takes a re-login.
	t.Run("a read-only device names the group fix", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.kvm = func() system.KVM { return system.KVM{Present: true} }

		got := d.checkKVM()
		joined := strings.Join(append(got.Fix, got.Notes...), " ")
		if !strings.Contains(joined, "usermod -aG kvm") {
			t.Errorf("no group fix offered: %v", got.Fix)
		}
		if !strings.Contains(joined, "read-only check passes") {
			t.Errorf("the note does not explain why a read check is not enough: %v", got.Notes)
		}
	})
}

func TestDoctorRuby(t *testing.T) {
	const keg = "/opt/homebrew/opt/ruby/bin/ruby"

	t.Run("modern ruby on PATH", func(t *testing.T) {
		dir := privateDir(t)
		cmds := healthyRunner("darwin")
		cmds["ruby --version"] = fakeCmd{out: "ruby 3.4.2 (2026-01-15 revision abc) [arm64-darwin25]"}
		d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)
		d.sys = newSystemForTest("darwin", dir, cmds, "ruby")

		got, bin := d.checkRuby(t.Context())
		if got.Level != levelPass {
			t.Fatalf("level = %v, want pass (%q)", got.Level, got.Detail)
		}
		if bin == "" {
			t.Error("a passing ruby check returned no bin directory for bundler to be probed in")
		}
	})

	// The documented trap: macOS ships 2.6 at /usr/bin/ruby, Homebrew's is
	// keg-only, and `which ruby` after "installing Ruby" still answers 2.6.
	t.Run("the keg-only trap", func(t *testing.T) {
		dir := privateDir(t)
		cmds := healthyRunner("darwin")
		cmds["ruby --version"] = fakeCmd{out: "ruby 2.6.10 (2022-04-12 revision def) [universal.arm64e-darwin25]"}
		cmds[keg+" --version"] = fakeCmd{out: "ruby 3.4.2 (2026-01-15 revision abc) [arm64-darwin25]"}
		d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)
		d.sys = newSystemForTest("darwin", dir, cmds, "ruby")

		got, bin := d.checkRuby(t.Context())
		if got.Level != levelWarn {
			t.Fatalf("level = %v, want warn (%q)", got.Level, got.Detail)
		}
		// Both halves of the situation, because either alone is confusing:
		// what PATH gives, and what is installed.
		if !strings.Contains(got.Detail, "2.6.10") || !strings.Contains(got.Detail, "3.4.2") {
			t.Errorf("detail does not name both rubies: %q", got.Detail)
		}
		if bin != "/opt/homebrew/opt/ruby/bin" {
			t.Errorf("bin = %q, want the keg's bin so bundler is probed against the ruby that works", bin)
		}
		if len(got.Fix) == 0 || !strings.Contains(got.Fix[0], "PATH") {
			t.Errorf("no PATH fix offered: %v", got.Fix)
		}
	})

	t.Run("no ruby anywhere", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("darwin", dir, map[string]fakeCmd{}, testConfig(dir), nil)
		d.sys = newSystemForTest("darwin", dir, map[string]fakeCmd{}, "nothing")

		got, bin := d.checkRuby(t.Context())
		if got.Level != levelWarn {
			t.Fatalf("level = %v, want warn", got.Level)
		}
		if bin != "" {
			t.Errorf("bin = %q, want empty when there is no ruby", bin)
		}
		// Only fastlane needs it, and saying so is what stops an operator
		// installing Ruby on a machine that builds neither iOS nor Android.
		if !strings.Contains(strings.Join(got.Notes, " "), "fastlane") {
			t.Errorf("the note does not say what needs ruby: %v", got.Notes)
		}
	})

	t.Run("too old, with nothing better installed", func(t *testing.T) {
		dir := privateDir(t)
		cmds := map[string]fakeCmd{"ruby --version": {out: "ruby 2.6.10 (2022-04-12) [arm64-darwin25]"}}
		d := newTestDoctor("darwin", dir, cmds, testConfig(dir), nil)
		d.sys = newSystemForTest("darwin", dir, cmds, "ruby")

		got, _ := d.checkRuby(t.Context())
		if got.Level != levelWarn {
			t.Fatalf("level = %v, want warn", got.Level)
		}
		if !strings.Contains(got.Detail, "too old") {
			t.Errorf("detail = %q, want it to say the version is too old", got.Detail)
		}
	})
}

func TestDoctorAndroidSDK(t *testing.T) {
	t.Run("ANDROID_HOME set and populated", func(t *testing.T) {
		dir := privateDir(t)
		sdk := mkdirs(t, dir, "sdk", "platform-tools")
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.sys = newSystemForTestEnv("linux", dir, healthyRunner("linux"),
			map[string]string{"ANDROID_HOME": sdk})

		got := d.checkAndroidSDK()
		if got.Level != levelPass {
			t.Fatalf("level = %v, want pass (%q)", got.Level, got.Detail)
		}
	})

	// The value is set and wrong, which is worse than unset: Gradle's error
	// blames the SDK location and the variable looks configured.
	t.Run("ANDROID_HOME set but empty", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.sys = newSystemForTestEnv("linux", dir, healthyRunner("linux"),
			map[string]string{"ANDROID_HOME": dir + "/nowhere"})

		got := d.checkAndroidSDK()
		if got.Level != levelWarn {
			t.Fatalf("level = %v, want warn", got.Level)
		}
		if !strings.Contains(got.Detail, "platform-tools") {
			t.Errorf("detail = %q, want it to name what is missing", got.Detail)
		}
	})

	t.Run("an SDK on disk that nothing points at", func(t *testing.T) {
		dir := privateDir(t)
		// The path doctor knows to look in; see androidSDKs.
		mkdirs(t, dir, "Library", "Android", "sdk", "platform-tools")
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.sys = newSystemForTestEnv("linux", dir, healthyRunner("linux"), nil)

		got := d.checkAndroidSDK()
		if got.Level != levelWarn {
			t.Fatalf("level = %v, want warn (%q)", got.Level, got.Detail)
		}
		if len(got.Fix) == 0 || !strings.Contains(got.Fix[0], "ANDROID_HOME") {
			t.Errorf("no export offered: %v", got.Fix)
		}
		// local.properties is Gradle's own advice and the wrong answer for a
		// runner, so the note has to say why.
		if !strings.Contains(strings.Join(got.Notes, " "), "local.properties") {
			t.Errorf("the note does not address Gradle's advice: %v", got.Notes)
		}
	})

	t.Run("no SDK at all", func(t *testing.T) {
		dir := privateDir(t)
		d := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
		d.sys = newSystemForTestEnv("linux", dir, healthyRunner("linux"), nil)

		got := d.checkAndroidSDK()
		if got.Level != levelWarn || !strings.Contains(got.Detail, "not found") {
			t.Fatalf("got %v %q, want a warning saying it is not found", got.Level, got.Detail)
		}
	})
}

// The broker check's two silent paths: a machine with no config and one with no
// remote broker. Both must skip rather than warn — an agent that never
// registers is a supported state (§7.3), and warning about a clock nobody
// compares against would be noise on every local-only machine.
func TestDoctorBroker_SkipsWhenThereIsNothingToCompareAgainst(t *testing.T) {
	dir := privateDir(t)

	noCfg := newTestDoctor("linux", dir, healthyRunner("linux"), nil, errors.New("no config"))
	if got := noCfg.checkBroker(t.Context()); len(got) != 1 || got[0].Level != levelSkip {
		t.Errorf("no config: got %v, want one skip", got)
	}

	local := newTestDoctor("linux", dir, healthyRunner("linux"), testConfig(dir), nil)
	got := local.checkBroker(t.Context())
	if len(got) != 1 || got[0].Level != levelSkip {
		t.Fatalf("local only: got %v, want one skip", got)
	}
	if !strings.Contains(got[0].Detail, "no remote broker") {
		t.Errorf("detail = %q", got[0].Detail)
	}
}
