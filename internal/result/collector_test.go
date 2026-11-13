package result

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const testRun = "r_0123456789abcdef01234567"

func frames(stream protocol.Stream, lines ...string) []protocol.LogFrame {
	out := make([]protocol.LogFrame, 0, len(lines))
	for _, l := range lines {
		out = append(out, protocol.LogFrame{RunID: testRun, Stream: stream, Line: l})
	}
	return out
}

func fileLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

func newCollector(t *testing.T) (*Collector, string) {
	t.Helper()
	dir := t.TempDir()
	c := NewCollector(dir, Header{RunID: testRun, Repo: "app", Job: "test", Ref: "main",
		Inputs: map[string]string{"tier": "2"}})
	return c, dir
}

func exit(n int) *int { return &n }

// A run's result is its steps, how each ended, and the problems each named,
// errors first; it is on disk as each step ends and when the run does.
func TestCollectorBuildsARunsResult(t *testing.T) {
	c, dir := newCollector(t)
	started := time.Now().Add(-time.Minute)
	c.Describe("refs/heads/main", strings.Repeat("a", 40), "linux", "warm", started, "/work/app")

	if _, err := Read(dir, testRun); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a run with no step yet has a result: %v", err)
	}

	c.StepStart("lint")
	_ = c.Write(frames(protocol.StreamStderr, fileLines(t, "cargo-clippy.txt")...))
	c.StepEnd(OutcomeSuccess, false, exit(0), "")

	mid, err := Read(dir, testRun)
	if err != nil {
		t.Fatalf("the result after the first step: %v", err)
	}
	if mid.Settled || len(mid.Steps) != 1 || mid.Steps[0].Warnings != 3 || mid.Warnings != 3 {
		t.Fatalf("mid-run, the result is %+v", mid)
	}

	c.StepStart("test")
	_ = c.Write(frames(protocol.StreamAgent, "[latem] src/agent.rs:1:1: error: latem's own lines are not read"))
	_ = c.Write(frames(protocol.StreamStdout, fileLines(t, "cargo-test.txt")...))
	c.StepEnd(OutcomeFailure, false, exit(101), "")
	c.StepSkipped("deploy", "an earlier step failed")
	if err := c.Finish(protocol.StateFailed, protocol.ErrStep, `step "test" exited 101`, exit(101)); err != nil {
		t.Fatal(err)
	}

	r, err := Read(dir, testRun)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Settled || r.State != protocol.StateFailed || r.ErrorCode != "E_STEP" || *r.ExitCode != 101 {
		t.Errorf("the run's ending: %+v", r)
	}
	if r.Runner != "linux" || r.Workspace != "warm" || r.Inputs["tier"] != "2" || r.DurationMs < 59_000 {
		t.Errorf("the run's description: %+v", r)
	}
	if len(r.Steps) != 3 || r.Steps[1].Outcome != OutcomeFailure || *r.Steps[1].ExitCode != 101 ||
		r.Steps[1].Errors != 2 || r.Steps[2].Outcome != OutcomeSkipped {
		t.Errorf("the steps: %+v", r.Steps)
	}
	// The test step printed the build's two warnings twice over, which the
	// lint step had already named in a third: each step's are its own, and
	// within a step a warning printed twice is one.
	if r.Errors != 2 || r.Warnings != 5 || len(r.Problems) != 7 {
		t.Fatalf("%d errors, %d warnings, %d kept; want 2, 5 and 7:\n%s", r.Errors, r.Warnings, len(r.Problems), show(r.Problems))
	}
	if r.Problems[0].Severity != SeverityError || r.Problems[1].Severity != SeverityError ||
		r.Problems[0].Test != "tests::adds" || r.Problems[0].Step != "test" || r.Problems[2].Step != "lint" {
		t.Errorf("errors are not first, in the order printed:\n%s", show(r.Problems))
	}
}

