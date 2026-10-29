package main

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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
	f.state = system.ServiceState{Installed: true, Running: true, Path: "/fake/com.latem.agent.plist"}
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
}

// fakeSetup routes setup's outward steps to fakes: registration still runs
// the real device flow, against whatever --url the test passes.
func fakeSetup(t *testing.T, docker bool) *setupRecorder {
	t.Helper()
	rec := &setupRecorder{svc: &fakeService{}, docker: docker}
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
		}
	}
	t.Cleanup(func() { newSetupDeps = prev })
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
	for _, want := range []string{"1/4 config", "wrote", "2/4 register", "approved", "3/4 service", "4/4 doctor",
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
	rec.svc.state = system.ServiceState{Installed: true, Running: true}
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
