// Package metrics serves the agent's counters and gauges on loopback.
//
// RUNNER_SPEC §20 states the position exactly: INV-1 forbids binding a
// non-loopback listener; it does not forbid loopback. `metrics_addr` (default
// 127.0.0.1:9838, empty to disable) serves runs by terminal state, run
// duration, queue depth, cache volume sizes, disk free per runner, broker
// connection state, and dropped log frames. Anyone wanting these off-machine
// points a local Prometheus at it or tunnels; the agent does not grow a remote
// surface for observability.
//
// # Why the listener re-checks the address
//
// internal/config already rejects a non-loopback `metrics_addr` at load, which
// is the cheap place to enforce it. This package refuses one again at
// [Serve]. That is not redundancy for its own sake. INV-1 is the invariant the
// whole threat model rests on, the config loader is not the only possible
// caller of this package, and a future default, test harness, or flag that
// reaches Serve without passing through Load would otherwise open a port on
// every interface with nothing to stop it. One validation point that
// everything else trusts is the shape of the next incident.
//
// # Why no Prometheus client library
//
// The exposition format is a few hundred lines of text and this package emits
// seven metric families. Pulling in a client library to produce them would add
// a dependency to a module whose dependency list is a reviewed part of the
// spec (§3), for output that is verifiable by reading it.
package metrics

import (
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Namespace prefixes every metric name. Prometheus convention, and it makes
// the agent's series greppable in a Prometheus that scrapes several things.
const Namespace = "latem"

// DurationBuckets are the histogram boundaries for run duration, in seconds.
//
// They run from ten seconds to four hours because that is the real range: a
// lint job finishes in under a minute and an iOS build with a cold cache takes
// twenty (§13.2). Buckets clustered around one minute, which is what a general
// -purpose default gives, would put every mobile build in +Inf and answer no
// question anybody has.
var DurationBuckets = []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}

// Metrics is the agent's metric set.
//
// The zero value is not usable; call [New]. Every field is safe for concurrent
// use, because the producers are a runner pool, a broker goroutine, and a
// daily ticker, all writing while an HTTP handler reads.
type Metrics struct {
	// RunsTotal counts runs that reached a terminal state, labelled by state.
	// It is a counter and never decreases, so a control plane can rate() it.
	RunsTotal *CounterVec
	// RunDuration observes the wall clock of a finished run, in seconds.
	RunDuration *Histogram
	// QueueDepth is how many runs are queued or awaiting approval — the two
	// states §11 parks a run in.
	QueueDepth *GaugeVec
	// CacheBytes is the size of each cache, labelled by runner and cache.
	// Sizes come from the daily prune sweep rather than from a walk per
	// scrape; a scrape that stats a 25 GB volume tree is a scrape that
	// competes with the build.
	CacheBytes *GaugeVec
	// DiskFreeGB is free space per runner, against which §14.4's per-runner
	// floor is judged. Labelled by runner because the floors differ: 40 GB
	// for docker, 60 for native, 150 for tart.
	DiskFreeGB *GaugeVec
	// RunnerAvailable is 1 when a runner passed preflight, 0 when it did not
	// (§20). An unavailable runner accepts no work, and a machine that looks
	// online while accepting nothing is the confusing state this exposes.
	RunnerAvailable *GaugeVec
	// BrokerUp is 1 when a broker is connected, labelled by broker kind.
	BrokerUp *GaugeVec
	// LogFramesDropped counts frames the log pipeline could not deliver.
	// It is the one metric that should always be zero; §16's pipeline drops
	// rather than blocks a build, so a non-zero value means logs are lying.
	LogFramesDropped *Counter
	// AgentInfo carries build identity as a constant 1-valued gauge, the
	// conventional way to expose labels that are not dimensions.
	AgentInfo *GaugeVec

	startedAt time.Time
	families  []family
}

