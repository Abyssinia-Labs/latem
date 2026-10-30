package agent

import (
	"context"
	"os"
	"reflect"
	"time"
)

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
	probe := a.opts.ProbeToolchains
	if probe == nil {
		return
	}
	t := time.NewTicker(a.toolchainsEvery)
	defer t.Stop()
	for {
		tc := probe(ctx, stepPath(a.current(), os.Getenv("PATH")))
		if ctx.Err() != nil {
			return
		}
		if prev := a.toolchains.Swap(&tc); prev != nil && !reflect.DeepEqual(toolchainsAdvert(*prev), toolchainsAdvert(tc)) {
			a.log.Info("toolchains changed", "xcode", tc.XcodeVersions())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.reprobe:
		}
	}
}

// reprobeToolchains asks toolchainsLoop to probe now. A probe already asked
// for and not yet begun is the same request.
func (a *Agent) reprobeToolchains() {
	select {
	case a.reprobe <- struct{}{}:
	default:
	}
}
