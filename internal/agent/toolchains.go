package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/capacity"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// readinessTimeout bounds one look at what the machine can build, so that a
// Docker daemon that does not answer holds up the next toolchain probe by no
// more than this.
const readinessTimeout = 2 * time.Minute

// toolchainsLoop keeps what the agent says it has for builds current: what a
// run's note compares a job's needs against (A59), and what the heartbeat
// tells the control plane (A61). It probes once at start, in the background
// because the probe reads every Xcode's Info.plist and asks a JDK list — a
// second or two startup need not wait for, so a run before the first probe
// lands writes no note — and then every toolchainsEvery, and whenever
// reprobeToolchains asks. A machine that went on advertising the Xcode it
// started with, after the App Store replaced it, would have every job
// explained against an Xcode it no longer has.
func (a *Agent) toolchainsLoop(ctx context.Context) {
	probe, measure := a.opts.ProbeToolchains, a.opts.MeasureMachine
	if probe == nil && measure == nil {
		return
	}
	t := time.NewTicker(a.toolchainsEvery)
	defer t.Stop()
	for {
		if probe != nil {
			tc := probe(ctx, stepPath(a.current(), os.Getenv("PATH")))
			if ctx.Err() != nil {
				return
			}
			if prev := a.toolchains.Swap(&tc); prev != nil && !reflect.DeepEqual(toolchainsAdvert(*prev), toolchainsAdvert(tc)) {
				a.log.Info("toolchains changed", "xcode", tc.XcodeVersions())
			}
		}
		// The machine's size, on the same beat: Docker Desktop's share of
		// it is a setting that changes under a running agent too (A63).
		if measure != nil {
			m := measure(ctx)
			if ctx.Err() != nil {
				return
			}
			a.machine.Store(&m)
		}
		a.reportReadiness(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.reprobe:
		}
	}
}

// reportReadiness works out what the heartbeat says of what this machine can
// build (A70). It reads every repository and probes again, so it runs on this
// beat and never a heartbeat's. One that fails keeps the last answer, which
// only explains, and is logged: nothing else depends on it.
func (a *Agent) reportReadiness(ctx context.Context) {
	if a.opts.Readiness == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	r, err := a.opts.Readiness(ctx, a.current())
	if err != nil {
		if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			a.log.Warn("could not work out what this machine can build", "err", err)
		}
		return
	}
	a.readiness.Store(r)
}

// reprobeToolchains asks toolchainsLoop to probe now. A probe already asked
// for and not yet begun is the same request.
func (a *Agent) reprobeToolchains() {
	select {
	case a.reprobe <- struct{}{}:
	default:
	}
}

// watchLoop notices the machine changing between preflights (A64): above all
// an App Store update replacing Xcode, which takes minutes during which a
// native runner would accept work that cannot build, and after which it may
// want a licence or a first launch. It asks HostFingerprint — a few stats —
// every watchEvery, and when the answer moves it runs preflight and the
// toolchain probe at once and rings: the runner goes dark within half a
// minute of the update and back as soon as Xcode is whole, instead of up to
// five minutes either side.
func (a *Agent) watchLoop(ctx context.Context) {
	fingerprint := a.opts.HostFingerprint
	if fingerprint == nil {
		return
	}
	last := fingerprint()
	t := time.NewTicker(a.watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := fingerprint()
		if now == last {
			continue
		}
		last = now
		a.log.Info("the machine's Xcode changed; checking its runners again")
		a.preflightAll(ctx)
		a.reprobeToolchains()
		a.ring()
	}
}

// capacityAdvert is the heartbeat's capacity (A63): the machine's size and
// each runner's recommended concurrency, worked out against runners.
func capacityAdvert(m capacity.Machine, runners []*config.Runner) *protocol.Capacity {
	out := &protocol.Capacity{CPUs: m.CPUs, MemoryGB: m.MemoryGB}
	if m.Docker != nil {
		out.Docker = &protocol.DockerCapacity{CPUs: m.Docker.CPUs, MemoryGB: m.Docker.MemoryGB}
	}
	for _, a := range capacity.Recommend(m, runners) {
		out.Runners = append(out.Runners, protocol.RunnerCapacity{Name: a.Runner, Recommended: a.Recommended, Why: a.Why})
	}
	return out
}
