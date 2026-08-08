package logstream

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// gradleLog approximates the output this has to survive. Verbose Gradle is the
// worst realistic case: tens of thousands of short lines, almost none of which
// contain anything worth redacting, so the cost is dominated by scanning
// misses rather than by building replacements.
func gradleLog(lines int) []string {
	templates := []string{
		"> Task :app:compileDebugKotlin",
		"> Task :app:mergeDebugResources UP-TO-DATE",
		"w: file:///src/app/src/main/java/com/example/Foo.kt:42:9 variable 'x' is never used",
		"Download https://repo.maven.apache.org/maven2/org/example/lib/1.2.3/lib-1.2.3.jar",
		"Note: Some input files use or override a deprecated API.",
		"AAPT2 aapt2-8.1.0-10154469-osx Daemon #0: shutdown",
		"BUILD SUCCESSFUL in 1m 42s",
		"321 actionable tasks: 118 executed, 203 up-to-date",
	}
	out := make([]string, lines)
	for i := range out {
		out[i] = templates[i%len(templates)]
	}
	return out
}

// secretSet is a realistic declaration: the iOS and Android jobs in
// RUNNER_SPEC §18 declare nine between them, and each expands into six or
// seven literals under the §16 rules.
func secretSet() [][]byte {
	out := [][]byte{[]byte(pemFixture)}
	for i := 0; i < 12; i++ {
		out = append(out, []byte(fmt.Sprintf("secret-value-number-%02d-abcdefghij", i)))
	}
	return out
}

func BenchmarkRedact_GradleLog(b *testing.B) {
	r := NewRedactor()
	for _, s := range secretSet() {
		r.Register(s)
	}
	lines := gradleLog(1000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			_ = r.Redact(l)
		}
	}
}

// The floor: how fast is it with nothing registered? The gap between this and
// the benchmark above is what registration costs per line.
func BenchmarkRedact_PatternsOnly(b *testing.B) {
	r := NewRedactor()
	lines := gradleLog(1000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, l := range lines {
			_ = r.Redact(l)
		}
	}
}

// A line that actually contains a secret exercises the replacement path.
func BenchmarkRedact_Hit(b *testing.B) {
	r := NewRedactor()
	for _, s := range secretSet() {
		r.Register(s)
	}
	line := "deploying with token secret-value-number-03-abcdefghij to production"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Redact(line)
	}
}

// End to end: split, redact, sequence, fan out to a discarding sink.
func BenchmarkPipeline(b *testing.B) {
	r := NewRedactor()
	for _, s := range secretSet() {
		r.Register(s)
	}
	body := strings.Join(gradleLog(1000), "\n") + "\n"

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := NewEmitter("run_1", r, discardSink{})
		_ = SplitLines(strings.NewReader(body), MaxLineBytes, func(line string, dropped int) error {
			e.EmitTruncated(protocol.StreamStdout, line, dropped)
			return nil
		})
	}
}

func BenchmarkRegister_PEM(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := NewRedactor()
		r.Register([]byte(pemFixture))
	}
}

type discardSink struct{}

func (discardSink) Write([]protocol.LogFrame) error { return nil }
func (discardSink) Close() error                    { return nil }

var _ io.Writer = io.Discard
