package readiness

import (
	"slices"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/system"
)

// Docker is the daemon a docker runner's jobs go to, as setup found it
// (A106, A111). The agent dials config.toml's `[docker] host`, else
// DOCKER_HOST, else the system socket, and never reads `docker context`; the
// `docker` command follows the context. So the command can answer while the
// agent reaches nothing, which is what Colima, OrbStack, Kiri and Rancher
// Desktop leave behind on a Mac: their socket is under the home directory.
// Its zero value is a machine where nobody asked.
type Docker struct {
	// Asked: a docker runner is configured, so the agent's daemon is one
	// its jobs need.
	Asked bool
	// Host is where the agent dials. FromConfig says it is config.toml's
	// [docker] host, FromEnv that it is DOCKER_HOST's; neither, the system
	// socket.
	Host       string
	FromConfig bool
	FromEnv    bool
	// Answers: a ping there was answered.
	Answers bool
	// Link is where the system socket points, when it is a link. The rest are
	// read only when the agent's daemon does not answer.
	Link string
	// CLIHost is the endpoint of the `docker` command's current context, which
	// names the engine a person is running.
	CLIHost string
	// Found is an engine's socket that answers where the agent does not look:
	// the context's endpoint, or one of the places Colima, OrbStack, Kiri,
	// Rancher Desktop and Docker Desktop put theirs (A111). Writing it as
	// [docker] host is the fix, and needs no sudo.
	Found string
}

// Item IDs for the agent's daemon.
const (
	// ItemDockerAgent is the item for a docker runner whose daemon does not
	// answer where the agent looks.
	ItemDockerAgent = "docker-agent"
	// ItemDockerHost is a daemon the agent reaches through a DOCKER_HOST in
	// its environment, which `latem service install` does not keep (A111).
	ItemDockerHost = "docker-host"
	// ItemNoLinuxRunner is a machine with no runner for Linux jobs.
	ItemNoLinuxRunner = "no-linux-runner"
)

// LinuxRunner is the name of the docker runner setup adds, as `latem setup`
// names the one it writes into a new config.toml.
const LinuxRunner = "linux"

// Engine names the Docker engine whose socket an address is, or "".
func Engine(host string) string {
	p := strings.TrimPrefix(host, "unix://")
	switch {
	case strings.Contains(p, "/.kiri/"):
		return "Kiri"
	case strings.Contains(p, "/.orbstack/"):
		return "OrbStack"
	case strings.Contains(p, "/.colima/"):
		return "Colima"
	case strings.Contains(p, "/.rd/"):
		return "Rancher Desktop"
	case strings.Contains(p, "/.docker/run/"), strings.Contains(p, "/Library/Containers/com.docker.docker/"):
		return "Docker Desktop"
	}
	return ""
}

// DockerHostChange writes host as config.toml's [docker] host (A111).
func DockerHostChange(host string) Change {
	return Change{What: ChangeDockerHost, Value: host, Line: "[docker] host = " + strconv.Quote(host)}
}

func dockerAgentItem(d Docker, h system.Host, jobs []string) []Item {
	it, ok := DockerAgentItem(d, h)
	if !ok {
		it, ok = DockerHostItem(d)
	}
	if !ok {
		return nil
	}
	it.Jobs = jobs
	return []Item{it}
}

