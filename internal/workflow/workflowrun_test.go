package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func runScope(needs map[string]protocol.NeedResult, inputs map[string]any) expr.Scope {
	sc := testScope()
	sc.Contexts["needs"] = NeedsContext(needs)
	if inputs != nil {
		sc.Contexts["inputs"] = inputs
	}
	return sc
}

func compileRun(t *testing.T, wf, job string, sc expr.Scope) (*registry.Job, error) {
	t.Helper()
	return CompileWith(t.Context(), parse(t, wf), job, sc, Options{WorkflowRun: true})
}

const runWorkflow = `
on:
  workflow_dispatch:
    inputs:
      dry: {type: boolean, default: false}
      count: {type: number}
jobs:
  lint:
    runs-on: [linux]
    steps: [{run: make lint}]
  web:
    needs: lint
    runs-on: [linux]
    outputs:
      version: ${{ steps.v.outputs.version }}
      stamped: v${{ steps.v.outputs.version }}-${{ needs.lint.outputs.tag }}
    steps:
      - id: v
        run: echo "version=1.2.3" >> "$GITHUB_OUTPUT"
        env:
          TAG: ${{ needs.lint.outputs.tag }}
          TOKEN: ${{ github.token }}
  always:
    needs: lint
    if: always()
    runs-on: [linux]
    steps: [{run: ./report.sh}]
  dry:
    if: inputs.dry
    runs-on: [linux]
    steps: [{run: ./deploy.sh --dry}]
  late:
    if: steps.x.outputs.y
    runs-on: [linux]
    steps: [{run: x}]
`

// A workflow run's job reads its needs, answers its if: against them, and
// reports outputs as it ends (A52).
func TestWorkflowRunJob(t *testing.T) {
	ok := map[string]protocol.NeedResult{"lint": {Result: protocol.NeedSuccess, Outputs: map[string]string{"tag": "abc"}}}
	failed := map[string]protocol.NeedResult{"lint": {Result: protocol.NeedFailure}}
	skipped := map[string]protocol.NeedResult{"lint": {Result: protocol.NeedSkipped}}

	job, err := compileRun(t, runWorkflow, "web", runScope(ok, nil))
	if err != nil {
		t.Fatal(err)
	}
	if job.Skip {
		t.Fatal("web skipped after its need succeeded")
	}
	// Known when the run is admitted, so a plain value — as data, never in
	// the command (A42).
	if tag := job.Steps[0].Env["TAG"]; tag != "abc" {
		t.Errorf("needs.lint.outputs.tag = %q", tag)
	}
	tok, err := job.Steps[0].Late["TOKEN"].Resolve(registry.StepScope{Secrets: map[string]string{TokenSecret: "ghs_x"}})
	if err != nil || tok != "ghs_x" {
		t.Errorf("github.token = %q, %v; it is the job's token, late", tok, err)
	}
	if !strings.Contains(strings.Join(job.Secrets, ","), TokenSecret) {
		t.Errorf("secrets %v: reading github.token names the token", job.Secrets)
	}
	at := registry.StepScope{Outputs: map[string]map[string]string{"v": {"version": "1.2.3"}}}
	for name, want := range map[string]string{"version": "1.2.3", "stamped": "v1.2.3-abc"} {
		got, err := job.Outputs[name].Resolve(at)
		if err != nil || got != want {
			t.Errorf("output %s = %q, %v; want %q", name, got, err, want)
		}
	}

	for _, c := range []struct {
		job   string
		needs map[string]protocol.NeedResult
		in    map[string]any
		skip  bool
	}{
		{"web", failed, nil, true},                         // implicit success()
		{"web", skipped, nil, true},                        // a skipped need is not a success
		{"always", failed, nil, false},                     // always() runs after a failure
		{"always", skipped, nil, false},                    // and after a skip
		{"dry", nil, map[string]any{"dry": "true"}, false}, // a boolean input is a boolean
		{"dry", nil, map[string]any{"dry": "false"}, true}, // "false" is not truthy here
	} {
		job, err := compileRun(t, runWorkflow, c.job, runScope(c.needs, c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.job, err)
		}
		if job.Skip != c.skip {
			t.Errorf("%s with needs %v, inputs %v: skip = %v, want %v", c.job, c.needs, c.in, job.Skip, c.skip)
		}
	}

	if _, err := compileRun(t, runWorkflow, "late", runScope(ok, nil)); err == nil ||
		!strings.Contains(err.Error(), "reads steps") {
		t.Errorf("an if: reading steps compiled: %v", err)
	}
	// Outside a workflow run, needs and a job's if: are still refused.
	if _, err := CompileWith(t.Context(), parse(t, runWorkflow), "web", testScope(), Options{}); err == nil ||
		!strings.Contains(err.Error(), "not a workflow run's job") {
		t.Errorf("needs outside a workflow run: %v", err)
	}
}

// A run names its workflow file, so a job key shared by two files is two jobs.
func TestResolveWorkflowJob(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, Dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("checks.yml", "name: Checks\non: push\njobs:\n  web:\n    runs-on: [linux]\n    steps: [{run: make check}]\n")
	write("deploy.yml", "on: push\njobs:\n  web:\n    if: github.ref == 'refs/heads/prod'\n    runs-on: [linux]\n    steps: [{run: make deploy}]\n")

	reg, src, err := ResolveWorkflowJob(t.Context(), dir, Dir+"/checks.yml", "web", runScope(nil, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if src.Workflow != "Checks" || !strings.Contains(reg.Jobs["web"].Steps[0].Run, "make check") {
		t.Errorf("resolved %+v %+v", src, reg.Jobs["web"])
	}
	reg, _, err = ResolveWorkflowJob(t.Context(), dir, Dir+"/deploy.yml", "web", runScope(nil, nil), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if j := reg.Jobs["web"]; !j.Skip || j.SkipReason != "github.ref == 'refs/heads/prod'" {
		t.Errorf("deploy's web: %+v", j)
	}
	if _, _, err := ResolveWorkflowJob(t.Context(), dir, "../x.yml", "web", runScope(nil, nil), Options{}); err == nil {
		t.Error("a path outside .github/workflows resolved")
	}
}

func TestVarsStopJob(t *testing.T) {
	w := parse(t, `on: push
jobs:
  stopped:
    if: vars.LATEM != 'true'
    runs-on: linux
    steps: [{run: "true"}]
  running:
    if: vars.LATEM == 'true'
    runs-on: linux
    steps: [{run: "true"}]
  on-an-event:
    if: github.event_name == 'release'
    runs-on: linux
    steps: [{run: "true"}]
  plain:
    runs-on: linux
    steps: [{run: "true"}]
`)
	sc := runScope(nil, nil)
	vars := func(string) map[string]string { return map[string]string{"LATEM": "true"} }
	for job, want := range map[string]bool{"stopped": true, "running": false, "on-an-event": false, "plain": false, "absent": false} {
		if got := VarsStopJob(w, job, sc, vars); got != want {
			t.Errorf("%s: got %v, want %v", job, got, want)
		}
	}
	if VarsStopJob(w, "stopped", sc, nil) {
		t.Error("with no variables LATEM is not 'true', so its if: is true")
	}
}
