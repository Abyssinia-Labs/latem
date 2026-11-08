package run

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// The artifact step end to end: an upload that runs because of `if: always()`
// after a failure, its files collected at the step and reported with the run,
// the hidden file left out, and a later step in the same run restoring it.
func TestExecute_UploadAndDownloadArtifactAtTheStep(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	var restored string
	var runID string
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		switch step.Name {
		case "build":
			return 2, nil
		case "check":
			// Read from inside the run, the way a later step would: the
			// workspace is removed at teardown, so after Execute it is gone.
			// `path: out` roots the artifact at out, as upload-artifact
			// does, so the download puts build.log straight into restored.
			b, err := os.ReadFile(filepath.Join(h.source.root, runID, "restored", "build.log"))
			if err == nil {
				restored = string(b)
			}
		}
		return 0, nil
	}
	o := h.build()

	req := h.request("test")
	runID = req.ID
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - name: build
        run: make
      - uses: actions/upload-artifact@v4
        if: always()
        with: {name: logs, path: out}
      - uses: actions/download-artifact@v4
        if: always()
        with: {name: logs, path: restored}
      - name: check
        if: always()
        run: cat restored/build.log
`)
	h.writeWorkspaceFile(req.ID, "out/build.log", "it broke")
	h.writeWorkspaceFile(req.ID, "out/.env", "SECRET=1")

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
		t.Fatalf("got %s/%s, want the build's failure to stand", res.State, res.ErrorCode)
	}

	var paths []string
	for _, a := range res.Artifacts {
		paths = append(paths, a.Path)
	}
	if !slices.Equal(paths, []string{"logs/build.log"}) {
		t.Errorf("reported artifacts = %v, want only the log (the .env is hidden)", paths)
	}
	// Neither artifact step reached the executor: they are the run loop's.
	if got := executedNames(h.env.executed()); !slices.Equal(got, []string{"build", "check"}) {
		t.Errorf("executor ran %v", got)
	}
	if restored != "it broke" {
		t.Errorf("a later step read %q from the restored artifact", restored)
	}
}

// A50: continue-on-error lets the job carry on as though the step had not
// failed, and the steps context tells the truth about what happened — the
// failure's outcome, continue-on-error's conclusion, a skipped step skipped.
func TestExecute_ContinueOnErrorAndTheStepsContext(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.env.exec = func(_ context.Context, step executor.Step, _, _ io.Writer) (int, error) {
		if step.Name == "flaky" || step.Name == "broken" {
			return 1, nil
		}
		return 0, nil
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
      - id: flaky
        name: flaky
        run: exit 1
        continue-on-error: true
      - id: cleanup
        name: cleanup
        if: failure()
        run: never
      - name: report
        run: echo ${{ steps.flaky.outcome }} ${{ steps.flaky.conclusion }} ${{ steps.cleanup.outcome }}
      - id: broken
        name: broken
        run: exit 1
      - name: after
        if: always()
        run: echo ${{ steps.broken.outcome }} ${{ steps.broken.conclusion }}
`)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// broken has no continue-on-error, so it is what the run reports.
	if res.State != protocol.StateFailed || res.ErrorCode != protocol.ErrStep {
		t.Fatalf("got %s/%s, want failed/E_STEP from the step without continue-on-error", res.State, res.ErrorCode)
	}
	steps := h.env.executed()
	if got := executedNames(steps); !slices.Equal(got, []string{"flaky", "report", "broken", "after"}) {
		t.Fatalf("executed %v — failure() should stay false after a forgiven step", got)
	}
	report := steps[1].Env
	if got := report["GHA_EXPR_1"] + "|" + report["GHA_EXPR_2"] + "|" + report["GHA_EXPR_3"]; got != "failure|success|skipped" {
		t.Errorf("steps context = %q, want failure|success|skipped", got)
	}
	after := steps[3].Env
	if got := after["GHA_EXPR_1"] + "|" + after["GHA_EXPR_2"]; got != "failure|failure" {
		t.Errorf("an unforgiven failure = %q, want failure|failure", got)
	}
}

// A51: what a JavaScript action's pre and main write to $GITHUB_STATE reaches
// that use's later steps as STATE_*, and no other use's.
func TestExecute_ActionStateFlowsBetweenItsSteps(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.env.state = func(step executor.Step) map[string]string {
		switch {
		case step.Env["GHA_ACTION_ENTRY"] == "pre.js":
			return map[string]string{"cacheKey": "k-" + step.Env["INPUT_ID"]}
		case step.Env["GHA_ACTION_ENTRY"] == "main.js" && step.Env["INPUT_ID"] == "1":
			return map[string]string{"installed": "yes"}
		}
		return nil
	}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/actions/tool/action.yml",
		"inputs:\n  id:\n    required: true\nruns:\n  using: node20\n  pre: pre.js\n  main: main.js\n  post: post.js\n")
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - uses: ./.github/actions/tool
        with:
          id: "1"
      - uses: ./.github/actions/tool
        with:
          id: "2"
`)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s (%s)", res.State, res.ErrorCode, res.ErrorMsg)
	}
	steps := h.env.executed()
	if len(steps) != 6 {
		t.Fatalf("executed %d steps, want pre, pre, main, main, post, post", len(steps))
	}
	byEntry := func(entry, id string) executor.Step {
		for _, s := range steps {
			if s.Env["GHA_ACTION_ENTRY"] == entry && s.Env["INPUT_ID"] == id {
				return s
			}
		}
		t.Fatalf("no %s step for use %s", entry, id)
		return executor.Step{}
	}
	if got := byEntry("main.js", "1").Env["STATE_cacheKey"]; got != "k-1" {
		t.Errorf("main of use 1: STATE_cacheKey = %q, want what its pre saved", got)
	}
	post1 := byEntry("post.js", "1").Env
	if post1["STATE_cacheKey"] != "k-1" || post1["STATE_installed"] != "yes" {
		t.Errorf("post of use 1 = %v, want both saves", post1)
	}
	post2 := byEntry("post.js", "2").Env
	if post2["STATE_cacheKey"] != "k-2" || post2["STATE_installed"] != "" {
		t.Errorf("post of use 2 = %v, want only its own state", post2)
	}
}