// New builds the metric set of RUNNER_SPEC §20.
func New(version string) *Metrics {
	m := &Metrics{
		RunsTotal:        newCounterVec("state"),
		RunDuration:      newHistogram(DurationBuckets),
		QueueDepth:       newGaugeVec("state"),
		CacheBytes:       newGaugeVec("runner", "cache"),
		DiskFreeGB:       newGaugeVec("runner"),
		RunnerAvailable:  newGaugeVec("runner"),
		BrokerUp:         newGaugeVec("broker"),
		LogFramesDropped: &Counter{},
		AgentInfo:        newGaugeVec("version"),
		startedAt:        time.Now(),
	}
	if version == "" {
		version = "dev"
	}
	m.AgentInfo.Set(1, version)

	m.families = []family{
		{"runs_total", "counter", "Runs that reached a terminal state.", m.RunsTotal},
		{"run_duration_seconds", "histogram", "Wall clock of a finished run.", m.RunDuration},
		{"queue_depth", "gauge", "Runs parked waiting for a slot or a human.", m.QueueDepth},
		{"cache_size_bytes", "gauge", "Size of each mounted build cache.", m.CacheBytes},
		{"disk_free_gb", "gauge", "Free disk on the filesystem holding the work directory.", m.DiskFreeGB},
		{"runner_available", "gauge", "1 when a runner passed preflight.", m.RunnerAvailable},
		{"broker_up", "gauge", "1 when a broker transport is connected.", m.BrokerUp},
		{"log_frames_dropped_total", "counter", "Log frames the pipeline could not deliver.", m.LogFramesDropped},
		{"agent_info", "gauge", "Agent build identity; the value is always 1.", m.AgentInfo},
		{"uptime_seconds", "gauge", "Seconds since the agent started.", uptimeCollector{m}},
	}
	return m
}

// collector renders one metric family's samples.
type collector interface {
	samples() []sample
}

// sample is one exposed time series.
type sample struct {
	// suffix is appended to the family name: "_bucket", "_sum", "_count", or
	// empty for the family itself.
	suffix string
	labels []labelPair
	value  float64
}

type labelPair struct{ name, value string }

type family struct {
	name string
	kind string
	help string
	c    collector
}

// Counter is a monotonically increasing value.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add increases the counter. Negative deltas are ignored rather than applied:
// a counter that goes down breaks every rate() computed from it, and silently
// producing nonsense is worse than losing one increment.
func (c *Counter) Add(n int64) {
	if n <= 0 {
		return
	}
	c.v.Add(uint64(n))
}

// Value reads the counter.
func (c *Counter) Value() uint64 { return c.v.Load() }

func (c *Counter) samples() []sample {
	return []sample{{value: float64(c.v.Load())}}
}

// CounterVec is a counter partitioned by labels.
type CounterVec struct {
	labelNames []string
	mu         sync.RWMutex
	values     map[string]*counterEntry
}

type counterEntry struct {
	labels []string
	n      uint64
}

func newCounterVec(names ...string) *CounterVec {
	return &CounterVec{labelNames: names, values: map[string]*counterEntry{}}
}

// Inc adds one to the series with these label values.
func (c *CounterVec) Inc(labels ...string) { c.Add(1, labels...) }

// Add increases the series with these label values.
//
// A label-count mismatch is dropped rather than panicking. A metrics call site
// is instrumentation, not logic, and taking down a build machine because
// somebody added a dimension in the wrong place is a bad trade.
func (c *CounterVec) Add(n int64, labels ...string) {
	if n <= 0 || len(labels) != len(c.labelNames) {
		return
	}
	key := joinLabels(labels)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.values[key]
	if !ok {
		e = &counterEntry{labels: append([]string(nil), labels...)}
		c.values[key] = e
	}
	e.n += uint64(n)
}

// Value reads one series.
func (c *CounterVec) Value(labels ...string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.values[joinLabels(labels)]; ok {
		return e.n
	}
	return 0
}

func (c *CounterVec) samples() []sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]sample, 0, len(c.values))
	for _, e := range c.values {
		out = append(out, sample{labels: pair(c.labelNames, e.labels), value: float64(e.n)})
	}
	sortSamples(out)
	return out
}

// GaugeVec is a gauge partitioned by labels.
type GaugeVec struct {
	labelNames []string
	mu         sync.RWMutex
	values     map[string]*gaugeEntry
}

type gaugeEntry struct {
	labels []string
	v      float64
}

func newGaugeVec(names ...string) *GaugeVec {
	return &GaugeVec{labelNames: names, values: map[string]*gaugeEntry{}}
}

// Set replaces the series with these label values.
func (g *GaugeVec) Set(v float64, labels ...string) {
	if len(labels) != len(g.labelNames) {
		return
	}
	key := joinLabels(labels)
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.values[key]
	if !ok {
		e = &gaugeEntry{labels: append([]string(nil), labels...)}
		g.values[key] = e
	}
	e.v = v
}

// Value reads one series.
func (g *GaugeVec) Value(labels ...string) float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if e, ok := g.values[joinLabels(labels)]; ok {
		return e.v
	}
	return 0
}

// Delete removes a series.
//
// It exists for the cache and runner gauges: a cache that was pruned away or a
// runner removed from config would otherwise keep reporting its last value
// forever, and a stale gauge is worse than a missing one because nothing about
// it looks stale.
func (g *GaugeVec) Delete(labels ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.values, joinLabels(labels))
}

