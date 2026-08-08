package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestCounter(t *testing.T) {
	var c Counter
	c.Inc()
	c.Add(4)
	if got := c.Value(); got != 5 {
		t.Errorf("Value = %d, want 5", got)
	}
	// A counter that goes down breaks every rate() computed from it, so a
	// negative delta is dropped rather than applied.
	c.Add(-3)
	c.Add(0)
	if got := c.Value(); got != 5 {
		t.Errorf("Value after a negative add = %d, want 5", got)
	}
}

func TestCounterVec(t *testing.T) {
	c := newCounterVec("state")
	c.Inc("succeeded")
	c.Inc("succeeded")
	c.Add(3, "failed")

	if got := c.Value("succeeded"); got != 2 {
		t.Errorf("succeeded = %d, want 2", got)
	}
	if got := c.Value("failed"); got != 3 {
		t.Errorf("failed = %d, want 3", got)
	}
	if got := c.Value("timed_out"); got != 0 {
		t.Errorf("an unseen series should read 0, got %d", got)
	}
	// Instrumentation must not be able to take down a build machine, so a
	// label-count mismatch is dropped rather than panicked on.
	c.Inc("too", "many")
	c.Inc()
	if len(c.samples()) != 2 {
		t.Errorf("a mismatched label set created a series: %+v", c.samples())
	}
}

func TestGaugeVec(t *testing.T) {
	g := newGaugeVec("runner")
	g.Set(120.5, "ios")
	g.Set(41, "linux-x64")
	if got := g.Value("ios"); got != 120.5 {
		t.Errorf("ios = %v, want 120.5", got)
	}
	g.Set(90, "ios")
	if got := g.Value("ios"); got != 90 {
		t.Errorf("ios after Set = %v, want 90", got)
	}

	// A stale gauge is worse than a missing one, because nothing about it
	// looks stale: a pruned cache would otherwise report its last size for as
	// long as the agent runs.
	g.Delete("ios")
	if got := g.Value("ios"); got != 0 {
		t.Errorf("ios after Delete = %v, want 0", got)
	}
	g.Reset()
	if len(g.samples()) != 0 {
		t.Errorf("Reset left %d series", len(g.samples()))
	}
}

func TestGaugeVecReplace(t *testing.T) {
	// A sweep produces the complete picture, so anything not in it no longer
	// exists — that is the shape the disk and cache gauges need.
	g := newGaugeVec("runner", "cache")
	g.Set(1, "ios", "gone")
	g.Replace(map[string]float64{
		LabelKey("ios", "derived-data"): 2 << 30,
		LabelKey("linux-x64", "pnpm"):   512 << 20,
	})
	if got := g.Value("ios", "gone"); got != 0 {
		t.Errorf("Replace left a stale series: %v", got)
	}
	if got := g.Value("ios", "derived-data"); got != 2<<30 {
		t.Errorf("derived-data = %v", got)
	}
	if len(g.samples()) != 2 {
		t.Errorf("Replace produced %d series, want 2", len(g.samples()))
	}
}

func TestHistogram(t *testing.T) {
	h := newHistogram([]float64{1, 10, 100})
	for _, v := range []float64{0.5, 5, 50, 500} {
		h.Observe(v)
	}
	if got := h.Count(); got != 4 {
		t.Errorf("Count = %d, want 4", got)
	}
	if got := h.Sum(); got != 555.5 {
		t.Errorf("Sum = %v, want 555.5", got)
	}

	got := h.samples()
	// Cumulative buckets: 1 → {0.5}, 10 → {0.5,5}, 100 → {0.5,5,50}, +Inf → all.
	want := []float64{1, 2, 3, 4}
	for i, w := range want {
		if got[i].value != w {
			t.Errorf("bucket %d = %v, want %v", i, got[i].value, w)
		}
	}
	last := got[len(got)-3]
	if last.labels[0].value != "+Inf" {
		t.Errorf("the last bucket must be +Inf, got %q", last.labels[0].value)
	}
	if last.value != float64(h.Count()) {
		t.Errorf("+Inf bucket %v must equal _count %d", last.value, h.Count())
	}
}

func TestHistogramObserveDuration(t *testing.T) {
	h := newHistogram(DurationBuckets)
	h.ObserveDuration(20 * time.Minute)
	if got := h.Sum(); got != 1200 {
		t.Errorf("Sum = %v seconds, want 1200", got)
	}
	// §13.2's iOS build is twenty minutes. A bucket set clustered around one
	// minute would file every mobile build under +Inf and answer no question.
	if DurationBuckets[len(DurationBuckets)-1] < 1200 {
		t.Error("the duration buckets do not reach a real iOS build")
	}
}

func TestExposeCoversEverySection20Metric(t *testing.T) {
	// RUNNER_SPEC §20 names the set outright: runs by terminal state, run
	// duration, queue depth, cache volume sizes, disk free per runner, broker
	// connection state, and dropped log frames.
	m := New("1.2.3")
	out := m.Expose()
	for _, want := range []string{
		"latem_runs_total",
		"latem_run_duration_seconds",
		"latem_queue_depth",
		"latem_cache_size_bytes",
		"latem_disk_free_gb",
		"latem_broker_up",
		"latem_log_frames_dropped_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %s:\n%s", want, out)
		}
	}
	for _, name := range m.Families() {
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE line", name)
		}
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
	}
}

