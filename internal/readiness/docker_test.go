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

const colimaHost = "unix:///Users/me/.colima/default/docker.sock"

// The agent's own socket is dead while Kiri answers at its own. Linux is in
// the way, and latem closes it by writing Kiri's socket as [docker] host: no
// sudo, no link for Docker Desktop's helper to put back, and nothing in the
// service definition for an install to drop (A111).
func TestDocker_TheAgentsSocketDeadWhileAnEngineAnswersWritesDockerHost(t *testing.T) {
	k := kind(t, Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock",
		Link: desktopSock, CLIHost: kiriHost, Found: kiriHost})), needs.KindLinux)
	if k.State != Missing {
		t.Errorf("state = %s, want missing", k.State)
	}
	it := item(t, k, ItemDockerAgent)
	if it.Soft || it.Have != "nothing answers at /var/run/docker.sock, a link to "+desktopSock+
		"; Kiri answers at /Users/me/.kiri/run/docker.sock" {
		t.Errorf("item = %+v", it)
	}
	if len(it.Yours) != 0 || len(it.Manual) != 0 {
		t.Errorf("a person's fix as well: yours %q, manual %q", lines(it.Yours), it.Manual)
	}
	if len(it.Config) != 1 || it.Config[0].What != ChangeDockerHost || it.Config[0].Value != kiriHost ||
		it.Config[0].Line != `[docker] host = "`+kiriHost+`"` {
		t.Errorf("config = %+v", it.Config)
	}
	if !slices.Contains(it.Jobs, "tsedal checks.yml web") {
		t.Errorf("jobs = %q", it.Jobs)
	}
}

// Whatever the engine, one that answers at its own socket is written down,
// and nothing asks for sudo.
func TestDocker_EveryEngineThatAnswersIsWrittenDown(t *testing.T) {
	for _, h := range []string{
		"unix:///Users/me/.orbstack/run/docker.sock", "unix:///Users/me/.rd/docker.sock",
		colimaHost, "unix://" + desktopSock, "unix:///Users/me/.config/podman/docker.sock",
	} {
		it, ok := DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock", CLIHost: h, Found: h}, mac)
		if !ok || len(it.Config) != 1 || it.Config[0].Value != h || len(it.Yours) != 0 {
			t.Errorf("%s: item = %+v", h, it)
		}
	}
}

// The context names an engine that does not answer either: it is not
// running, and starting it is the fix, after which setup writes it.
func TestDocker_TheContextsEngineStoppedIsStartedThenWritten(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock", CLIHost: colimaHost}, mac)
	if len(it.Config) != 0 || len(it.Yours) != 0 || len(it.Manual) != 1 ||
		!strings.HasPrefix(it.Manual[0], "start Colima, which the `docker` command's context names") ||
		!strings.Contains(it.Manual[0], "latem setup apply docker-agent") {
		t.Errorf("item = %+v", it)
	}
	if strings.Contains(strings.Join(it.Manual, ""), "sudo") || strings.Contains(strings.Join(it.Manual, ""), "service") {
		t.Errorf("a link or a service edit: %q", it.Manual)
	}
}

// With nothing answering anywhere, the engine is not running: the one the
// link names, or on Linux the system's daemon.
func TestDocker_NothingAnswersAnywhere(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock", Link: desktopSock}, mac)
	if got := lines(it.Yours); !slices.Equal(got, []string{"open -a Docker"}) {
		t.Errorf("mac: yours = %q", got)
	}
	it, _ = DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock"}, mac)
	if len(it.Manual) != 1 || !strings.Contains(it.Manual[0], "Colima") {
		t.Errorf("mac, no link: manual = %q", it.Manual)
	}
	it, _ = DockerAgentItem(Docker{Asked: true, Host: "unix:///var/run/docker.sock"}, arch)
	if got := lines(it.Yours); !slices.Equal(got, []string{"sudo systemctl start docker"}) {
		t.Errorf("linux: yours = %q", got)
	}
}

// A [docker] host that answers nothing is config.toml's to change, and one
// that another engine answers beside is changed to that.
func TestDocker_ADeadDockerHost(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: colimaHost, FromConfig: true}, mac)
	if it.Have != "nothing answers at [docker] host, "+colimaHost ||
		!strings.Contains(strings.Join(it.Manual, ""), "change [docker] host in config.toml") {
		t.Errorf("item = %+v", it)
	}
	it, _ = DockerAgentItem(Docker{Asked: true, Host: colimaHost, FromConfig: true, Found: kiriHost}, mac)
	if len(it.Config) != 1 || it.Config[0].Value != kiriHost {
		t.Errorf("item = %+v", it)
	}
}

// A DOCKER_HOST that answers nothing is moved past, to config.toml.
func TestDocker_ADeadDOCKER_HOST(t *testing.T) {
	it, _ := DockerAgentItem(Docker{Asked: true, Host: "unix:///nowhere.sock", FromEnv: true}, mac)
	if it.Have != "nothing answers at DOCKER_HOST, unix:///nowhere.sock" || len(it.Yours) != 0 ||
		!strings.Contains(strings.Join(it.Manual, ""), "set [docker] host in config.toml, which the agent reads before DOCKER_HOST") {
		t.Errorf("item = %+v", it)
	}
}

