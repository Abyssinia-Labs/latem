package readiness

import (
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/system"
)

// Docker is the daemon a docker runner's jobs go to, as setup found it
// (A100). The agent dials DOCKER_HOST, or else the system socket, and never
// reads `docker context`; the `docker` command follows the context. So the
// command can answer while the agent reaches nothing, which is what an engine
// other than Docker Desktop leaves behind until the system socket points at
// it. Its zero value is a machine where nobody asked.
type Docker struct {
	// Asked: a docker runner is configured, so the agent's daemon was asked.
	Asked bool
	// Host is where the agent dials: DOCKER_HOST's value, or the system
	// socket's address. FromEnv says it is DOCKER_HOST's.
	Host    string
	FromEnv bool
	// Answers: a ping there was answered.
	Answers bool
	// Link is where the system socket points, when it is a link. The rest are
	// read only when the agent's daemon does not answer.
	Link string
	// CLIHost is the endpoint of the `docker` command's current context, which
	// names the engine a person is running.
	CLIHost string
	// DesktopHelper: Docker Desktop's privileged helper, com.docker.socket, is
	// installed and enabled. It points the system socket back at Docker
	// Desktop's own at every boot.
	DesktopHelper bool
}

// ItemDockerAgent is the item for a docker runner whose daemon does not
// answer where the agent looks.
const ItemDockerAgent = "docker-agent"

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

func dockerAgentItem(d Docker, h system.Host, jobs []string) []Item {
	it, ok := DockerAgentItem(d, h)
	if !ok {
		return nil
	}
	it.Jobs = jobs
	return []Item{it}
}

// DockerAgentItem is what stands between a docker runner and its daemon, and
// false when the daemon answers or nobody asked. It names the fix for the
// engine the `docker` command reaches, which is the one a person is running.
// doctor says the same (A100).
func DockerAgentItem(d Docker, h system.Host) (Item, bool) {
	if !d.Asked || d.Answers {
		return Item{}, false
	}
	it := Item{ID: ItemDockerAgent, Title: "Docker answering where the agent looks"}
	where := socketPath(d.Host)
	switch {
	case d.FromEnv:
		it.Have = "nothing answers at DOCKER_HOST, " + d.Host
	case d.Link != "":
		it.Have = "nothing answers at " + where + ", a link to " + d.Link
	default:
		it.Have = "nothing answers at " + where
	}

	cli := ""
	if d.CLIHost != "" && d.CLIHost != d.Host {
		cli = d.CLIHost
	}
	engine := Engine(cli)
	dockerHost := "or give the agent's service DOCKER_HOST=" + cli + ": the agent reads DOCKER_HOST or " +
		system.DockerSocket + ", and never `docker context`"

	switch {
	case d.FromEnv && cli != "":
		it.Manual = []string{"start what serves " + d.Host + ", or change DOCKER_HOST in the agent's service to " + cli +
			", which the `docker` command reaches"}
		return it, true
	case d.FromEnv:
		it.Manual = []string{"start what serves " + d.Host + ", or change DOCKER_HOST in the agent's service"}
		return it, true
	case cli == "" && h.Linux():
		it.Yours = [][]string{{"sudo", "systemctl", "start", "docker"}}
		return it, true
	case cli == "":
		// The `docker` command reaches nothing else either, so the engine is
		// not running: the one the link names, if it names one.
		if e := Engine(d.Link); e != "" {
			it.Manual = []string{"start " + e + ", which " + where + " points at"}
			if e == "Docker Desktop" {
				it.Yours = [][]string{{"open", "-a", "Docker"}}
				it.Manual = nil
			}
			return it, true
		}
		it.Manual = []string{"start your Docker engine (Docker Desktop, Kiri, OrbStack, Colima or Rancher Desktop), " +
			"and point " + where + " at it"}
		return it, true
	}

	// The `docker` command reaches an engine at another socket.
	if h.Mac() && d.DesktopHelper && engine != "Docker Desktop" {
		it.Have += "; Docker Desktop's com.docker.socket points it back at Docker Desktop at every boot"
		it.Yours = append(it.Yours, []string{"sudo", "launchctl", "disable", "system/com.docker.socket"})
		it.Manual = append(it.Manual, "uninstalling Docker Desktop removes com.docker.socket too")
	}
	sock := socketPath(cli)
	switch engine {
	case "Kiri":
		if d.Link != "" && d.Link != sock {
			// kiri setup refuses while another engine's link is there.
			it.Yours = append(it.Yours, []string{"sudo", "rm", where})
		}
		it.Yours = append(it.Yours, []string{"kiri", "setup", "--docker-socket"})
	case "OrbStack":
		it.Manual = append(it.Manual, "in OrbStack's settings, allow it admin privileges, with which it links "+where+" to itself")
	case "Rancher Desktop":
		it.Manual = append(it.Manual, "in Rancher Desktop's preferences, turn on administrative access, with which it links "+where+" to itself")
	case "Docker Desktop":
		it.Manual = append(it.Manual, "in Docker Desktop's settings, Advanced, allow the default Docker socket to be used")
	default:
		if h.Mac() {
			// Colima, and an engine latem does not know, link by hand.
			it.Yours = append(it.Yours, []string{"sudo", "ln", "-sf", sock, where})
		}
	}
	it.Manual = append(it.Manual, dockerHost)
	return it, true
}

func socketPath(host string) string {
	if p, ok := strings.CutPrefix(host, "unix://"); ok {
		return p
	}
	return host
}