// DockerAgentItem is what stands between a docker runner and its daemon, and
// false when the daemon answers or nobody asked. When an engine answers at a
// socket of its own, the fix is config.toml's [docker] host, which latem
// writes; otherwise it is starting the engine. doctor says the same (A106,
// A111).
func DockerAgentItem(d Docker, h system.Host) (Item, bool) {
	if !d.Asked || d.Answers {
		return Item{}, false
	}
	it := Item{ID: ItemDockerAgent, Title: "Docker answering where the agent looks"}
	where := socketPath(d.Host)
	switch {
	case d.FromConfig:
		it.Have = "nothing answers at [docker] host, " + d.Host
	case d.FromEnv:
		it.Have = "nothing answers at DOCKER_HOST, " + d.Host
	case d.Link != "":
		it.Have = "nothing answers at " + where + ", a link to " + d.Link
	default:
		it.Have = "nothing answers at " + where
	}

	if d.Found != "" {
		it.Have += "; " + engineAt(d.Found) + " answers at " + socketPath(d.Found)
		it.Config = []Change{DockerHostChange(d.Found)}
		return it, true
	}

	cli := ""
	if d.CLIHost != "" && d.CLIHost != d.Host {
		cli = d.CLIHost
	}
	then := "; `latem setup apply " + ItemDockerAgent + "` then writes it as [docker] host in config.toml"
	switch {
	case cli != "":
		// The context names an engine, and it did not answer either.
		it.Manual = []string{"start " + engineAt(cli) + ", which the `docker` command's context names" + then}
	case d.FromConfig:
		it.Manual = []string{"start what serves " + d.Host + ", or change [docker] host in config.toml"}
	case d.FromEnv:
		it.Manual = []string{"start what serves " + d.Host + ", or set [docker] host in config.toml, " +
			"which the agent reads before DOCKER_HOST"}
	case h.Linux():
		it.Yours = [][]string{{"sudo", "systemctl", "start", "docker"}}
	default:
		// The `docker` command reaches nothing either, so the engine is not
		// running: the one the link names, if it names one.
		switch e := Engine(d.Link); e {
		case "Docker Desktop":
			it.Yours = [][]string{{"open", "-a", "Docker"}}
		case "":
			it.Manual = []string{"start your Docker engine (Docker Desktop, Colima, OrbStack, Kiri or Rancher Desktop)" + then}
		default:
			it.Manual = []string{"start " + e + ", which " + where + " points at"}
		}
	}
	return it, true
}

// DockerHostItem is a daemon that answers at a DOCKER_HOST in the agent's
// environment, which on a machine set up by hand is a line added to the
// service definition. `latem service install` writes that definition whole
// and drops the line, so the item moves it to config.toml (A111). It is soft:
// the agent reaches Docker today. Only a Unix socket moves; config.toml takes
// nothing else.
func DockerHostItem(d Docker) (Item, bool) {
	if !d.Answers || !d.FromEnv || !strings.HasPrefix(d.Host, "unix://") {
		return Item{}, false
	}
	return Item{ID: ItemDockerHost, Title: "the Docker daemon's address in config.toml", Soft: true,
		Have:   "DOCKER_HOST=" + d.Host + " in the agent's environment, which `latem service install` does not keep",
		Config: []Change{DockerHostChange(d.Host)}}, true
}

// noLinuxRunnerItem adds a docker runner when a daemon answers, the agent's
// or an engine's at a socket of its own, and says to start one otherwise
// (A111).
func noLinuxRunnerItem(d Docker, runners []Runner) Item {
	it := Item{ID: ItemNoLinuxRunner, Title: "a runner labelled linux"}
	name := LinuxRunner
	if slices.ContainsFunc(runners, func(r Runner) bool { return r.Name == name }) {
		name = "docker"
	}
	runner := Change{What: ChangeDockerRunner, Value: name,
		Line: "a docker runner named " + name + ", which runs Linux jobs in containers"}
	switch {
	case d.Answers && d.FromEnv && strings.HasPrefix(d.Host, "unix://"):
		// A DOCKER_HOST the service carries, which an install drops: the
		// runner would lose its daemon at the next one.
		it.Config = []Change{DockerHostChange(d.Host), runner}
	case d.Answers:
		it.Config = []Change{runner}
	case d.Found != "":
		it.Have = engineAt(d.Found) + " answers at " + socketPath(d.Found) + ", where the agent does not look"
		it.Config = []Change{DockerHostChange(d.Found), runner}
	default:
		it.Manual = []string{"start Docker (Docker Desktop, Colima, OrbStack, Kiri or Rancher Desktop); " +
			"`latem setup apply " + ItemNoLinuxRunner + "` then adds a docker runner"}
	}
	return it
}

// engineAt names the engine at host, or the socket itself.
func engineAt(host string) string {
	if e := Engine(host); e != "" {
		return e
	}
	return "what serves " + host
}

func socketPath(host string) string {
	if p, ok := strings.CutPrefix(host, "unix://"); ok {
		return p
	}
	return host
}
