package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor/docker"
	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

const (
	colimaSock = "unix:///Users/me/.colima/default/docker.sock"
	systemSock = "unix:///var/run/docker.sock"
)

// fakeDockerDeps answers a ping at each host in answering, finds a socket's
// file for each in present, and reports context as the `docker` command's.
func fakeDockerDeps(cli string, answering []string, present ...string) (dockerDeps, *[]string) {
	var pinged []string
	return dockerDeps{
		ping: func(_ context.Context, host string) bool {
			pinged = append(pinged, host)
			return slices.Contains(answering, host)
		},
		context: func(context.Context) string { return cli },
		home:    "/Users/me",
		exists:  func(p string) bool { return slices.Contains(present, "unix://"+p) },
	}, &pinged
}

func noEnv(string) string { return "" }

// The Mac Studio of 2026-11-18: Colima answers at its own socket, nothing at
// the system's, and the context is Colima's. The agent's daemon does not
// answer, and Colima's socket is found (A111).
func TestAgentDocker_FindsColimasSocket(t *testing.T) {
	deps, _ := fakeDockerDeps(colimaSock, []string{colimaSock}, colimaSock)
	d := probeAgentDocker(t.Context(), "", noEnv, deps)
	if d.Answers || d.Host != systemSock || d.FromConfig || d.FromEnv || d.Found != colimaSock || d.CLIHost != colimaSock {
		t.Errorf("docker = %+v", d)
	}

	// No context to say so, and Colima's socket is still where Colima puts it.
	deps, _ = fakeDockerDeps("", []string{colimaSock}, colimaSock)
	if d := probeAgentDocker(t.Context(), "", noEnv, deps); d.Found != colimaSock {
		t.Errorf("without a context: %+v", d)
	}

	// A socket that is not there is not asked.
	deps, pinged := fakeDockerDeps("", nil)
	if d := probeAgentDocker(t.Context(), "", noEnv, deps); d.Found != "" || !slices.Equal(*pinged, []string{systemSock}) {
		t.Errorf("found %q, pinged %q", d.Found, *pinged)
	}
}

// Once written, [docker] host is where the agent looks, before DOCKER_HOST,
// and an answer there is the end of it.
func TestAgentDocker_AsksAtDockerHostFirst(t *testing.T) {
	deps, pinged := fakeDockerDeps(colimaSock, []string{colimaSock}, colimaSock)
	env := func(k string) string { return map[string]string{"DOCKER_HOST": "unix:///other.sock"}[k] }
	d := probeAgentDocker(t.Context(), colimaSock, env, deps)
	if !d.Answers || !d.FromConfig || d.Host != colimaSock || !slices.Equal(*pinged, []string{colimaSock}) {
		t.Errorf("docker = %+v, pinged %q", d, *pinged)
	}
	deps, _ = fakeDockerDeps("", []string{"unix:///other.sock"})
	if d := probeAgentDocker(t.Context(), "", env, deps); !d.Answers || !d.FromEnv || d.Host != "unix:///other.sock" {
		t.Errorf("DOCKER_HOST: %+v", d)
	}
}

// The daemon a new config.toml's docker runner uses: the system socket's,
// written as nothing; this shell's DOCKER_HOST, written down because the
// service will not have it; an engine's own socket, written down; or none.
func TestFindDocker(t *testing.T) {
	deps, _ := fakeDockerDeps("", []string{systemSock}, colimaSock)
	if h, ok := findDockerWith(t.Context(), noEnv, deps); h != "" || !ok {
		t.Errorf("system socket: %q, %v", h, ok)
	}
	env := func(k string) string { return map[string]string{"DOCKER_HOST": colimaSock}[k] }
	deps, _ = fakeDockerDeps("", []string{colimaSock}, colimaSock)
	if h, ok := findDockerWith(t.Context(), env, deps); h != colimaSock || !ok {
		t.Errorf("DOCKER_HOST: %q, %v", h, ok)
	}
	deps, _ = fakeDockerDeps(colimaSock, []string{colimaSock}, colimaSock)
	if h, ok := findDockerWith(t.Context(), noEnv, deps); h != colimaSock || !ok {
		t.Errorf("colima: %q, %v", h, ok)
	}
	deps, _ = fakeDockerDeps(colimaSock, nil, colimaSock)
	if h, ok := findDockerWith(t.Context(), noEnv, deps); h != "" || ok {
		t.Errorf("nothing: %q, %v", h, ok)
	}
}

