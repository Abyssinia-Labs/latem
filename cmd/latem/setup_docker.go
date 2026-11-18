package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/dockerhost"
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// agentDockerWait bounds each question agentDocker asks. Every one is of a
// local socket or a local command, which answers in milliseconds or not at
// all: a daemon that is not running refuses the connection at once.
const agentDockerWait = 5 * time.Second

// agentDocker asks the daemon the agent's docker runners will use (A106,
// A111): config.toml's [docker] host, else DOCKER_HOST, else the system
// socket, never the `docker` command's context. DOCKER_HOST is the one the
// installed service definition sets, when it sets one, since that is what the
// agent is given and not what this shell has. It runs once per plan, and the
// agent's plan is made every 15 minutes (A70), so it is one ping when Docker
// answers. Only when it does not are the context, the socket's link and the
// engines' own sockets read, to name the fix.
func agentDocker(ctx context.Context, configured string) readiness.Docker {
	return probeAgentDocker(ctx, configured, serviceEnv(system.New(system.Options{})), dockerDeps{})
}

// dockerDeps are what probeAgentDocker and findDocker ask, each nil for the
// real one.
type dockerDeps struct {
	ping    func(ctx context.Context, host string) bool
	context func(ctx context.Context) string
	home    string
	exists  func(path string) bool
}

func (d dockerDeps) withDefaults() dockerDeps {
	if d.ping == nil {
		d.ping = pingDocker
	}
	if d.context == nil {
		d.context = contextHost
	}
	if d.home == "" {
		d.home, _ = os.UserHomeDir()
	}
	if d.exists == nil {
		d.exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	return d
}

// serviceEnv reads DOCKER_HOST as the agent has it: the service definition's,
// when it sets one, else this process's.
func serviceEnv(sys *system.System) func(string) string {
	return func(k string) string {
		if k == dockerhost.EnvVar {
			if h := sys.ServiceDockerHost(); h != "" {
				return h
			}
		}
		return os.Getenv(k)
	}
}

func probeAgentDocker(ctx context.Context, configured string, getenv func(string) string, deps dockerDeps) readiness.Docker {
	deps = deps.withDefaults()
	host, src := dockerhost.Resolve(configured, getenv)
	d := readiness.Docker{Asked: true, Host: host,
		FromConfig: src == dockerhost.FromConfig, FromEnv: src == dockerhost.FromEnv}
	d.Answers = deps.ping(ctx, host)
	if d.Answers {
		return d
	}
	if src == dockerhost.FromSystem {
		d.Link, _ = os.Readlink(dockerhost.SocketPath(host))
	}
	d.CLIHost = deps.context(ctx)
	d.Found = engineSocket(ctx, deps, host, d.CLIHost)
	return d
}

// engineSocket is the first socket that answers, other than skip: the
// `docker` command's context's, then the places the engines latem knows put
// theirs (A111). Only a socket that is there is asked.
func engineSocket(ctx context.Context, deps dockerDeps, skip, cli string) string {
	var tried []string
	for _, h := range append([]string{cli}, dockerhost.Candidates(deps.home)...) {
		p := dockerhost.SocketPath(h)
		if p == "" || h == skip || slices.Contains(tried, h) {
			continue
		}
		tried = append(tried, h)
		if deps.exists(p) && deps.ping(ctx, h) {
			return h
		}
	}
	return ""
}

// findDocker is the daemon a new config.toml's docker runner would use: ""
// and true when the agent would reach one with no [docker] host, the socket
// to write as [docker] host and true when an engine answers at one of its
// own, and false when nothing answers (A111). A DOCKER_HOST in this shell is
// written down, because the agent's service will not have this shell's
// environment.
func findDocker(ctx context.Context) (string, bool) {
	return findDockerWith(ctx, os.Getenv, dockerDeps{})
}

func findDockerWith(ctx context.Context, getenv func(string) string, deps dockerDeps) (string, bool) {
	deps = deps.withDefaults()
	host, src := dockerhost.Resolve("", getenv)
	if deps.ping(ctx, host) {
		if src == dockerhost.FromEnv && dockerhost.SocketPath(host) != "" {
			return host, true
		}
		return "", src == dockerhost.FromSystem
	}
	if h := engineSocket(ctx, deps, host, deps.context(ctx)); h != "" {
		return h, true
	}
	return "", false
}

// pingDocker reports whether a daemon answers at host. A unix:// host is
// dialled as written; any other is DOCKER_HOST's, with the TLS settings the
// environment carries beside it.
func pingDocker(ctx context.Context, host string) bool {
	configured := host
	if dockerhost.SocketPath(host) == "" {
		configured = ""
	}
	cli, err := dockerhost.NewClient(configured)
	if err != nil {
		return false
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, agentDockerWait)
	defer cancel()
	_, err = cli.Ping(ctx)
	return err == nil
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