// A step that passed keeps its warnings and loses the errors the readers
// found: it was showing the shape of one, not having one. What it named
// itself with ::error stays.
func TestCollectorAStepThatPassedHasNoErrorsButItsOwn(t *testing.T) {
	c, dir := newCollector(t)
	c.StepStart("ui tests")
	_ = c.Write(frames(protocol.StreamStdout, fileLines(t, "cargo-check.txt")...))
	_ = c.Write(frames(protocol.StreamStderr, "src/x.c:1:1: warning: kept"))
	c.Annotate("error", "the step says so itself", map[string]string{"file": "a.rs", "line": "3", "col": "7", "title": "T1"})
	c.Annotate("notice", "not a problem", nil)
	c.StepEnd(OutcomeSuccess, false, exit(0), "")
	_ = c.Finish(protocol.StateSucceeded, "", "", exit(0))

	r, err := Read(dir, testRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.Errors != 1 || r.Warnings != 1 || len(r.Problems) != 2 {
		t.Fatalf("kept:\n%s", show(r.Problems))
	}
	want := Problem{Severity: "error", File: "a.rs", Line: 3, Column: 7, Code: "T1",
		Message: "the step says so itself", Tool: "annotation", Step: "ui tests"}
	if r.Problems[0] != want {
		t.Errorf("the step's own error is %+v, want %+v", r.Problems[0], want)
	}
}

// A build that prints ten thousand errors has its first hundred kept and the
// rest counted, and one of any length is cut.
func TestCollectorBoundsWhatItKeeps(t *testing.T) {
	c, dir := newCollector(t)
	c.StepStart("build")
	long := strings.Repeat("é", MaxMessageBytes)
	for i := 1; i <= MaxErrors+25; i++ {
		_ = c.Write(frames(protocol.StreamStderr, "src/a.c:"+itoa(i)+":1: error: "+long))
	}
	for i := 1; i <= MaxWarnings+5; i++ {
		_ = c.Write(frames(protocol.StreamStderr, "src/a.c:"+itoa(i)+":1: warning: w"))
	}
	c.StepEnd(OutcomeFailure, false, exit(1), "")
	_ = c.Finish(protocol.StateFailed, protocol.ErrStep, "", exit(1))

	r, err := Read(dir, testRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.Errors != MaxErrors+25 || r.Warnings != MaxWarnings+5 || r.Dropped != 30 || len(r.Problems) != MaxErrors+MaxWarnings {
		t.Errorf("%d errors, %d warnings, %d dropped, %d kept", r.Errors, r.Warnings, r.Dropped, len(r.Problems))
	}
	if msg := r.Problems[0].Message; len(msg) > MaxMessageBytes+8 || !strings.HasSuffix(msg, " …") {
		t.Errorf("a long message is %d bytes and ends %q", len(msg), msg[len(msg)-8:])
	}
	info, err := os.Stat(filepath.Join(dir, testRun+Suffix))
	if err != nil || info.Size() > 1<<20 {
		t.Errorf("the result file is %d bytes (%v)", info.Size(), err)
	}
}

// A run stopped under a step ends that step as the run ended.
func TestCollectorAStepTheRunWasStoppedUnder(t *testing.T) {
	for state, want := range map[protocol.RunState]string{
		protocol.StateCancelled: OutcomeCancelled,
		protocol.StateTimedOut:  OutcomeTimedOut,
		protocol.StateFailed:    OutcomeFailure,
	} {
		c, dir := newCollector(t)
		c.StepStart("build")
		_ = c.Finish(state, "", "", nil)
		r, err := Read(dir, testRun)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Steps) != 1 || r.Steps[0].Outcome != want {
			t.Errorf("%s: the open step ended %+v, want %s", state, r.Steps, want)
		}
	}
}

// A run with nothing wrong says so with empty lists, not with nulls a caller
// has to test for before it loops.
func TestCollectorWritesEmptyListsNotNulls(t *testing.T) {
	c, dir := newCollector(t)
	c.StepStart("test")
	c.StepEnd(OutcomeSuccess, false, exit(0), "")
	_ = c.Finish(protocol.StateSucceeded, "", "", exit(0))
	data, err := os.ReadFile(filepath.Join(dir, testRun+Suffix))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"problems":[]`) || strings.Contains(string(data), "null") {
		t.Errorf("the file holds a null: %s", data)
	}

	c, dir = newCollector(t)
	_ = c.Finish(protocol.StateRejected, protocol.ErrUnknownJob, "no such job", nil)
	data, _ = os.ReadFile(filepath.Join(dir, testRun+Suffix))
	if !strings.Contains(string(data), `"steps":[]`) || !strings.Contains(string(data), `"problems":[]`) {
		t.Errorf("a run with no step: %s", data)
	}
}

func TestPathRefusesWhatIsNotARunID(t *testing.T) {
	for _, id := range []string{"", "../x", "a/b", strings.Repeat("x", 200)} {
		if _, err := Path(t.TempDir(), id); err == nil {
			t.Errorf("Path took %q", id)
		}
	}
}