// `latem setup` on a fresh Mac running Colima writes [docker] host and the
// linux docker runner itself, sized from the daemon at that socket (A111).
func TestSetup_AFreshMacOnColimaWritesDockerHostAndTheLinuxRunner(t *testing.T) {
	rec := fakeSetup(t, true)
	rec.dockerHost = colimaSock
	noAgentRunning(t)
	dir := filepath.Join(privateTempDir(t), "fresh")

	out, err := latemIn(t, dir, "", "setup", "--no-register", "--no-service")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg := loadConfigIn(t, dir)
	if cfg.Docker.Host != colimaSock {
		t.Errorf("[docker] host = %q\n%s", cfg.Docker.Host, readConfigFile(t, dir))
	}
	if r, ok := cfg.Runner("linux"); !ok || r.Executor != config.ExecutorDocker {
		t.Errorf("no linux docker runner:\n%s", readConfigFile(t, dir))
	}
	if rec.measuredAt != colimaSock {
		t.Errorf("measured Docker at %q", rec.measuredAt)
	}
	if !strings.Contains(out, `docker: [docker] host = "`+colimaSock+`", where Colima answers`) {
		t.Errorf("setup did not say where Docker is:\n%s", out)
	}
	if !strings.Contains(readConfigFile(t, dir), "in Colima's Linux VM") {
		t.Errorf("the runner's comment names another engine:\n%s", readConfigFile(t, dir))
	}
}

// A Mac whose config.toml has no docker runner, with Colima answering at its
// own socket: `setup apply no-linux-runner` writes the host and the runner,
// where it used to say to do both by hand (A111).
func TestSetupApply_NoLinuxRunnerWritesDockerHostAndTheRunner(t *testing.T) {
	rec := fakeSetup(t, true)
	rec.machine.docker = &readiness.Docker{Host: systemSock, Found: colimaSock, CLIHost: colimaSock}
	stubDockerSlots(t, 3)
	dir := macConfigDir(t, false)
	fakeReload(t, func(config.Paths) (agent.ReloadStatus, error) {
		return agent.ReloadStatus{Version: "v1", Deferred: []string{`runner "linux" was added`}}, nil
	})
	fakeSwitch(t, func(string) (agent.ReloadStatus, error) { return agent.ReloadStatus{Version: "v1"}, nil })

	_, stderr, err := applyIn(t, dir, readiness.ItemNoLinuxRunner)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	cfg := loadConfigIn(t, dir)
	r, ok := cfg.Runner(readiness.LinuxRunner)
	if cfg.Docker.Host != colimaSock || !ok || r.Executor != config.ExecutorDocker || r.Concurrency != 3 ||
		!slices.Contains(r.Labels, "linux") {
		t.Errorf("docker %+v, runner %+v\n%s", cfg.Docker, r, readConfigFile(t, dir))
	}
	if !strings.Contains(stderr, "no-linux-runner: wrote [docker] host = \""+colimaSock+"\"; a docker runner named linux") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// A docker runner whose daemon does not answer at the system socket, with
// Colima answering at its own: `setup apply docker-agent` writes [docker]
// host, and nothing goes into the service definition (A111).
func TestSetupApply_DockerAgentWritesDockerHost(t *testing.T) {
	rec := fakeSetup(t, true)
	rec.machine.docker = &readiness.Docker{Asked: true, Host: systemSock, Found: colimaSock}
	dir := macConfigDir(t, true)
	noAgentRunning(t)

	_, stderr, err := applyIn(t, dir, readiness.ItemDockerAgent)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if got := loadConfigIn(t, dir).Docker.Host; got != colimaSock {
		t.Errorf("[docker] host = %q\n%s", got, stderr)
	}
	if rec.svc.installs != 0 {
		t.Error("the service was installed")
	}
}

// setup --json says the agent's dead socket is in Linux's way, and that
// writing Colima's as [docker] host closes it.
func TestSetup_JSONSaysTheAgentsDockerIsDead(t *testing.T) {
	dir, rec := setupMachine(t, nil)
	rec.machine.docker = &readiness.Docker{Asked: true, Host: systemSock,
		Link: "/Users/me/.docker/run/docker.sock", CLIHost: colimaSock, Found: colimaSock}
	plan := setupJSON(t, dir)
	i := slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return k.Name == needs.KindLinux })
	if i < 0 {
		t.Fatalf("kinds = %+v", plan.Kinds)
	}
	k := plan.Kinds[i]
	if k.State != readiness.Missing || !slices.ContainsFunc(k.Items, func(it readiness.Item) bool {
		return it.ID == readiness.ItemDockerAgent && len(it.Config) == 1 && it.Config[0].Value == colimaSock
	}) {
		t.Errorf("linux = %+v", k)
	}
}

