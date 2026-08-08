package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// ErrNotFromFile is returned by [Reloader.Reload] for a config built by
// [Parse], which has no file to re-read.
var ErrNotFromFile = errors.New("config: this config was not loaded from a file and cannot be reloaded")

// Reloader holds the live config and swaps it on SIGHUP (§8).
//
// It does not install a signal handler. Signal handling belongs to the agent
// (WS-11), which owns the process lifecycle and already has a shutdown path to
// coordinate with; a library that reaches for signal.Notify decides that on
// its caller's behalf and cannot be embedded twice.
//
// The split between reloadable and restart-only is §8's: "changes to
// [[runner]] require a restart". This type extends that to everything else
// whose value was consumed once at startup and cannot be revisited without
// tearing down state — see [RequiresRestart].
type Reloader struct {
	mu  sync.RWMutex
	cfg *Config
}

// NewReloader wraps a config loaded by [Load].
func NewReloader(cfg *Config) *Reloader { return &Reloader{cfg: cfg} }

// Config returns the live config. Callers should call this per use rather than
// caching the pointer, since a reload replaces it.
func (r *Reloader) Config() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// Reload re-reads the config file and applies the reloadable subset.
//
// It returns the new live config and a description of every change that was
// read but not applied because it needs a restart. Those descriptions are for
// the operator: a SIGHUP that silently ignores half an edit is worse than one
// that refuses it, because the operator walks away believing the change took.
//
// On a validation failure the live config is left exactly as it was and the
// error is returned. A broken file must not take down a running agent — the
// operator was editing, and killing a machine with builds in flight over a
// typo is the worst available response. The agent should log the error and
// keep going.
func (r *Reloader) Reload() (*Config, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.cfg
	if current.source == nil {
		return current, nil, ErrNotFromFile
	}

	next, err := Load(*current.source)
	if err != nil {
		return current, nil, err
	}

	deferred := RequiresRestart(current, next)
	r.cfg = merge(current, next)
	return r.cfg, deferred, nil
}

// RequiresRestart describes the differences between two configs that a SIGHUP
// cannot apply.
//
// A field is restart-only when its value was consumed once, at startup, into
// state that cannot be rebuilt without disturbing running work:
//
//   - [[runner]] — §8 says so outright. A runner is a goroutine pool holding
//     containers, VMs, and cache locks.
//   - machine.id — the machine's identity to a control plane. Changing it
//     mid-flight would orphan every run already reported under the old one.
//   - agent.work_dir — in-flight runs have workspaces underneath it, and
//     startup reconciliation sweeps it (§6 step 5).
//   - agent.metrics_addr and broker.local.socket — already bound. Rebinding a
//     listener under live connections is not a config change, it is a restart
//     with extra steps.
//
// Everything else reloads: the repo allowlist (the common case — adding a repo
// should not need downtime), the remote broker, retention and TTLs, and the
// secrets chain.
func RequiresRestart(old, next *Config) []string {
	var out []string
	note := func(format string, args ...any) {
		out = append(out, fmt.Sprintf(format, args...))
	}

	if old.Machine.ID != next.Machine.ID {
		note("machine.id changed from %q to %q", old.Machine.ID, next.Machine.ID)
	}
	if old.Paths.Work != next.Paths.Work {
		note("agent.work_dir changed from %s to %s", old.Paths.Work, next.Paths.Work)
	}
	if old.Agent.MetricsAddr != next.Agent.MetricsAddr {
		note("agent.metrics_addr changed from %q to %q", old.Agent.MetricsAddr, next.Agent.MetricsAddr)
	}
	if old.Broker.Local.Enabled != next.Broker.Local.Enabled {
		note("broker.local.enabled changed from %v to %v", old.Broker.Local.Enabled, next.Broker.Local.Enabled)
	}
	if old.Paths.Socket != next.Paths.Socket {
		note("broker.local.socket changed from %s to %s", old.Paths.Socket, next.Paths.Socket)
	}
	out = append(out, runnerChanges(old.Runners, next.Runners)...)
	return out
}

// runnerChanges reports added, removed, and modified runners by name.
func runnerChanges(old, next []*Runner) []string {
	oldByName := indexRunners(old)
	nextByName := indexRunners(next)

	names := make([]string, 0, len(oldByName)+len(nextByName))
	for name := range oldByName {
		names = append(names, name)
	}
	for name := range nextByName {
		if _, ok := oldByName[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		o, inOld := oldByName[name]
		n, inNext := nextByName[name]
		switch {
		case !inNext:
			out = append(out, fmt.Sprintf("runner %q was removed", name))
		case !inOld:
			out = append(out, fmt.Sprintf("runner %q was added", name))
		case !reflect.DeepEqual(o, n):
			out = append(out, fmt.Sprintf("runner %q was changed", name))
		}
	}
	return out
}

// indexRunners keys runners by name, skipping unnamed ones. A config that
// reaches here has been validated, so every runner has a unique name.
func indexRunners(rs []*Runner) map[string]*Runner {
	m := make(map[string]*Runner, len(rs))
	for _, r := range rs {
		if r.Name != "" {
			m[r.Name] = r
		}
	}
	return m
}

// merge builds the config that goes live after a reload: the new file's
// reloadable fields over the old config's restart-only ones.
//
// Carrying the old values forward matters more than it looks. If the live
// config advertised the new [[runner]] list, a heartbeat would report runners
// that do not exist and `latem doctor` would check a machine that is not the
// one running. The live config must describe the agent as it actually is.
func merge(old, next *Config) *Config {
	merged := *next

	merged.SchemaVersion = old.SchemaVersion
	merged.Machine.ID = old.Machine.ID
	merged.Agent.WorkDir = old.Agent.WorkDir
	merged.Agent.MetricsAddr = old.Agent.MetricsAddr
	merged.Broker.Local = old.Broker.Local
	merged.Runners = old.Runners

	// Paths derive from the config directory, which cannot change under a
	// reload, and from work_dir and the socket, which are restart-only. The
	// token file is the exception: rotating a machine token is a routine
	// operation and the remote broker re-reads it on reconnect.
	tokenFile := merged.Paths.TokenFile
	merged.Paths = old.Paths
	merged.Paths.TokenFile = tokenFile

	merged.source = old.source
	return &merged
}