// Reset drops every series.
func (g *GaugeVec) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values = map[string]*gaugeEntry{}
}

// Replace swaps the whole family for a fresh set of readings.
//
// This is the shape the disk and cache gauges actually need: a sweep produces
// the complete picture, and anything not in it no longer exists.
func (g *GaugeVec) Replace(readings map[string]float64) {
	next := make(map[string]*gaugeEntry, len(readings))
	for key, v := range readings {
		next[key] = &gaugeEntry{labels: splitLabels(key), v: v}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.values = next
}

func (g *GaugeVec) samples() []sample {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]sample, 0, len(g.values))
	for _, e := range g.values {
		out = append(out, sample{labels: pair(g.labelNames, e.labels), value: e.v})
	}
	sortSamples(out)
	return out
}

// Histogram observes a distribution over fixed buckets.
type Histogram struct {
	bounds []float64
	mu     sync.RWMutex
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram(bounds []float64) *Histogram {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	return &Histogram{bounds: b, counts: make([]uint64, len(b))}
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sum += v
	h.total++
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

// ObserveDuration records a duration in seconds.
func (h *Histogram) ObserveDuration(d time.Duration) { h.Observe(d.Seconds()) }

// Count is how many observations there have been.
func (h *Histogram) Count() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.total
}

// Sum is the total of every observation.
func (h *Histogram) Sum() float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sum
}

func (h *Histogram) samples() []sample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]sample, 0, len(h.bounds)+3)
	for i, b := range h.bounds {
		out = append(out, sample{
			suffix: "_bucket",
			labels: []labelPair{{"le", formatFloat(b)}},
			value:  float64(h.counts[i]),
		})
	}
	// Prometheus requires the +Inf bucket, and it must equal _count.
	out = append(out,
		sample{suffix: "_bucket", labels: []labelPair{{"le", "+Inf"}}, value: float64(h.total)},
		sample{suffix: "_sum", value: h.sum},
		sample{suffix: "_count", value: float64(h.total)},
	)
	return out
}

type uptimeCollector struct{ m *Metrics }

func (u uptimeCollector) samples() []sample {
	return []sample{{value: time.Since(u.m.startedAt).Seconds()}}
}

// Snapshot is every current value, for the JSON endpoint.
func (m *Metrics) Snapshot() map[string]any {
	out := map[string]any{}
	for _, f := range m.families {
		samples := f.c.samples()
		if len(samples) == 1 && len(samples[0].labels) == 0 {
			out[f.name] = samples[0].value
			continue
		}
		series := map[string]any{}
		for _, s := range samples {
			series[labelKey(s)] = s.value
		}
		out[f.name] = series
	}
	return out
}

func labelKey(s sample) string {
	parts := make([]string, 0, len(s.labels))
	for _, l := range s.labels {
		parts = append(parts, l.name+"="+l.value)
	}
	key := strings.Join(parts, ",")
	if s.suffix != "" {
		if key == "" {
			return strings.TrimPrefix(s.suffix, "_")
		}
		return strings.TrimPrefix(s.suffix, "_") + ":" + key
	}
	return key
}

// labelSeparator cannot appear in a label value that survives escaping, so it
// is safe as a map key separator.
const labelSeparator = "\x00"

// LabelKey builds the key [GaugeVec.Replace] expects for a set of label
// values. Exported because the caller assembling a sweep's readings has to
// spell the key the same way this package reads it back.
func LabelKey(labels ...string) string { return joinLabels(labels) }

func joinLabels(labels []string) string { return strings.Join(labels, labelSeparator) }

func splitLabels(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, labelSeparator)
}

func pair(names, values []string) []labelPair {
	n := min(len(names), len(values))
	out := make([]labelPair, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, labelPair{names[i], values[i]})
	}
	return out
}

func sortSamples(s []sample) {
	sort.Slice(s, func(i, j int) bool {
		for k := 0; k < len(s[i].labels) && k < len(s[j].labels); k++ {
			if s[i].labels[k].value != s[j].labels[k].value {
				return s[i].labels[k].value < s[j].labels[k].value
			}
		}
		return len(s[i].labels) < len(s[j].labels)
	})
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Families returns the exposed metric names, for tests and documentation.
func (m *Metrics) Families() []string {
	out := make([]string, 0, len(m.families))
	for _, f := range m.families {
		out = append(out, Namespace+"_"+f.name)
	}
	return out
}

// clone is used by the JSON endpoint so a caller cannot mutate internals.
func clone(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}