// setupJSON is `latem setup --json` in dir, read back.
func setupJSON(t *testing.T, dir string) readiness.Plan {
	t.Helper()
	out, err := latemIn(t, dir, "", "setup", "--json")
	if err != nil {
		t.Fatalf("setup --json: %v\n%s", err, out)
	}
	var plan readiness.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return plan
}

// doctor's docker.daemon names the config setting, written by setup apply,
// and neither a link under sudo nor an edit to the service (A111).
func TestDoctor_ADeadAgentSocketNamesDockerHost(t *testing.T) {
	dir := privateDir(t)
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{"docker --version": {out: "Docker version 29.0.0, build x"}}, testConfig(dir), nil)
	d.newDocker = (&fakeDocker{preflightErr: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")}).factory()
	d.agentDocker = func(context.Context, string) readiness.Docker {
		return readiness.Docker{Asked: true, Host: systemSock, Link: "/Users/me/.docker/run/docker.sock",
			CLIHost: colimaSock, Found: colimaSock}
	}
	c := checkByID(d.checkDocker(t.Context()), "docker.daemon")
	if c == nil || c.Level != levelFail {
		t.Fatalf("docker.daemon = %+v", c)
	}
	want := []string{`latem setup apply docker-agent   # writes [docker] host = "` + colimaSock + `"`}
	if !slices.Equal(c.Fix, want) || !strings.Contains(c.Detail, "Colima answers at /Users/me/.colima/default/docker.sock") {
		t.Errorf("docker.daemon = %+v", c)
	}
	for _, s := range append(c.Fix, c.Manual...) {
		if strings.Contains(s, "sudo") || strings.Contains(s, "DOCKER_HOST") {
			t.Errorf("a link or a service edit: %q", s)
		}
	}
}

// With [docker] host set, doctor's client dials it, and a native step is
// given it, so the `docker` command's context is not the agent's problem.
func TestDoctor_DockerHostIsWhereDoctorLooks(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Docker.Host = colimaSock
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{"docker --version": {out: "Docker version 29.0.0, build x"}}, cfg, nil)
	fd := &fakeDocker{}
	var hosts []string
	inner := fd.factory()
	d.newDocker = func(o docker.Options) (dockerProbe, error) {
		hosts = append(hosts, o.Host)
		return inner(o)
	}
	checks := d.checkDocker(t.Context())
	if len(hosts) == 0 || hosts[0] != colimaSock {
		t.Errorf("doctor dialled %q", hosts)
	}
	if c := checkByID(checks, "docker.daemon"); c == nil || !strings.Contains(c.Detail, "[docker] host, "+colimaSock) {
		t.Errorf("docker.daemon = %+v", c)
	}
	if c := checkByID(checks, "docker.context"); c == nil || c.Level != levelPass || !strings.Contains(c.Detail, "DOCKER_HOST="+colimaSock) {
		t.Errorf("docker.context = %+v", c)
	}
}

