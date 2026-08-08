package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Paths served by [Metrics.Handler].
const (
	// PathMetrics is the Prometheus text exposition endpoint.
	PathMetrics = "/metrics"
	// PathVars is a JSON snapshot in expvar's shape, for a human with curl and
	// no Prometheus.
	PathVars = "/debug/vars"
)

// Handler serves the metrics endpoints.
//
// It is a plain http.Handler with no dependency on the listener, so a test can
// exercise the exposition without binding anything and the loopback rule in
// [Serve] stays the single place a socket is opened.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathMetrics, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(m.Expose()))
	})
	mux.HandleFunc(PathVars, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(clone(m.Snapshot()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "latem agent metrics\n%s\n%s\n", PathMetrics, PathVars)
	})
	return mux
}

// Expose renders the Prometheus text exposition format.
//
// Families are emitted in a fixed order with their HELP and TYPE lines, and
// series within a family are sorted by label value, so two scrapes of an
// unchanged agent produce byte-identical output. That is what makes a golden
// test of this possible, and a golden test is what stops a label being renamed
// by accident — metric names and label names are an API to whatever is
// scraping them.
func (m *Metrics) Expose() string {
	var b strings.Builder
	for _, f := range m.families {
		name := Namespace + "_" + f.name
		fmt.Fprintf(&b, "# HELP %s %s\n", name, f.help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, f.kind)
		for _, s := range f.c.samples() {
			b.WriteString(name)
			b.WriteString(s.suffix)
			writeLabels(&b, s.labels)
			b.WriteByte(' ')
			b.WriteString(formatValue(s.value))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func writeLabels(b *strings.Builder, labels []labelPair) {
	if len(labels) == 0 {
		return
	}
	// Sorted so the rendering is stable regardless of map iteration order.
	pairs := append([]labelPair(nil), labels...)
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].name < pairs[j].name })

	b.WriteByte('{')
	for i, l := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(l.value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// escapeLabelValue applies the three escapes the exposition format defines.
//
// It matters more here than it looks. Label values carry runner names, cache
// names, and an agent version, and while §8 constrains runner names to a
// character class, the version string comes from `git describe` and a cache
// name comes from a repository's own jobs.yaml. An unescaped quote in either
// produces a scrape that fails to parse, and a Prometheus that reports the
// whole agent as down.
func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func formatValue(v float64) string {
	// Whole numbers render without a decimal point; the format is text and
	// a counter reading "12" is easier to eyeball than "1.2e+01".
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