func TestExposeFormat(t *testing.T) {
	m := New("v0.1.0")
	m.RunsTotal.Inc(string(protocol.StateSucceeded))
	m.RunsTotal.Add(2, string(protocol.StateFailed))
	m.QueueDepth.Set(3, string(protocol.StateQueued))
	m.DiskFreeGB.Set(41.5, "linux-x64")
	m.CacheBytes.Set(1024, "linux-x64", "pnpm-store")
	m.BrokerUp.Set(1, "local")
	m.BrokerUp.Set(0, "remote")
	m.LogFramesDropped.Add(7)
	m.RunDuration.ObserveDuration(90 * time.Second)

	out := m.Expose()
	for _, want := range []string{
		`latem_runs_total{state="succeeded"} 1`,
		`latem_runs_total{state="failed"} 2`,
		`latem_queue_depth{state="queued"} 3`,
		`latem_disk_free_gb{runner="linux-x64"} 41.5`,
		`latem_cache_size_bytes{cache="pnpm-store",runner="linux-x64"} 1024`,
		`latem_broker_up{broker="local"} 1`,
		`latem_broker_up{broker="remote"} 0`,
		"latem_log_frames_dropped_total 7",
		`latem_run_duration_seconds_bucket{le="120"} 1`,
		"latem_run_duration_seconds_count 1",
		`latem_agent_info{version="v0.1.0"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q:\n%s", want, out)
		}
	}
}

func TestExposeIsStable(t *testing.T) {
	// Two scrapes of an unchanged agent must be identical, or a diff of the
	// output is useless and nothing can be golden-tested. Series come out of
	// maps, so without the sort in Expose this fails within a few iterations.
	//
	// Uptime is excluded because it is the one value that is *supposed* to
	// change between scrapes.
	m := New("v1")
	for _, r := range []string{"b", "a", "c"} {
		m.DiskFreeGB.Set(1, r)
		m.CacheBytes.Set(2, r, "cache")
	}
	first := withoutUptime(m.Expose())
	for i := 0; i < 20; i++ {
		if got := withoutUptime(m.Expose()); got != first {
			t.Fatalf("exposition changed between scrapes:\n%s\n---\n%s", first, got)
		}
	}
	// Sorted by label value, so a reader and a differ both see a stable order.
	if idxA, idxB := strings.Index(first, `runner="a"`), strings.Index(first, `runner="b"`); idxA > idxB {
		t.Errorf("series are not sorted by label value:\n%s", first)
	}
}

func withoutUptime(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, Namespace+"_uptime_seconds ") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestEscapeLabelValue(t *testing.T) {
	// A version comes from `git describe` and a cache name comes from a
	// repository's own jobs.yaml. An unescaped quote makes the whole scrape
	// unparsable, which Prometheus reports as the agent being down.
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{`with"quote`, `with\"quote`},
		{`back\slash`, `back\\slash`},
		{"line\nbreak", `line\nbreak`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := escapeLabelValue(tt.in); got != tt.want {
				t.Errorf("escapeLabelValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	m := New(`v1"; evil="`)
	if strings.Contains(m.Expose(), `evil="`) {
		t.Error("a quote in the version escaped into the exposition unescaped")
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{41.5, "41.5"},
		{1 << 30, "1073741824"},
		{0.001, "0.001"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := formatValue(tt.in); got != tt.want {
				t.Errorf("formatValue(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSnapshot(t *testing.T) {
	m := New("v1")
	m.RunsTotal.Inc("succeeded")
	m.LogFramesDropped.Add(2)
	m.RunDuration.Observe(5)

	snap := m.Snapshot()
	if got := snap["log_frames_dropped_total"]; got != float64(2) {
		t.Errorf("log_frames_dropped_total = %v, want 2", got)
	}
	runs, ok := snap["runs_total"].(map[string]any)
	if !ok {
		t.Fatalf("runs_total = %T, want a map of series", snap["runs_total"])
	}
	if runs["state=succeeded"] != float64(1) {
		t.Errorf("runs_total series = %v", runs)
	}
	if _, ok := snap["uptime_seconds"]; !ok {
		t.Error("uptime is missing from the snapshot")
	}
}

func TestHandlerEndpoints(t *testing.T) {
	m := New("v1")
	m.RunsTotal.Inc("succeeded")
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	t.Run("prometheus", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + PathMetrics)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q", ct)
		}
	})

	t.Run("json", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + PathVars)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("the JSON endpoint did not produce JSON: %v", err)
		}
		if _, ok := got["runs_total"]; !ok {
			t.Errorf("snapshot is missing runs_total: %v", got)
		}
	})

	t.Run("index lists the endpoints", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("anything else is 404", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/etc/passwd")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestConcurrentUse(t *testing.T) {
	// The producers are a runner pool, a broker goroutine, and a daily ticker,
	// all writing while an HTTP handler reads.
	m := New("v1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.RunsTotal.Inc("succeeded")
				m.DiskFreeGB.Set(float64(j), "linux-x64")
				m.RunDuration.Observe(float64(j))
				m.LogFramesDropped.Inc()
				_ = m.Expose()
				_ = m.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	if got := m.RunsTotal.Value("succeeded"); got != 1600 {
		t.Errorf("runs_total = %d, want 1600", got)
	}
}