// The Mac Studio of 2026-11-18: Colima answers at the DOCKER_HOST added to the
// LaunchAgent by hand. Nothing is in the way today, so the item is soft, and
// it writes the address to config.toml, where an install cannot drop it.
func TestDocker_AnAnsweringDOCKER_HOSTIsMovedToConfig(t *testing.T) {
	k := kind(t, Build(macbook(Docker{Asked: true, Host: colimaHost, FromEnv: true, Answers: true})), needs.KindLinux)
	if k.State != Ready {
		t.Errorf("state = %s, want ready", k.State)
	}
	it := item(t, k, ItemDockerHost)
	if !it.Soft || len(it.Config) != 1 || it.Config[0].What != ChangeDockerHost || it.Config[0].Value != colimaHost {
		t.Errorf("item = %+v", it)
	}
	// Only a socket moves; config.toml takes nothing else.
	if _, ok := DockerHostItem(Docker{Asked: true, Host: "tcp://10.0.0.2:2376", FromEnv: true, Answers: true}); ok {
		t.Error("a tcp:// DOCKER_HOST was offered to config.toml")
	}
	if _, ok := DockerHostItem(Docker{Asked: true, Host: colimaHost, FromConfig: true, Answers: true}); ok {
		t.Error("[docker] host was offered to itself")
	}
}

// A machine with no Linux runner gets one written when a daemon answers, the
// agent's own or an engine's at its own socket, which is written first.
func TestDocker_NoLinuxRunnerIsWrittenWhenADaemonAnswers(t *testing.T) {
	in := func(d Docker) Input {
		return Input{Machine: "studio", Host: mac,
			Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos"}}},
			Jobs:    []Job{linuxJob()}, Docker: d}
	}
	it := item(t, kind(t, Build(in(Docker{Host: "unix:///var/run/docker.sock", Found: colimaHost})), needs.KindLinux), ItemNoLinuxRunner)
	if len(it.Config) != 2 || it.Config[0].What != ChangeDockerHost || it.Config[0].Value != colimaHost ||
		it.Config[1].What != ChangeDockerRunner || it.Config[1].Value != LinuxRunner || len(it.Manual) != 0 {
		t.Errorf("colima: item = %+v", it)
	}
	it = item(t, kind(t, Build(in(Docker{Host: "unix:///var/run/docker.sock", Answers: true})), needs.KindLinux), ItemNoLinuxRunner)
	if len(it.Config) != 1 || it.Config[0].What != ChangeDockerRunner {
		t.Errorf("answering: item = %+v", it)
	}
	it = item(t, kind(t, Build(in(Docker{Host: "unix:///var/run/docker.sock"})), needs.KindLinux), ItemNoLinuxRunner)
	if len(it.Config) != 0 || len(it.Manual) != 1 || !strings.Contains(it.Manual[0], "latem setup apply no-linux-runner") {
		t.Errorf("nothing: item = %+v", it)
	}
}

// A [docker] host reaches a native step as DOCKER_HOST, so the proof of a
// native job that runs docker asks there.
func TestDocker_ANativeProofAsksAtDockerHost(t *testing.T) {
	r := &Runner{Name: "macos", Executor: needs.ExecutorNative, Env: map[string]string{"JAVA_HOME": "/j"}}
	p := proofOf(definition{name: needs.KindLinuxDocker}, system.Toolchains{}, r, Docker{Host: colimaHost, FromConfig: true})
	if len(p) != 1 || p[0].Env["DOCKER_HOST"] != colimaHost || p[0].Env["JAVA_HOME"] != "/j" {
		t.Errorf("proof = %+v", p)
	}
	if _, set := r.Env["DOCKER_HOST"]; set {
		t.Error("the runner's own env was changed")
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
	a := Advert(Build(macbook(Docker{Asked: true, Host: "unix:///var/run/docker.sock", Link: desktopSock, CLIHost: kiriHost, Found: kiriHost})))
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

// Neither of A111's items sends a socket's path off the machine.
func TestDocker_TheAdvertSaysA111sItemsWithoutAPath(t *testing.T) {
	plans := []Plan{
		Build(macbook(Docker{Asked: true, Host: colimaHost, FromEnv: true, Answers: true})),
		Build(Input{Machine: "studio", Host: mac, Jobs: []Job{linuxJob()},
			Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos"}}},
			Docker:  Docker{Host: "unix:///var/run/docker.sock", Found: colimaHost}}),
	}
	for _, p := range plans {
		for _, k := range Advert(p).Kinds {
			for _, it := range k.Items {
				if strings.Contains(it.Have, "/Users/me") || strings.Contains(it.Title, "/Users/me") {
					t.Errorf("%s: %+v", k.Kind, it)
				}
			}
		}
	}
}

// A daemon the agent reaches through a DOCKER_HOST its service carries is
// written down with the runner, which would otherwise lose it at the next
// install.
func TestDocker_NoLinuxRunnerOnADOCKER_HOSTWritesItDown(t *testing.T) {
	in := Input{Machine: "studio", Host: mac, Jobs: []Job{linuxJob()},
		Runners: []Runner{{Name: "macos", Executor: needs.ExecutorNative, Labels: []string{"macos"}}},
		Docker:  Docker{Host: colimaHost, FromEnv: true, Answers: true}}
	it := item(t, kind(t, Build(in), needs.KindLinux), ItemNoLinuxRunner)
	if len(it.Config) != 2 || it.Config[0].What != ChangeDockerHost || it.Config[0].Value != colimaHost ||
		it.Config[1].What != ChangeDockerRunner {
		t.Errorf("item = %+v", it)
	}
}
