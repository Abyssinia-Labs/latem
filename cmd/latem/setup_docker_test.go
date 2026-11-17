package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
)

func TestHelperDisabled(t *testing.T) {
	for out, want := range map[string]bool{
		"disabled services = {\n\t\t\"com.docker.socket\" => disabled\n\t\t\"com.docker.vmnetd\" => enabled\n\t}\n": true,
		"disabled services = {\n\t\t\"com.docker.socket\" => true\n\t}\n":                                           true,
		"disabled services = {\n\t\t\"com.docker.socket\" => enabled\n\t}\n":                                        false,
		"disabled services = {\n\t\t\"com.docker.vmnetd\" => disabled\n\t}\n":                                       false,
		"": false,
	} {
		if got := helperDisabled(out); got != want {
			t.Errorf("helperDisabled(%q) = %v, want %v", out, got, want)
		}
	}
}

// setup --json asks the agent's daemon when a docker runner is configured,
// and a dead one is in Linux's way.
func TestSetup_JSONSaysTheAgentsDockerIsDead(t *testing.T) {
	dir, rec := setupMachine(t, nil)
	rec.machine.docker = &readiness.Docker{Asked: true, Host: "unix:///var/run/docker.sock",
		Link: "/Users/me/.docker/run/docker.sock", CLIHost: "unix:///Users/me/.kiri/run/docker.sock"}
	plan := setupJSON(t, dir)
	i := slices.IndexFunc(plan.Kinds, func(k readiness.Kind) bool { return k.Name == needs.KindLinux })
	if i < 0 {
		t.Fatalf("kinds = %+v", plan.Kinds)
	}
	k := plan.Kinds[i]
	if k.State != readiness.Missing || !slices.ContainsFunc(k.Items, func(it readiness.Item) bool {
		return it.ID == readiness.ItemDockerAgent && slices.Contains(commandLines(it.Yours), "kiri setup --docker-socket")
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

// doctor's docker.daemon names the engine's fix, as the plan does, and not
// Docker Desktop's `open -a Docker` on a Mac that runs Kiri (A100).
func TestDoctor_ADeadAgentSocketNamesTheEnginesFix(t *testing.T) {
	dir := privateDir(t)
	d := newTestDoctor("darwin", dir, map[string]fakeCmd{"docker --version": {out: "Docker version 29.0.0, build x"}}, testConfig(dir), nil)
	d.newDocker = (&fakeDocker{preflightErr: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")}).factory()
	d.agentDocker = func(context.Context) readiness.Docker {
		return readiness.Docker{Asked: true, Host: "unix:///var/run/docker.sock", Link: "/Users/me/.docker/run/docker.sock",
			CLIHost: "unix:///Users/me/.kiri/run/docker.sock", DesktopHelper: true}
	}
	c := checkByID(d.checkDocker(t.Context()), "docker.daemon")
	if c == nil || c.Level != levelFail {
		t.Fatalf("docker.daemon = %+v", c)
	}
	want := []string{"sudo launchctl disable system/com.docker.socket", "sudo rm /var/run/docker.sock", "kiri setup --docker-socket"}
	if !slices.Equal(c.Fix, want) || !strings.Contains(c.Detail, "com.docker.socket") {
		t.Errorf("docker.daemon = %+v", c)
	}
}
