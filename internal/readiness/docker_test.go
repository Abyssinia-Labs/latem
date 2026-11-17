package readiness

import (
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/needs"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

var mac = system.Host{OS: "macOS", Arch: "arm64"}

const (
	desktopSock = "/Users/me/.docker/run/docker.sock"
	kiriHost    = "unix:///Users/me/.kiri/run/docker.sock"
)

// macbook is the MacBook of 2026-11-16: a docker runner, Kiri running and the
// `docker` command's context on it, and the system socket still Docker
// Desktop's dead link, which the agent dials.
func macbook(d Docker) Input {
	return Input{Machine: "macbook", Host: mac,
		Runners: []Runner{{Name: "linux", Executor: needs.ExecutorDocker, Labels: []string{"linux"}}},
		Jobs:    []Job{linuxJob()},
		Docker:  d}
}

func lines(argvs [][]string) []string {
	out := make([]string, len(argvs))
	for i, a := range argvs {
		out[i] = strings.Join(a, " ")
	}
	return out
}

// The agent's own socket is dead while the `docker` command answers through
// Kiri's context. Linux is in the way, with Kiri's fix, and the stale link
// goes first because kiri setup refuses while it is there.
func TestDocker_TheAgentsSocketDeadWhileTheContextAnswersIsInTheWay(t *testing.T) {
	k := kind(t, Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock",
		Link: desktopSock, CLIHost: kiriHost})), needs.KindLinux)
	if k.State != Missing {
		t.Errorf("state = %s, want missing", k.State)
	}
	it := item(t, k, ItemDockerAgent)
	if it.Soft || it.Have != "nothing answers at /var/run/docker.sock, a link to "+desktopSock {
		t.Errorf("item = %+v", it)
	}
	if got := lines(it.Yours); !slices.Equal(got, []string{"sudo rm /var/run/docker.sock", "kiri setup --docker-socket"}) {
		t.Errorf("yours = %q", got)
	}
	if !slices.Contains(it.Jobs, "tsedal checks.yml web") {
		t.Errorf("jobs = %q", it.Jobs)
	}
	if len(it.Manual) == 0 || !strings.Contains(it.Manual[len(it.Manual)-1], "DOCKER_HOST="+kiriHost) {
		t.Errorf("manual = %q", it.Manual)
	}
}

// Docker Desktop's helper puts the link back at every boot, so a fix by hand
// lasts until the next restart. It is named as the cause, and disabled first.
func TestDocker_DockerDesktopsHelperIsNamedAsTheCause(t *testing.T) {
	it := item(t, kind(t, Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock",
		Link: desktopSock, CLIHost: kiriHost, DesktopHelper: true})), needs.KindLinux), ItemDockerAgent)
	if !strings.Contains(it.Have, "com.docker.socket points it back at Docker Desktop at every boot") {
		t.Errorf("have = %q", it.Have)
	}
	want := []string{"sudo launchctl disable system/com.docker.socket", "sudo rm /var/run/docker.sock", "kiri setup --docker-socket"}
	if got := lines(it.Yours); !slices.Equal(got, want) {
		t.Errorf("yours = %q, want %q", got, want)
	}
}

// Each engine gets its own way of taking the system socket.
func TestDocker_EachEnginesFix(t *testing.T) {
	for _, c := range []struct {
		cli, yours, manual string
	}{
		{cli: "unix:///Users/me/.orbstack/run/docker.sock", manual: "OrbStack's settings"},
		{cli: "unix:///Users/me/.rd/docker.sock", manual: "Rancher Desktop's preferences"},
		{cli: "unix:///Users/me/.colima/default/docker.sock", yours: "sudo ln -sf /Users/me/.colima/default/docker.sock /var/run/docker.sock"},
		{cli: "unix://" + desktopSock, manual: "allow the default Docker socket to be used"},
	} {
		it, ok := DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock", CLIHost: c.cli}, mac)
		if !ok {
			t.Fatalf("%s: no item", c.cli)
		}
		if c.yours != "" && !slices.Contains(lines(it.Yours), c.yours) {
			t.Errorf("%s: yours = %q", c.cli, lines(it.Yours))
		}
		if c.manual != "" && !strings.Contains(strings.Join(it.Manual, "\n"), c.manual) {
			t.Errorf("%s: manual = %q", c.cli, it.Manual)
		}
		if !strings.Contains(strings.Join(it.Manual, "\n"), "DOCKER_HOST="+c.cli) {
			t.Errorf("%s: no DOCKER_HOST way: %q", c.cli, it.Manual)
		}
	}
}

