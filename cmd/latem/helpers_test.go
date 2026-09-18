package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor/docker"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// fakeCmd is one canned answer for [fakeRunner].
type fakeCmd struct {
	out  string
	exit int
}

// fakeRunner answers external commands from a table, so every check runs on a
// machine that has none of the tools involved.
//
// That is the point of injecting the probes at all: the interesting `doctor`
// cases are a Mac with no Xcode, a Linux box with no Docker, and a machine
// whose keychain is locked, and none of those can be arranged on the machine
// running the tests.
type fakeRunner struct {
	mu   sync.Mutex
	cmds map[string]fakeCmd
}

func newFakeRunner(cmds map[string]fakeCmd) *fakeRunner {
	if cmds == nil {
		cmds = map[string]fakeCmd{}
	}
	return &fakeRunner{cmds: cmds}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	cmd, ok := f.cmds[key]
	f.mu.Unlock()
	if !ok {
		return nil, &system.CommandError{Name: name, ExitCode: -1, Err: exec.ErrNotFound}
	}
	if cmd.exit != 0 {
		return []byte(cmd.out), &system.CommandError{Name: name, ExitCode: cmd.exit, Output: []byte(cmd.out)}
	}
	return []byte(cmd.out), nil
}

func fakePath(names ...string) func(string) (string, error) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

// fakeDocker stands in for the Docker executor.
type fakeDocker struct {
	newErr       error
	preflightErr error
	report       docker.ImageReport
	reportErr    error
	closed       bool
}

func (f *fakeDocker) Preflight(context.Context) error { return f.preflightErr }
func (f *fakeDocker) CheckImage(context.Context, string) (docker.ImageReport, error) {
	return f.report, f.reportErr
}
func (f *fakeDocker) Close() error { f.closed = true; return nil }

func (f *fakeDocker) factory() func(docker.Options) (dockerProbe, error) {
	return func(docker.Options) (dockerProbe, error) {
		if f.newErr != nil {
			return nil, f.newErr
		}
		return f, nil
	}
}

// fakeSecrets stands in for the §15 resolution chain.
type fakeSecrets struct {
	values map[string]string
	errs   map[string]error
}

func (f *fakeSecrets) Resolve(_ context.Context, name string) (secrets.Value, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	if v, ok := f.values[name]; ok {
		return secrets.Value(v), nil
	}
	return nil, secrets.ErrNotFound
}

func (f *fakeSecrets) Providers() []string { return []string{"env", "file"} }

func (f *fakeSecrets) factory() func(secrets.Options) (secretResolver, error) {
	return func(secrets.Options) (secretResolver, error) { return f, nil }
}

// healthyRunner is a command table for a machine where everything works.
func healthyRunner(goos string) map[string]fakeCmd {
	cmds := map[string]fakeCmd{
		"git --version":    {out: "git version 2.50.1"},
		"docker --version": {out: "Docker version 29.6.2, build dfc4efb"},
	}
	if goos == "darwin" {
		cmds["tart --version"] = fakeCmd{out: "2.28.0"}
		cmds["fdesetup status"] = fakeCmd{out: "FileVault is Off."}
		cmds["defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser"] = fakeCmd{out: "tester\n"}
		cmds["launchctl managername"] = fakeCmd{out: "Aqua"}
		cmds["security show-keychain-info"] = fakeCmd{out: `Keychain "<NULL>" no-timeout`}
		cmds["xcode-select -p"] = fakeCmd{out: "/Applications/Xcode.app/Contents/Developer"}
		cmds["xcodebuild -checkFirstLaunchStatus"] = fakeCmd{}
		cmds["xcodebuild -version"] = fakeCmd{out: "Xcode 26.0.1"}
		cmds["launchctl print gui/501/com.latem.agent"] = fakeCmd{out: "com.latem.agent = {\n\tstate = running\n"}
		cmds[localeProbe] = fakeCmd{out: "en_US.UTF-8\t\tUTF-8\n"}
	} else {
		cmds["id -nG"] = fakeCmd{out: "tester docker"}
		cmds["loginctl show-user tester --property=Linger"] = fakeCmd{out: "Linger=yes"}
		cmds["systemctl --user is-active latem.service"] = fakeCmd{out: "active"}
	}
	return cmds
}

// testConfig builds a valid config rooted at dir.
//
// It is assembled as a struct literal rather than parsed from TOML because
// several checks need a config that internal/config would refuse to produce —
// a non-loopback metrics_addr, most importantly, which the loader rejects and
// which this package's second-line check still has to be tested against.
func testConfig(dir string) *config.Config {
	return &config.Config{
		SchemaVersion: config.SchemaVersion,
		Machine:       config.Machine{Name: "test"},
		Agent:         config.Agent{MetricsAddr: config.DefaultMetricsAddr, WorkDir: dir + "/work"},
		Repos: map[string]*config.Repo{
			"app": {URL: "https://example.invalid/app.git", AllowedRefs: []string{"main"}},
		},
		Runners: []*config.Runner{{
			Name:        "linux-x64",
			Executor:    config.ExecutorDocker,
			Concurrency: 2,
			Docker:      &config.DockerRunner{DefaultImage: "ghcr.io/me/ci-base:latest", MinFreeGB: 40},
		}},
		Paths: config.Paths{
			Dir:        dir,
			File:       dir + "/config.toml",
			Work:       dir + "/work",
			Mirrors:    dir + "/mirrors",
			Caches:     dir + "/caches",
			Logs:       dir + "/logs",
			Artifacts:  dir + "/artifacts",
			Socket:     dir + "/agent.sock",
			TokenFile:  dir + "/agent.key",
			SecretsEnv: dir + "/secrets.env",
			DB:         dir + "/state.db",
			Lock:       dir + "/agent.lock",
		},
	}
}

