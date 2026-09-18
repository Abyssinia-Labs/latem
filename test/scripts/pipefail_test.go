// Package scripts holds the checks over the shell this repository owns.
//
// It exists because of RUNNER_SPEC amendment A37, which recorded a measured
// defect and a rule. The defect: `unzip -l file | grep -q PATTERN` under
// `set -o pipefail` returned a false negative **17 times in 200 runs**. grep -q
// exits at its first match, the producer takes SIGPIPE, and the pipeline
// reports failure on input that matched. It is timing-dependent, so it passes
// every hand test and fails on a loaded machine — which is exactly what a
// runner is.
//
// The rule is the part that needed this file: **a fix recorded only as a
// comment is not a fix.** That defect was fixed once, with an explanatory
// comment left beside it, and then written again two hours later in a sibling
// script by the same hand. A37 says a hazard that can recur needs something
// mechanical — a shared helper, a lint rule, or a test. This is the test.
package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ownedShell walks the shell scripts this repository is responsible for.
//
// Everything vendored or generated is skipped, and the list of exclusions is
// specific rather than clever: `examples/expo-app/ios/Pods`, the Xcode build
// intermediates, `node_modules` and `vendor` contain hundreds of scripts
// written by other people, and a check that flagged those would be turned off
// within a week.
func ownedShell(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)

	skip := func(path string) bool {
		for _, part := range []string{
			"node_modules", "vendor", "/Pods/", "Target Support Files",
			"/build/", "/.git/", "/dist/",
		} {
			if strings.Contains(path, part) {
				return true
			}
		}
		return false
	}

	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".sh") || skip(path) {
			return nil //nolint:nilerr // an unreadable tree is the walk's problem, not this check's
		}
		body, readErr := os.ReadFile(path) //nolint:gosec // paths come from walking this repository
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// earlyExit matches a pipeline stage that stops reading before its producer
// stops writing, which is what turns a pipe into a SIGPIPE under pipefail.
//
//	grep -q   exits at the first match
//	grep -m N exits at the Nth
//	grep -l   exits at the first matching file
//	head      exits after its count
//
// A stage that reads to the end — plain `grep`, `sed`, `awk`, `cut` — cannot
// produce this, which is why `aapt dump badging | grep -E …` is not a finding.
var earlyExit = regexp.MustCompile(`^(head\b|grep\b[^|]*\s-[a-zA-Z]*[qml]\b)`)

// safeMarker is the documented opt-out, in the same spirit as INV-3's
// allowlist: a hazard somebody has looked at and can explain is not the same as
// one nobody has seen. `|| true` counts too — a pipeline whose failure is
// explicitly discarded cannot report a false negative to anything.
const safeMarker = "# pipefail-safe:"

type finding struct {
	file string
	line int
	text string
}

// scanPipefailHazards reports every pipeline under `pipefail` whose last stage
// exits early.
func scanPipefailHazards(file, body string) []finding {
	if !strings.Contains(body, "pipefail") {
		return nil
	}

	var out []finding
	lines := strings.Split(body, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, safeMarker) || strings.Contains(line, "|| true") {
			continue
		}
		// The line above may carry the marker instead, so a long explanation
		// does not have to share the line it explains.
		if i > 0 && strings.Contains(lines[i-1], safeMarker) {
			continue
		}

		// Split on single pipes only: `||` is not a pipeline.
		stages := splitPipeline(line)
		if len(stages) < 2 {
			continue
		}
		// Every stage but the first, not just the last. `xcodebuild -version |
		// head -1 | cut -d' ' -f2` fails the same way: head stops reading,
		// xcodebuild takes SIGPIPE, and pipefail reports the pipeline as failed
		// however well `cut` did. The first stage is exempt because it has no
		// producer upstream of it to kill.
		for _, stage := range stages[1:] {
			if earlyExit.MatchString(strings.TrimSpace(stage)) {
				out = append(out, finding{file: file, line: i + 1, text: line})
				break
			}
		}
	}
	return out
}

// splitPipeline splits on `|` while leaving `||` alone.
func splitPipeline(line string) []string {
	var stages []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '|' {
			if i+1 < len(line) && line[i+1] == '|' {
				// `||` ends any pipeline that preceded it; what follows is a
				// separate command and its own business.
				break
			}
			stages = append(stages, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	stages = append(stages, cur.String())
	return stages
}

// TestA37_NoProducerPipedIntoAnEarlyExitingConsumer is the mechanical half of
// the rule.
func TestA37_NoProducerPipedIntoAnEarlyExitingConsumer(t *testing.T) {
	// Negative control first. The detector is two regexes and a splitter, and
	// either could be wrong in a way that finds nothing anywhere — which is
	// indistinguishable from a clean tree. A25 recorded that exact failure in a
	// different test, so this one proves it fires before it is trusted to
	// report an absence.
	control := "set -euo pipefail\nunzip -l \"$AAB\" | grep -q META-INF/\n"
	if got := scanPipefailHazards("control.sh", control); len(got) != 1 {
		t.Fatalf("the detector found %d hazards in a script that contains one; every assertion "+
			"below would pass against anything", len(got))
	}
	// And it must not fire on the fixed form, or the only way to satisfy it
	// would be to stop using grep.
	fixed := "set -euo pipefail\nlisting=$(unzip -l \"$AAB\")\ncase $listing in *META-INF/*) ;; esac\n"
	if got := scanPipefailHazards("fixed.sh", fixed); len(got) != 0 {
		t.Fatalf("the detector flagged the fixed form: %v", got)
	}
	// A script with no pipefail is a different situation: the pipeline's exit
	// status is its last stage's, so an early exit cannot report a false
	// failure. Flagging those would be noise.
	noPipefail := "set -eu\nsecurity find-identity | grep -q Distribution\n"
	if got := scanPipefailHazards("nopipefail.sh", noPipefail); len(got) != 0 {
		t.Fatalf("the detector flagged a script without pipefail: %v", got)
	}

	var findings []finding
	for file, body := range ownedShell(t) {
		findings = append(findings, scanPipefailHazards(file, body)...)
	}

	for _, f := range findings {
		t.Errorf("%s:%d pipes a producer into a consumer that exits early, under pipefail "+
			"(amendment A37, measured at 17 false negatives in 200 runs):\n    %s\n"+
			"Capture the output to a variable and match on it, or use a tool that answers the "+
			"question directly — `jarsigner -verify` rather than grepping a zip listing. If this "+
			"one is genuinely safe, say why with a `%s <reason>` comment.",
			f.file, f.line, f.text, safeMarker)
	}
}

// moduleRoot walks up to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