// With nothing answering anywhere, the engine is not running: the one the
// link names, or on Linux the system's daemon.
func TestDocker_NothingAnswersAnywhere(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock", Link: desktopSock}, mac)
	if got := lines(it.Yours); !slices.Equal(got, []string{"open -a Docker"}) {
		t.Errorf("mac: yours = %q", got)
	}
	it, _ = DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock"}, arch)
	if got := lines(it.Yours); !slices.Equal(got, []string{"sudo systemctl start docker"}) {
		t.Errorf("linux: yours = %q", got)
	}
}

// A DOCKER_HOST that answers nothing is the service's setting, not the
// system socket's.
func TestDocker_ADeadDOCKER_HOST(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: "unix:///nowhere.sock", FromEnv: true, CLIHost: kiriHost}, mac)
	if it.Have != "nothing answers at DOCKER_HOST, unix:///nowhere.sock" || len(it.Yours) != 0 ||
		!strings.Contains(strings.Join(it.Manual, ""), "change DOCKER_HOST in the agent's service to "+kiriHost) {
		t.Errorf("item = %+v", it)
	}
}

// A daemon that answers is nothing in the way, and the proof asks the same
// daemon the agent does, not the context's.
func TestDocker_AnAnsweringDaemonIsNothingAndTheProofAsksIt(t *testing.T) {
	k := kind(t, Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock", Answers: true})), needs.KindLinux)
	if k.State != Ready || slices.ContainsFunc(k.Items, func(it Item) bool { return it.ID == ItemDockerAgent }) {
		t.Errorf("kind = %+v", k)
	}
	if len(k.Proof) != 1 || k.Proof[0].Env["DOCKER_HOST"] != "unix:///var/run/docker.sock" {
		t.Errorf("proof = %+v", k.Proof)
	}
}

// Nobody asked, which is a plan made without a probe: nothing is said.
func TestDocker_NotAskedSaysNothing(t *testing.T) {
	k := kind(t, Build(macbook(Docker{})), needs.KindLinux)
	if slices.ContainsFunc(k.Items, func(it Item) bool { return it.ID == ItemDockerAgent }) || k.Proof[0].Env != nil {
		t.Errorf("kind = %+v", k)
	}
}

func TestEngine(t *testing.T) {
	for host, want := range map[string]string{
		kiriHost: "Kiri",
		"unix:///Users/me/.orbstack/run/docker.sock":                          "OrbStack",
		"unix:///Users/me/.colima/default/docker.sock":                        "Colima",
		"unix:///Users/me/.rd/docker.sock":                                    "Rancher Desktop",
		"unix://" + desktopSock:                                               "Docker Desktop",
		"/Users/me/Library/Containers/com.docker.docker/Data/docker.raw.sock": "Docker Desktop",
		"unix:///var/run/docker.sock":                                         "",
		"tcp://10.0.0.2:2375":                                                 "",
	} {
		if got := Engine(host); got != want {
			t.Errorf("Engine(%s) = %q, want %q", host, got, want)
		}
	}
}

// The heartbeat says the item without its paths.
func TestDocker_TheAdvertSaysItWithoutAPath(t *testing.T) {
	a := Advert(Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock", Link: desktopSock, CLIHost: kiriHost})))
	for _, k := range a.Kinds {
		if k.Kind != needs.KindLinux {
			continue
		}
		for _, it := range k.Items {
			if it.Title == "Docker answering where the agent looks" {
				if it.Have != "nothing answers" {
					t.Errorf("have = %q", it.Have)
				}
				return
			}
		}
		t.Fatalf("items = %+v", k.Items)
	}
	t.Fatal("no linux kind")
}