// newTestDoctor wires a doctor with every external dependency faked.
func newTestDoctor(goos, dir string, cmds map[string]fakeCmd, cfg *config.Config, cfgErr error) *doctor {
	onPath := []string{"git", "docker", "id", "loginctl", "systemctl"}
	if goos == "darwin" {
		onPath = []string{"git", "docker", "tart", "fdesetup", "defaults", "launchctl", "security", "xcode-select", "xcodebuild", "env", "bash"}
	}
	return &doctor{
		sys: system.New(system.Options{
			GOOS:     goos,
			GOARCH:   "arm64",
			Home:     dir,
			User:     "tester",
			Run:      newFakeRunner(cmds).run,
			LookPath: fakePath(onPath...),
			Getenv:   func(string) string { return "" },
			Getuid:   func() int { return 501 },
		}),
		dir:  dir,
		file: dir + "/config.toml",
		// Set directly as well as through loadCfg, so a test can exercise one
		// check in isolation without going through run().
		cfg:        cfg,
		cfgErr:     cfgErr,
		loadCfg:    func() (*config.Config, error) { return cfg, cfgErr },
		newDocker:  (&fakeDocker{}).factory(),
		newSecrets: (&fakeSecrets{}).factory(),
		sameFS:     func(string, string) error { return nil },
		// A machine with room. Every default here describes a healthy host, and
		// free space was the one that came from the real filesystem instead —
		// so "a 40 GB floor passes" held on a laptop and failed on every
		// runner. A test that wants a full disk says so.
		freeGB: func(context.Context) (float64, error) { return testFreeGB, nil },
		// Nowhere, unless a test puts a ruby somewhere it controls. The real
		// list is absolute paths under /opt, which system.ToolAt stats for
		// real: inheriting them means reading whatever Homebrew this machine
		// happens to have.
		kegRubies: []string{filepath.Join(dir, "no", "keg", "ruby")},
	}
}

// testFreeGB is comfortably above every runner floor in §14.4, including
// tart's 200.
const testFreeGB = 500.0

// findCheck locates a check by ID across every section.
func findCheck(rep report, id string) (check, bool) {
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			if c.ID == id {
				return c, true
			}
		}
	}
	return check{}, false
}

func okImageReport(image string) docker.ImageReport {
	return docker.ImageReport{
		Image: image,
		Checks: []docker.ImageCheck{
			{Name: "user-1000", OK: true, Detail: "uid 1000 is runner"},
			{Name: "home-writable", OK: true, Detail: "/home/runner is writable"},
			{Name: "tool-bash", OK: true, Detail: "bash present"},
		},
	}
}

func brokenImageReport(image string) docker.ImageReport {
	return docker.ImageReport{
		Image: image,
		Checks: []docker.ImageCheck{
			{Name: "user-1000", OK: false, Detail: "no /etc/passwd entry for uid 1000"},
			{Name: "tool-git", OK: false, Detail: "git is missing"},
		},
	}
}

// privateDir is a t.TempDir chmod-ed to the 0700 RUNNER_SPEC §23 requires,
// so the mode checks do not fail for every test that uses one.
func privateDir(t interface {
	TempDir() string
	Fatal(...any)
}) string {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newSystemForTest builds a faked system.System with only the named programs
// resolvable on PATH.
func newSystemForTest(goos, home string, cmds map[string]fakeCmd, onPath ...string) *system.System {
	return system.New(system.Options{
		GOOS:     goos,
		GOARCH:   "arm64",
		Home:     home,
		User:     "tester",
		Run:      newFakeRunner(cmds).run,
		LookPath: fakePath(onPath...),
		Getenv:   func(string) string { return "" },
		Getuid:   func() int { return 501 },
	})
}

// localeProbe is the [fakeRunner] key for A33's locale probe.
//
// Spelled out rather than rebuilt from the check's own constants: if the argv
// changes, the fake machine should stop answering and a test should say so,
// because the argv *is* the contract with the shell.
const localeProbe = "env LANG=en_US.UTF-8 bash -lc " +
	`printf '%s\t%s\t%s\n' "${LANG-}" "${LC_ALL-}" "$(locale charmap 2>/dev/null)"`

// localeAnswer builds one canned answer for that probe.
func localeAnswer(lang, lcAll, charmap string) fakeCmd {
	return fakeCmd{out: lang + "\t" + lcAll + "\t" + charmap + "\n"}
}

// newSystemForTestEnv is [newSystemForTest] with an environment.
//
// Separate rather than variadic because most checks do not read the
// environment at all, and the ones that do — ANDROID_HOME, JAVA_HOME — are the
// ones whose interesting states are "set and wrong" rather than "absent".
func newSystemForTestEnv(goos, home string, cmds map[string]fakeCmd, env map[string]string) *system.System {
	return system.New(system.Options{
		GOOS:     goos,
		GOARCH:   "arm64",
		Home:     home,
		User:     "tester",
		Run:      newFakeRunner(cmds).run,
		LookPath: fakePath("git", "docker", "java", "keytool"),
		Getenv:   func(k string) string { return env[k] },
		Getuid:   func() int { return 501 },
	})
}

// mkdirs builds a nested directory under root and returns the path to the
// first segment, which is what an ANDROID_HOME points at.
func mkdirs(t *testing.T, root string, segments ...string) string {
	t.Helper()
	full := filepath.Join(append([]string{root}, segments...)...)
	if err := os.MkdirAll(full, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, segments[0])
}
