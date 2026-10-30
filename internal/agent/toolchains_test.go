package agent

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

// xcodeProbe is a machine whose Xcode the test changes under the agent, and
// which counts the probes that looked at it.
type xcodeProbe struct {
	version atomic.Pointer[string]
	probes  atomic.Int32
}

func newXcodeProbe(v string) *xcodeProbe {
	p := &xcodeProbe{}
	p.version.Store(&v)
	return p
}

func (p *xcodeProbe) probe(context.Context, string) system.Toolchains {
	p.probes.Add(1)
	return system.Toolchains{Xcodes: []system.XcodeInstall{{Version: system.Version{Text: *p.version.Load()}}}}
}

func (p *xcodeProbe) set(v string) { p.version.Store(&v) }

// heartbeatXcode is the Xcode the next heartbeat would advertise.
func heartbeatXcode(ta *testAgent) []string {
	if tc := ta.health(context.Background()).Toolchains; tc != nil {
		return tc.Xcode
	}
	return nil
}

// The App Store replaced the Mac mini's Xcode 26.0.1 with 27.2 under a
// running agent, which went on advertising 26.0.1 until it was restarted.
// The probe runs again on its own.
func TestToolchains_ProbedAgainOnATimer(t *testing.T) {
	p := newXcodeProbe("26.0.1")
	ta := newTestAgent(t, newConfigDir(t, configOptions{}), func(o *Options) {
		o.ProbeToolchains = p.probe
		o.ToolchainsInterval = 50 * time.Millisecond
	})
	ta.run(t)
	waitFor(t, "the first probe", 10*time.Second, func() bool {
		return slices.Equal(heartbeatXcode(ta), []string{"26.0.1"})
	})

	p.set("27.2")
	waitFor(t, "the heartbeat to carry the new Xcode", 10*time.Second, func() bool {
		return slices.Equal(heartbeatXcode(ta), []string{"27.2"})
	})
}

// Asked — by a SIGUSR2 that found nothing to move onto, or a reload — it
// probes now rather than at the next tick.
func TestToolchains_ProbedAgainWhenAsked(t *testing.T) {
	p := newXcodeProbe("26.0.1")
	ta := newTestAgent(t, newConfigDir(t, configOptions{}), func(o *Options) {
		o.ProbeToolchains = p.probe
		o.ToolchainsInterval = time.Hour
	})
	ta.run(t)
	waitFor(t, "the first probe", 10*time.Second, func() bool { return p.probes.Load() == 1 && heartbeatXcode(ta) != nil })

	p.set("27.2")
	ta.reprobeToolchains()
	waitFor(t, "a probe on request", 10*time.Second, func() bool {
		return slices.Equal(heartbeatXcode(ta), []string{"27.2"})
	})
	if n := p.probes.Load(); n != 2 {
		t.Errorf("%d probes, want 2: the hour-long tick should not have fired", n)
	}
}

// The heartbeat carries the machine's size and each runner's recommended
// concurrency (A63), measured on the same beat as the toolchains.
func TestCapacity_InTheHeartbeat(t *testing.T) {
	var measured atomic.Int32
	ta := newTestAgent(t, newConfigDir(t, configOptions{}), func(o *Options) {
		o.MeasureMachine = func(context.Context) capacity.Machine {
			measured.Add(1)
			return capacity.Machine{CPUs: 8, MemoryGB: 8}
		}
		o.ToolchainsInterval = time.Hour
	})
	ta.run(t)
	waitFor(t, "the first measure", 10*time.Second, func() bool {
		return ta.health(context.Background()).Capacity != nil
	})
	c := ta.health(context.Background()).Capacity
	if c.CPUs != 8 || c.MemoryGB != 8 || len(c.Runners) != 1 || c.Runners[0].Name != "test" || c.Runners[0].Recommended != 1 {
		t.Errorf("capacity = %+v", c)
	}
	if measured.Load() != 1 {
		t.Errorf("measured %d times, want once before the hour-long tick", measured.Load())
	}
}

// An update to Xcode moves the fingerprint, and the agent checks its runners
// at once rather than at the next five-minute preflight (A64).
func TestWatch_PreflightsWhenXcodeChanges(t *testing.T) {
	var fp atomic.Value
	fp.Store("27.2")
	ta := newTestAgent(t, newConfigDir(t, configOptions{}), func(o *Options) {
		o.HostFingerprint = func() string { return fp.Load().(string) }
		o.WatchInterval = 20 * time.Millisecond
		o.PreflightInterval = time.Hour
	})
	ta.run(t)
	waitFor(t, "the runner available", 10*time.Second, func() bool { return ta.availableRunners() == 1 })

	ta.exec.setPreflight(errors.New("Xcode at /Applications/Xcode.app is missing or half-installed"))
	time.Sleep(100 * time.Millisecond)
	if ta.availableRunners() != 1 {
		t.Fatal("the runner went dark with the fingerprint unchanged; the hour-long preflight fired")
	}
	fp.Store("27.3 half-installed")
	waitFor(t, "the runner unavailable", 10*time.Second, func() bool { return ta.availableRunners() == 0 })

	ta.exec.setPreflight(nil)
	fp.Store("27.3")
	waitFor(t, "the runner back", 10*time.Second, func() bool { return ta.availableRunners() == 1 })
}
