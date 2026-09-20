package run

import (
	"context"
	"io"
	"runtime"
	"slices"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The labels the job matched on are the source of truth; the agent's own
// GOOS/GOARCH is only the fallback. A docker runner on an arm64 Mac runs
// linux steps, and only its labels can say so.
func TestRunnerFactsComeFromLabels(t *testing.T) {
	f := runnerFacts(&Runner{Name: "mini", Labels: []string{"self-hosted", "Linux", "X64"}})
	if f.OS != "linux" || f.Arch != "amd64" || f.Name != "mini" {
		t.Errorf("facts = %+v, want linux/amd64/mini from the labels", f)
	}

	f2 := runnerFacts(&Runner{Name: "mini", Labels: []string{"xcode"}})
	if f2.OS != runtime.GOOS || f2.Arch != runtime.GOARCH {
		t.Errorf("with no os/arch label, facts = %+v, want the agent's own", f2)
	}

	if f3 := runnerFacts(nil); f3.OS != runtime.GOOS {
		t.Errorf("nil runner should still answer: %+v", f3)
	}
}

func TestStatusAfterFoldsTheFirstFailure(t *testing.T) {
	s := registry.StatusSuccess
	s = statusAfter(s, outcome{state: protocol.StateSucceeded})
	if s != registry.StatusSuccess {
		t.Errorf("a success should leave the status alone: %v", s)
	}
	s = statusAfter(s, outcome{state: protocol.StateFailed, code: protocol.ErrStep})
	if s != registry.StatusFailure {
		t.Errorf("a failure should turn the status: %v", s)
	}
	// Sticky: a later success does not turn it back.
	if got := statusAfter(s, outcome{state: protocol.StateSucceeded}); got != registry.StatusFailure {
		t.Errorf("status should stay failed after a later success: %v", got)
	}
	if got := statusAfter(registry.StatusSuccess, outcome{state: protocol.StateCancelled}); got != registry.StatusCancelled {
		t.Errorf("cancelled: %v", got)
	}
}

// A failure leaves later steps to their conditions. Cancellation and the job
// budget do not — §11 says a cancelled run stops, and a job past its budget
// has no time to spend on a cleanup that would itself be cut short. One
// step's own timeout is a failure like any other.
func TestStopsTheRun(t *testing.T) {
	cases := map[string]struct {
		out  outcome
		stop bool
	}{
		"step failure":  {outcome{state: protocol.StateFailed, code: protocol.ErrStep}, false},
		"step timeout":  {outcome{state: protocol.StateFailed, code: protocol.ErrStepTimeout}, false},
		"job timeout":   {outcome{state: protocol.StateFailed, code: protocol.ErrTimeout}, true},
		"cancelled":     {outcome{state: protocol.StateCancelled}, true},
		"executor fail": {outcome{state: protocol.StateFailed, code: protocol.ErrExecutor}, false},
	}
	for name, c := range cases {
		if got := stopsTheRun(c.out); got != c.stop {
			t.Errorf("%s: stopsTheRun = %v, want %v", name, got, c.stop)
		}
	}
}

// The run reports what broke it. A cancel arriving during a cleanup step that
// ran because of an earlier failure is still a cancel — that is the one
// stopping outcome that outranks the first failure, because it is what the
// operator did.
func TestSettleReportsTheFirstFailure(t *testing.T) {
	x := &execution{}
	first := outcome{state: protocol.StateFailed, code: protocol.ErrStep, msg: "build broke"}

	got := x.settle(outcome{state: protocol.StateFailed, code: protocol.ErrTimeout}, &first)
	if got.msg != "build broke" {
		t.Errorf("a job timeout after a failure should report the failure: %+v", got)
	}
	got = x.settle(outcome{state: protocol.StateCancelled}, &first)
	if got.state != protocol.StateCancelled {
		t.Errorf("a cancel should be reported as a cancel: %+v", got)
	}
	got = x.settle(outcome{state: protocol.StateFailed, code: protocol.ErrTimeout}, nil)
	if got.code != protocol.ErrTimeout {
		t.Errorf("with no earlier failure, the stopping outcome is the result: %+v", got)
	}
}

// The workflow the two end-to-end cases below run. No jobs.yaml is written,
// so gate 11 resolves the job from here — which is also the only way a step
// can carry a condition, since the native format cannot write one.
const conditionedWorkflow = `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - name: build
        run: make
      - name: after
        run: echo unconditioned
      - name: cleanup
        if: failure()
        run: echo status=${{ job.status }}
      - name: report
        if: ${{ always() }}
        run: ./report.sh
`

func executedNames(steps []executor.Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// A failure leaves later steps to their conditions: failure() and always()
// run, the unconditioned step does not, and the run reports the step that
// broke it — not the cleanup that ran afterwards.
func TestExecute_FailureLeavesLaterStepsToTheirConditions(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		if step.Name == "build" {
			return 3, nil
		}
		return 0, nil
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", conditionedWorkflow)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
		t.Fatalf("got %s/%s, want failed/E_STEP — the first failure is the result", res.State, res.ErrorCode)
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Errorf("exit code = %v, want build's 3, not the cleanup's 0", res.ExitCode)
	}

	steps := h.env.executed()
	want := []string{"build", "cleanup", "report"}
	if got := executedNames(steps); !slices.Equal(got, want) {
		t.Fatalf("executed %v, want %v (after is unconditioned and must be skipped)", got, want)
	}

	// The late value reached the cleanup step as data, computed against the
	// failed status — and the command never changed shape.
	cleanup := steps[1]
	if cleanup.Run != `echo status="$GHA_EXPR_1"` {
		t.Errorf("cleanup run = %q", cleanup.Run)
	}
	if cleanup.Env["GHA_EXPR_1"] != "failure" {
		t.Errorf("job.status at the cleanup step = %q, want failure", cleanup.Env["GHA_EXPR_1"])
	}
}

// And with nothing failing, failure() is the step that does not run.
func TestExecute_ConditionsOnAGreenRun(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", conditionedWorkflow)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s, want succeeded", res.State, res.ErrorCode)
	}
	want := []string{"build", "after", "report"}
	if got := executedNames(h.env.executed()); !slices.Equal(got, want) {
		t.Errorf("executed %v, want %v", got, want)
	}
}

// The whole channel end to end: a step writes $GITHUB_OUTPUT, and a later
// step's expression reads it as data — from a failed step too, which is the
// `if: failure()` idiom.
func TestExecute_StepOutputsReachLaterSteps(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		if step.Name == "build" {
			return 1, nil
		}
		return 0, nil
	}
	h.env.outputs = func(step executor.Step) map[string]string {
		if step.Name == "build" {
			return map[string]string{"log": "build.log", "sha": "; rm -rf /"}
		}
		return nil
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - id: build
        name: build
        run: make
      - name: report
        if: failure()
        run: cat ${{ steps.build.outputs.log }} ${{ steps.build.outputs.sha }}
`)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
		t.Fatalf("got %s/%s, want failed/E_STEP", res.State, res.ErrorCode)
	}
	steps := h.env.executed()
	if got := executedNames(steps); !slices.Equal(got, []string{"build", "report"}) {
		t.Fatalf("executed %v", got)
	}
	report := steps[1]
	if report.Run != `cat "$GHA_EXPR_1" "$GHA_EXPR_2"` {
		t.Errorf("run = %q — an output must never become command text", report.Run)
	}
	if report.Env["GHA_EXPR_1"] != "build.log" || report.Env["GHA_EXPR_2"] != "; rm -rf /" {
		t.Errorf("outputs did not reach the later step as data: %+v", report.Env)
	}
}
