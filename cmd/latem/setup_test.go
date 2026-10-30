package main

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// fakeService is a service manager that records what setup asked of it.
type fakeService struct {
	state             system.ServiceState
	installs, restart int
}

func (f *fakeService) Status(context.Context) (system.ServiceState, error) { return f.state, nil }

func (f *fakeService) Install(_ context.Context, w io.Writer) error {
	f.installs++
	f.state = system.ServiceState{Installed: true, Running: true, Path: "/fake/com.latem.agent.plist", Revision: system.ServiceRevision}
	printf(w, "  [ok  ] installed (fake)\n")
	return nil
}

func (f *fakeService) Restart(_ context.Context, w io.Writer) error {
	f.restart++
	f.state.Running = true
	printf(w, "  [ok  ] restarted (fake)\n")
	return nil
}

type setupRecorder struct {
	svc       *fakeService
	registers int
	doctors   int
	docker    bool
	machine   *fakeMachine
}

// fakeSetup routes setup's outward steps to fakes: registration still runs
// the real device flow, against whatever --url the test passes.
func fakeSetup(t *testing.T, docker bool) *setupRecorder {
	t.Helper()
	rec := &setupRecorder{svc: &fakeService{}, docker: docker, machine: newFakeMachine()}
	prev := newSetupDeps
	newSetupDeps = func() setupDeps {
		return setupDeps{
			dockerAnswers: func(context.Context) bool { return rec.docker },
			service:       rec.svc,
			register: func(cmd *cobra.Command, f registerFlags) error {
				rec.registers++
				return runRegister(cmd, f)
			},
			doctor: func(cmd *cobra.Command) error {
				rec.doctors++
				printf(cmd.OutOrStdout(), "  (doctor ran)\n")
				return nil
			},
			toolchains: rec.machine.deps(),
			measure: func(context.Context) capacity.Machine {
				return capacity.Machine{CPUs: 8, MemoryGB: 16, Docker: &capacity.Docker{CPUs: 8, MemoryGB: 8}}
			},
		}
	}
	t.Cleanup(func() { newSetupDeps = prev })
	// The toolchains step reads every configured repository; a test that
	// cares what is in one says so with its own fakeRepoFiles.
	fakeRepoFiles(t, fstest.MapFS{})
	return rec
}

