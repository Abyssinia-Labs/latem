// Package dockerhost is where the agent and its commands find the Docker
// daemon, and the one place a Docker client is built (A111).
//
// The order is config.toml's `[docker] host`, then DOCKER_HOST in the
// process's environment, then the system socket. Until A111 every caller
// built its client with the SDK's FromEnv, which knows only the last two, and
// neither the agent nor the SDK reads `docker context`. On a Mac running
// Colima, OrbStack or Kiri nothing answers at the system socket, so the only
// way to reach the engine without sudo was DOCKER_HOST in the agent's service
// definition, which `latem service install` writes whole and so dropped.
package dockerhost

import (
	"path/filepath"
	"strings"

	"github.com/docker/docker/client"
)

// SystemHost is the system socket, where the SDK looks when nothing says
// otherwise.
const SystemHost = client.DefaultDockerHost

// EnvVar is the variable the SDK, the docker command and buildx all read.
const EnvVar = "DOCKER_HOST"

// Source is where an address came from.
type Source int

// The three places, in the order they are read.
const (
	FromSystem Source = iota
	FromEnv
	FromConfig
)

// String names the place as config.toml and doctor say it.
func (s Source) String() string {
	switch s {
	case FromConfig:
		return "[docker] host"
	case FromEnv:
		return EnvVar
	}
	return "the system socket"
}

// Resolve is the address a client for configured dials, and where it came
// from: configured when it is set, else DOCKER_HOST as getenv reads it, else
// the system socket.
func Resolve(configured string, getenv func(string) string) (string, Source) {
	if configured != "" {
		return configured, FromConfig
	}
	if getenv != nil {
		if h := getenv(EnvVar); h != "" {
			return h, FromEnv
		}
	}
	return SystemHost, FromSystem
}

// NewClient is a client for the daemon Resolve names, with the API version
// negotiated, so that one agent binary talks to the range of engines a machine
// may run.
//
// With a configured host the environment is not read at all: config.toml
// accepts only a Unix socket, which needs none of the TLS settings DOCKER_*
// carries, and a stale DOCKER_HOST the agent inherited must not be able to
// make the client fail to build.
func NewClient(configured string) (*client.Client, error) {
	if configured != "" {
		return client.NewClientWithOpts(client.WithHost(configured), client.WithAPIVersionNegotiation())
	}
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// StepEnv is what a native step is given so that the `docker` it runs reaches
// the agent's daemon: DOCKER_HOST, when config.toml names one. Without it the
// step inherits the agent's environment as it always has, DOCKER_HOST
// included.
func StepEnv(configured string) map[string]string {
	if configured == "" {
		return nil
	}
	return map[string]string{EnvVar: configured}
}

// Candidates are the sockets the engines latem knows put under home, in the
// order setup tries them: Colima's default profile, OrbStack's, Kiri's,
// Rancher Desktop's, then Docker Desktop's own. Each is a unix:// address.
func Candidates(home string) []string {
	if home == "" {
		return nil
	}
	paths := []string{
		".colima/default/docker.sock",
		".orbstack/run/docker.sock",
		".kiri/run/docker.sock",
		".rd/docker.sock",
		".docker/run/docker.sock",
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, "unix://"+filepath.Join(home, p))
	}
	return out
}

// SocketPath is the file a unix:// address names, or "" for any other.
func SocketPath(host string) string {
	p, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return ""
	}
	return p
}
