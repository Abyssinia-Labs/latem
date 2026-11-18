package dockerhost

import (
	"testing"
)

const colima = "unix:///Users/me/.colima/default/docker.sock"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// config.toml's [docker] host first, then DOCKER_HOST, then the system socket
// (A111). The second is what a LaunchAgent edited by hand gives the agent, and
// the first is what `latem service install` cannot drop.
func TestResolve_ConfigThenDOCKER_HOSTThenTheSystemSocket(t *testing.T) {
	for _, c := range []struct {
		name, configured string
		env              map[string]string
		want             string
		from             Source
	}{
		{"config over DOCKER_HOST", colima, map[string]string{"DOCKER_HOST": "unix:///other.sock"}, colima, FromConfig},
		{"config alone", colima, nil, colima, FromConfig},
		{"DOCKER_HOST", "", map[string]string{"DOCKER_HOST": "unix:///other.sock"}, "unix:///other.sock", FromEnv},
		{"an empty DOCKER_HOST is none", "", map[string]string{"DOCKER_HOST": ""}, "unix:///var/run/docker.sock", FromSystem},
		{"the system socket", "", nil, "unix:///var/run/docker.sock", FromSystem},
	} {
		host, from := Resolve(c.configured, env(c.env))
		if host != c.want || from != c.from {
			t.Errorf("%s: Resolve = %q, %s; want %q, %s", c.name, host, from, c.want, c.from)
		}
	}
	if host, from := Resolve("", nil); host != SystemHost || from != FromSystem {
		t.Errorf("nil getenv: %q, %s", host, from)
	}
}

// The client a caller gets dials what Resolve names, in the same order,
// whatever DOCKER_HOST the process inherited.
func TestNewClient_DialsWhatResolveNames(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///from/env.sock")
	for configured, want := range map[string]string{
		colima: colima,
		"":     "unix:///from/env.sock",
	} {
		cli, err := NewClient(configured)
		if err != nil {
			t.Fatalf("NewClient(%q): %v", configured, err)
		}
		if got := cli.DaemonHost(); got != want {
			t.Errorf("NewClient(%q) dials %q, want %q", configured, got, want)
		}
		_ = cli.Close()
	}

	t.Setenv("DOCKER_HOST", "")
	cli, err := NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	if got := cli.DaemonHost(); got != SystemHost {
		t.Errorf("with nothing set, dials %q", got)
	}
}

// A stale DOCKER_HOST the agent inherited cannot stop a configured client
// from being built: it is not read at all.
func TestNewClient_AConfiguredHostIgnoresABrokenDOCKER_HOST(t *testing.T) {
	t.Setenv("DOCKER_HOST", "not a url")
	if _, err := NewClient(""); err == nil {
		t.Skip("this SDK accepts the value; nothing to show")
	}
	cli, err := NewClient(colima)
	if err != nil {
		t.Fatalf("NewClient(%q): %v", colima, err)
	}
	_ = cli.Close()
}

func TestStepEnv(t *testing.T) {
	if got := StepEnv(""); got != nil {
		t.Errorf("StepEnv(\"\") = %v", got)
	}
	if got := StepEnv(colima); len(got) != 1 || got["DOCKER_HOST"] != colima {
		t.Errorf("StepEnv = %v", got)
	}
}

func TestCandidates_AreEachEnginesSocket(t *testing.T) {
	got := Candidates("/Users/me")
	want := []string{
		colima,
		"unix:///Users/me/.orbstack/run/docker.sock",
		"unix:///Users/me/.kiri/run/docker.sock",
		"unix:///Users/me/.rd/docker.sock",
		"unix:///Users/me/.docker/run/docker.sock",
	}
	if len(got) != len(want) {
		t.Fatalf("Candidates = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Candidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if Candidates("") != nil {
		t.Error("no home, and still candidates")
	}
}