// A DOCKER_HOST added to the LaunchAgent by hand is named, with the command
// that moves it to config.toml, until config.toml has one (A111).
func TestDoctor_AServiceDOCKER_HOSTIsNamedUntilConfigHasIt(t *testing.T) {
	dir := privateDir(t)
	cfg := testConfig(dir)
	d := newTestDoctor("darwin", dir, nil, cfg, nil)
	st := system.ServiceState{Installed: true, Path: "/Users/me/Library/LaunchAgents/com.latem.agent.plist", DockerHost: colimaSock}
	got := d.checkServiceDockerHost(st)
	if len(got) != 1 || got[0].Level != levelWarn ||
		!slices.Equal(got[0].Fix, []string{`latem setup apply docker-host   # writes [docker] host = "` + colimaSock + `" to config.toml`}) {
		t.Errorf("checks = %+v", got)
	}
	cfg.Docker.Host = colimaSock
	if got := d.checkServiceDockerHost(st); len(got) != 0 {
		t.Errorf("with [docker] host set: %+v", got)
	}
}

// `latem service install` carries a hand-added DOCKER_HOST to config.toml
// before it rewrites the definition without it: the move from a LaunchAgent
// edited by hand to [docker] host is one install (A111).
func TestServiceInstall_CarriesAHandAddedDOCKER_HOSTToConfig(t *testing.T) {
	home := privateTempDir(t)
	dir := macConfigDir(t, true)
	prev := global.configDir
	global.configDir = dir
	t.Cleanup(func() { global.configDir = prev })
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.latem.agent.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<dict>\n<key>DOCKER_HOST</key>\n<string>"+colimaSock+"</string>\n</dict>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sys := system.New(system.Options{GOOS: "darwin", Home: home})

	var out strings.Builder
	if err := carryDockerHost(sys, &out, true); err != nil {
		t.Fatal(err)
	}
	if loadConfigIn(t, dir).Docker.Host != "" || !strings.Contains(out.String(), "would write [docker] host") {
		t.Errorf("a dry run wrote, or said nothing: %q", out.String())
	}
	out.Reset()
	if err := carryDockerHost(sys, &out, false); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigIn(t, dir).Docker.Host; got != colimaSock {
		t.Errorf("[docker] host = %q\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "carried DOCKER_HOST from the installed definition") {
		t.Errorf("out = %q", out.String())
	}
	// Once there, a second install leaves config.toml as it is.
	before := readConfigFile(t, dir)
	out.Reset()
	if err := carryDockerHost(sys, &out, false); err != nil || readConfigFile(t, dir) != before || out.Len() != 0 {
		t.Errorf("a second carry: %v, %q", err, out.String())
	}
}

// macConfigDir is a Mac's config.toml, setup's template, in a directory of
// its own: the native macos runner, and the linux docker runner when withDocker.
func macConfigDir(t *testing.T, withDocker bool) string {
	t.Helper()
	dir := filepath.Join(privateTempDir(t), "mac")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := renderInitConfig(initParams{Machine: "studio", GOOS: "darwin", GOARCH: "arm64", Refs: []string{"main"}, Docker: withDocker})
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubDockerSlots makes a docker runner setup adds take n jobs at once,
// measuring nothing.
func stubDockerSlots(t *testing.T, n int) {
	t.Helper()
	prev := dockerRunnerSlots
	dockerRunnerSlots = func(*config.Config, string) int { return n }
	t.Cleanup(func() { dockerRunnerSlots = prev })
}
