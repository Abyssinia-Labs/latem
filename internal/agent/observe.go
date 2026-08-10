package agent

import (
	"context"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/metrics"
	"github.com/Abyssinia-Labs/latem/internal/run"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Metrics returns the agent's metric set (§20), or nil before startup.
//
// It is exported so a wrapper embedding this package can expose the same
// counters without standing up a second registry. `latem agent` reaches them
// through the loopback endpoint instead.
func (a *Agent) Metrics() *metrics.Metrics { return a.metrics }

// recordResult updates the run counters when a run reaches a terminal state.
//
// Only terminal runs are counted. §20 asks for "runs by terminal state", and
// counting a gate-16 requeue as an outcome would make a healthy machine's rate
// look like a machine failing over and over.
func (a *Agent) recordResult(res run.Result, took time.Duration) {
	if a.metrics == nil || !res.State.IsTerminal() {
		return
	}
	a.metrics.RunsTotal.Inc(string(res.State))
	a.metrics.RunDuration.ObserveDuration(took)
}

// recordRunners publishes per-runner availability (§20).
func (a *Agent) recordRunners() {
	if a.metrics == nil {
		return
	}
	for _, r := range a.runners {
		v := 0.0
		if r.usable() && r.isAvailable() {
			v = 1
		}
		a.metrics.RunnerAvailable.Set(v, r.name)
	}
}

// recordHealth publishes what a heartbeat just said, so a local Prometheus and
// a control plane cannot disagree about the same machine.
func (a *Agent) recordHealth(h protocol.MachineHealth) {
	if a.metrics == nil {
		return
	}
	for _, r := range h.Runners {
		a.metrics.DiskFreeGB.Set(h.DiskFreeGB, r.Name)
	}
	a.metrics.BrokerUp.Set(boolGauge(a.localBrk != nil), "local")
	// §20 lists broker connection state among the counters, and this is the one
	// that can be 0 on a healthy machine: a partition is normal, and the gauge
	// going flat beside runs that keep succeeding is how an operator tells "the
	// control plane cannot see us" from "the machine is down".
	a.metrics.BrokerUp.Set(boolGauge(a.remoteBrk != nil && a.remoteBrk.Connected()), "remote")
	a.recordRunners()
	a.recordQueueDepth()
}

// recordQueueDepth counts the runs parked in the two waiting states (§11).
func (a *Agent) recordQueueDepth() {
	if a.metrics == nil || a.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := a.db.NonTerminal(ctx)
	if err != nil {
		return
	}
	depth := map[string]float64{
		string(protocol.StateQueued):           0,
		string(protocol.StateAwaitingApproval): 0,
	}
	for _, r := range rows {
		if _, counted := depth[string(r.State)]; counted {
			depth[string(r.State)]++
		}
	}
	// Replace rather than Set: a state that emptied since the last beat has to
	// go to zero, and a gauge nobody clears reports a queue that never drains.
	a.metrics.QueueDepth.Replace(depth)
}

// recordCaches publishes cache sizes after a prune.
//
// The sizes come from the sweep rather than from a walk per scrape: a scrape
// that stats a 25 GB volume tree is a scrape that competes with the build.
func (a *Agent) recordCaches(ctx context.Context) {
	if a.metrics == nil || a.caches == nil {
		return
	}
	infos, err := a.caches.List(ctx)
	if err != nil {
		return
	}
	readings := make(map[string]float64, len(infos))
	for _, in := range infos {
		if in.SizeBytes < 0 {
			continue // "unknown" is not zero, and the two lead to opposite decisions
		}
		readings[metrics.LabelKey(in.Runner, in.Name)] = float64(in.SizeBytes)
	}
	a.metrics.CacheBytes.Replace(readings)
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
