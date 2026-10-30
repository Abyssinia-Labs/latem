package main

import (
	"context"
	"fmt"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// measureMachine is what capacity's recommendation is worked out from: this
// machine's CPUs and memory, and what its Docker daemon is given when one
// answers (A63).
func measureMachine(ctx context.Context, sys *system.System) capacity.Machine {
	cpus, mem := sys.Resources(ctx)
	return capacity.Machine{CPUs: cpus, MemoryGB: capacity.BytesToGB(mem), Docker: dockerShare(ctx)}
}

// dockerShare is what the Docker daemon says it can use: on a Mac, the VM
// Docker Desktop runs, which is usually half the Mac or less. Nil when no
// daemon answers within a few seconds.
func dockerShare(ctx context.Context) *capacity.Docker {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := cli.Info(ctx)
	// A daemon that reports no CPUs or no memory has said nothing usable, and
	// the control plane refuses a zero in either (A63).
	if err != nil || info.NCPU <= 0 || info.MemTotal <= 0 {
		return nil
	}
	return &capacity.Docker{CPUs: info.NCPU, MemoryGB: capacity.BytesToGB(uint64(max(info.MemTotal, 0)))}
}

// checkCapacity says, per runner, whether its concurrency fits the machine.
// A runner set above its recommendation is a warning, not a failure: it
// runs, and fails only under load, in ways that read as flaky tests.
func (d *doctor) checkCapacity(ctx context.Context) []check {
	if d.cfg == nil {
		return []check{skip("capacity", "runner capacity", "config did not load")}
	}
	measure := d.machine
	if measure == nil {
		measure = func(ctx context.Context) capacity.Machine { return measureMachine(ctx, d.sys) }
	}
	m := measure(ctx)
	if m.CPUs == 0 || m.MemoryGB == 0 {
		return []check{skip("capacity", "runner capacity", "this machine's CPUs and memory could not be read")}
	}
	out := make([]check, 0, len(d.cfg.Runners))
	for _, a := range capacity.Recommend(m, d.cfg.Runners) {
		id, title := "capacity."+a.Runner, "capacity / "+a.Runner
		switch {
		case a.Over():
			out = append(out, warn(id, title,
				fmt.Sprintf("concurrency %d; this machine has room for %d: %s", a.Configured, a.Recommended, a.Why),
				fmt.Sprintf("# in config.toml, [[runner]] %q:", a.Runner),
				fmt.Sprintf("concurrency = %d   # then `latem service reload`", a.Recommended)).
				withNotes("Jobs that share too few CPUs or too little memory fail under load in ways that "+
					"look like flaky tests: a timeout, a compiler killed for memory, a test that expected "+
					"to finish in a second."))
		case a.Configured < a.Recommended:
			out = append(out, pass(id, title,
				fmt.Sprintf("concurrency %d; there is room for %d: %s", a.Configured, a.Recommended, a.Why)))
		default:
			out = append(out, pass(id, title, fmt.Sprintf("concurrency %d, as recommended: %s", a.Configured, a.Why)))
		}
	}
	return out
}
