package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// agentDockerWait bounds each question agentDocker asks. Every one is of a
// local socket or a local command, which answers in milliseconds or not at
// all: a daemon that is not running refuses the connection at once.
const agentDockerWait = 5 * time.Second

// desktopHelperPlist is Docker Desktop's privileged helper, which runs as root
// at every boot and points the system socket at Docker Desktop's own.
const desktopHelperPlist = "/Library/LaunchDaemons/com.docker.socket.plist"

// agentDocker asks the daemon the agent's docker runners will use (A106): the
// one client.FromEnv resolves to, DOCKER_HOST or else the system socket, never
// the `docker` command's context. It runs once per plan, and the agent's plan
// is made every 15 minutes (A70), so it is one ping when Docker answers. Only
// when it does not are the context, the socket's link and Docker Desktop's
// helper read, to name the fix.
func agentDocker(ctx context.Context) readiness.Docker {
	d := readiness.Docker{Asked: true, Host: "unix://" + system.DockerSocket}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		d.Host, d.FromEnv = h, true
	}
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err == nil {
		d.Host = cli.DaemonHost()
		pctx, cancel := context.WithTimeout(ctx, agentDockerWait)
		_, err = cli.Ping(pctx)
		cancel()
		_ = cli.Close()
		d.Answers = err == nil
	}
	if d.Answers {
		return d
	}
	if !d.FromEnv {
		d.Link, _ = os.Readlink(system.DockerSocket)
	}
	d.CLIHost = contextHost(ctx)
	d.DesktopHelper = desktopHelperEnabled(ctx)
	return d
}

// contextHost is the endpoint of the `docker` command's current context, or
// "" when there is no docker command or it cannot say.
func contextHost(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, agentDockerWait)
	defer cancel()
	//nolint:gosec // a fixed argv
	c := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	// Without DOCKER_HOST, which the command would only echo back: the
	// context is what names the engine a person runs.
	c.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "DOCKER_HOST=") })
	out, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// desktopHelperEnabled reports whether Docker Desktop's com.docker.socket is
// installed and not disabled, so it will take the system socket back at the
// next boot. Reading launchd's disabled list needs no sudo.
func desktopHelperEnabled(ctx context.Context) bool {
	if _, err := os.Stat(desktopHelperPlist); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, agentDockerWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "launchctl", "print-disabled", "system").Output()
	if err != nil {
		// Installed and of unknown state: it runs unless disabled.
		return true
	}
	return !helperDisabled(string(out))
}

// helperDisabled reads `launchctl print-disabled system` for com.docker.socket:
// `"com.docker.socket" => disabled` on current macOS, `=> true` on older.
func helperDisabled(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), "=>")
		if !ok || strings.TrimSpace(name) != `"com.docker.socket"` {
			continue
		}
		s := strings.TrimSpace(state)
		return s == "disabled" || s == "true"
	}
	return false
}