func TestSetup_AFreshMachineEndToEnd(t *testing.T) {
	cp := newControlPlane(t, "authorization_pending", "token")
	noSleep(t)
	rec := fakeSetup(t, true)
	dir := filepath.Join(privateTempDir(t), "fresh")

	out, err := latemIn(t, dir, "", "setup", "--url", cp.agentURL(), "--name", "build-01", "--image", "ghcr.io/me/ci:1")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg := loadConfigIn(t, dir)

	var names []string
	for _, r := range cfg.Runners {
		names = append(names, r.Name+"/"+string(r.Executor))
	}
	// Sized from the machine (A63): fakeSetup's Docker has 8 CPUs and 8 GB,
	// room for two jobs of 2 CPUs and 4 GB.
	if r, ok := cfg.Runner("linux"); !ok || r.Concurrency != 2 || !strings.Contains(out, "linux runner: concurrency 2 — Docker has 8 CPUs and 8 GB") {
		t.Errorf("the docker runner was not sized from the machine:\n%s", out)
	}
	want := "linux/docker"
	if runtime.GOOS == "darwin" {
		want = "macos/native linux/docker"
	}
	if got := strings.Join(names, " "); got != want {
		t.Errorf("runners = %s, want %s", got, want)
	}
	if r, ok := cfg.Runner("linux"); !ok || r.Docker.DefaultImage != "ghcr.io/me/ci:1" {
		t.Errorf("--image did not reach the docker runner: %+v", r)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("setup invented repos: %v", cfg.Repos)
	}
	if cfg.Machine.Name != "build-01" || cfg.Machine.ID != "mch_claimed" || cfg.Broker.Remote.URL != cp.agentURL() {
		t.Errorf("machine = %+v, remote = %+v", cfg.Machine, cfg.Broker.Remote)
	}
	if rec.registers != 1 || rec.svc.installs != 1 || rec.svc.restart != 0 || rec.doctors != 1 {
		t.Errorf("register %d, install %d, restart %d, doctor %d; want 1 1 0 1",
			rec.registers, rec.svc.installs, rec.svc.restart, rec.doctors)
	}
	for _, want := range []string{"1/6 config", "wrote", "3/6 toolchains", "4/6 register", "approved", "5/6 service", "6/6 doctor",
		"(doctor ran)", "latem repo add <owner/name>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// Setup starts the service itself, so register's "restart the agent"
	// would be advice to do something setup is about to do.
	if strings.Contains(out, "restart the agent to connect") {
		t.Error("register told the operator to restart the agent during setup")
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}

	// And again: everything is done, so nothing is.
	before := readConfigFile(t, dir)
	out, err = latemIn(t, dir, "", "setup", "--url", cp.agentURL())
	if err != nil {
		t.Fatalf("second setup: %v\n%s", err, out)
	}
	if readConfigFile(t, dir) != before {
		t.Error("a second setup rewrote config.toml")
	}
	if rec.registers != 1 || rec.svc.installs != 1 || rec.svc.restart != 0 || rec.doctors != 2 {
		t.Errorf("second run: register %d, install %d, restart %d, doctor %d; want 1 1 0 2",
			rec.registers, rec.svc.installs, rec.svc.restart, rec.doctors)
	}
	for _, want := range []string{"exists; left as it is", "registered with " + cp.agentURL() + " as mch_claimed", "installed and running"} {
		if !strings.Contains(out, want) {
			t.Errorf("second run lacks %q:\n%s", want, out)
		}
	}
}

// A machine whose service is already installed, registering for the first
// time: the agent has to be restarted to read [broker.remote].
func TestSetup_RestartsAServiceWhoseConfigChanged(t *testing.T) {
	cp := newControlPlane(t, "token")
	noSleep(t)
	rec := fakeSetup(t, false)
	rec.svc.state = system.ServiceState{Installed: true, Running: true, Revision: system.ServiceRevision}
	dir := configDirWith(t, baseConfig)

	if out, err := latemIn(t, dir, "", "setup", "--url", cp.agentURL()); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if rec.svc.restart != 1 || rec.svc.installs != 0 {
		t.Errorf("restart %d, install %d; want a restart", rec.svc.restart, rec.svc.installs)
	}
	if !strings.Contains(readConfigFile(t, dir), "# The operator's own note") {
		t.Error("setup rewrote an existing config")
	}
}

// A service an older latem installed is rewritten, not just restarted: a
// restart keeps the definition, and before revision 2 that was a LaunchAgent
// macOS throttled.
func TestSetup_RewritesAnOutdatedService(t *testing.T) {
	rec := fakeSetup(t, false)
	rec.svc.state = system.ServiceState{Installed: true, Running: true, Path: "/fake/com.latem.agent.plist", Revision: 1}
	dir := configDirWith(t, baseConfig)

	out, err := latemIn(t, dir, "", "setup", "--no-register")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if rec.svc.installs != 1 || rec.svc.restart != 0 {
		t.Errorf("install %d, restart %d; want the definition rewritten", rec.svc.installs, rec.svc.restart)
	}
	if !strings.Contains(out, "is revision 1; writing revision 2") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetup_LocalOnly(t *testing.T) {
	rec := fakeSetup(t, false)
	dir := filepath.Join(privateTempDir(t), "local")

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if rec.registers != 0 || rec.svc.installs != 0 || rec.svc.restart != 0 || rec.doctors != 1 {
		t.Errorf("register %d, install %d, restart %d, doctor %d", rec.registers, rec.svc.installs, rec.svc.restart, rec.doctors)
	}
	cfg := loadConfigIn(t, dir)
	if cfg.Broker.Remote.Enabled {
		t.Error("--no-register registered")
	}
	if runtime.GOOS == "darwin" {
		if _, ok := cfg.Runner("linux"); ok {
			t.Error("a docker runner was written though no daemon answered")
		}
		if !strings.Contains(out, "no Docker daemon answered") {
			t.Errorf("setup did not say why there is no linux runner:\n%s", out)
		}
	}
}

// Setup stops at a config the agent could not start on, rather than
// installing a service that would fail at once.
func TestSetup_StopsAtAConfigThatDoesNotLoad(t *testing.T) {
	rec := fakeSetup(t, false)
	dir := configDirWith(t, baseConfig+"\n[repos.broken]\nurl = 7\n")

	_, err := latemIn(t, dir, "", "setup", "--no-register")
	if err == nil || !strings.Contains(err.Error(), "setup stopped here") {
		t.Fatalf("want a stop, got %v", err)
	}
	if rec.svc.installs+rec.svc.restart+rec.doctors != 0 {
		t.Error("setup went on past a config that does not load")
	}
}

// The template setup writes loads on every platform, with and without the
// docker runner, and with no repository.
func TestSetupTemplate_LoadsWithNoRepos(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		for _, docker := range []bool{false, true} {
			body := renderInitConfig(initParams{Machine: "m", GOOS: goos, GOARCH: "arm64", Refs: []string{"main"}, Docker: docker})
			cfg, err := config.Parse([]byte(body), config.Options{Dir: shortDir(t)})
			if err != nil {
				t.Errorf("%s docker=%v: %v\n%s", goos, docker, err, body)
				continue
			}
			wantRunners := 1
			if goos == "darwin" && docker {
				wantRunners = 2
			}
			if len(cfg.Runners) != wantRunners {
				t.Errorf("%s docker=%v: %d runners, want %d", goos, docker, len(cfg.Runners), wantRunners)
			}
			if !strings.Contains(body, "latem repo add") {
				t.Errorf("%s: the empty allowlist does not say how to fill it", goos)
			}
		}
	}
}

// fakeInteractive says stdin is a terminal, so setup asks.
func fakeInteractive(t *testing.T) {
	t.Helper()
	prev := interactive
	interactive = func(*cobra.Command) bool { return true }
	t.Cleanup(func() { interactive = prev })
}

// a workflow that needs an alias and an allowlist entry on a fresh machine.
var setupRepoFiles = fstest.MapFS{
	".github/workflows/checks.yml": {Data: []byte("on: push\njobs:\n  web:\n    runs-on: blacksmith-4vcpu-ubuntu-2404\n" +
		"    steps:\n      - uses: oven-sh/setup-bun@v2\n      - run: bun test\n")},
}

// On a terminal, with no repositories, setup asks for one, reads its
// workflows without a checkout, and offers what they need (A57).
func TestSetup_AsksForARepositoryAndOffersWhatItsWorkflowsNeed(t *testing.T) {
	fakeSetup(t, true)
	fakeInteractive(t)
	fakeLsRemote(t, readable("main"))
	fakeStores(t, false)
	noAgentRunning(t)
	asked := fakeRepoFiles(t, setupRepoFiles)
	dir := filepath.Join(privateTempDir(t), "fresh")

	out, err := latemIn(t, dir, "nope\nme/app\ny\n\n", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg := loadConfigIn(t, dir)
	if _, ok := cfg.Repos["app"]; !ok {
		t.Fatalf("repos = %v, want app", cfg.Repos)
	}
	if got := cfg.LabelAliases["blacksmith-4vcpu-ubuntu-2404"]; !slices.Equal(got, []string{"linux"}) {
		t.Errorf("alias = %v, want [linux]", got)
	}
	if !slices.Contains(cfg.Actions.Allowed, "oven-sh/setup-bun") {
		t.Errorf("actions.allowed = %v", cfg.Actions.Allowed)
	}
	if len(*asked) == 0 || slices.ContainsFunc(*asked, func(s treeSource) bool { return s != fromMirrorThenRemote }) {
		t.Errorf("read from %v; a fresh machine has no mirror, so the remote", *asked)
	}
	// Checked again once the entries are in: the job is refused next for its
	// unpinned action, a setting setup leaves alone and says.
	for _, want := range []string{"2/6 repositories", "should build (owner/name)", "nope", "added repos.app",
		"jobs in app can run on this machine", "its workflows need", "add them? [Y/n]", "added 2 entries",
		"now 0 of 1 can run", `"v2" is a tag or branch`, "which setup leaves to you",
		"[actions] require_pinned = false", "another (owner/name)", "4/6 register"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// --repo adds without asking; what the workflows need is said, not written.
func TestSetup_RepoFlagAddsWithoutAsking(t *testing.T) {
	fakeSetup(t, true)
	fakeLsRemote(t, readable("main"))
	fakeStores(t, false)
	noAgentRunning(t)
	fakeRepoFiles(t, setupRepoFiles)
	dir := filepath.Join(privateTempDir(t), "fresh")

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--repo", "me/app")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg := loadConfigIn(t, dir)
	if _, ok := cfg.Repos["app"]; !ok || len(cfg.LabelAliases) != 0 {
		t.Errorf("repos = %v, aliases = %v: want app added and nothing else written", cfg.Repos, cfg.LabelAliases)
	}
	if strings.Contains(out, "add them?") || !strings.Contains(out, "latem workflows check --repo app --fix") {
		t.Errorf("output:\n%s", out)
	}

	if _, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service", "--repo", "not a slug"); exitCode(err) != ExitUsage {
		t.Errorf("--repo 'not a slug': exit %d (%v), want usage", exitCode(err), err)
	}
}

// Off a terminal, with no --repo, setup asks nothing and says what adds one.
func TestSetup_NoRepositoriesOffATerminal(t *testing.T) {
	fakeSetup(t, true)
	dir := filepath.Join(privateTempDir(t), "fresh")
	out, err := latemIn(t, dir, "me/app\n", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "none yet; `latem repo add <owner/name>` adds one") || strings.Contains(out, "should build") {
		t.Errorf("output:\n%s", out)
	}
}
