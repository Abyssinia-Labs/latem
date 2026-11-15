package workflow

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// planVars is a plan's variables as config.toml has them: read with no way
// to fail.
func planVars(m map[string]string) func() (map[string]string, error) {
	return func() (map[string]string, error) { return m, nil }
}

// errVaultDown stands for OpenVault's service not answering (A96).
var errVaultDown = errors.New("openvault: the service is not running")

// countingVars answers vars and counts how often it was asked.
type countingVars struct {
	asked int
	err   error
	vars  map[string]string
}

func (c *countingVars) plan() (map[string]string, error) {
	c.asked++
	return c.vars, c.err
}

func (c *countingVars) job(string) (map[string]string, error) {
	c.asked++
	return c.vars, c.err
}

// A96: the variables are read only when an expression reads one, so a plan
// none of whose expressions does never waits on OpenVault.
func TestPlan_VariablesAreReadOnlyWhenAnExpressionReadsOne(t *testing.T) {
	ev := protocol.Event{Name: EventPush, Ref: "refs/heads/main"}
	none := &countingVars{err: errVaultDown}
	mustPlan(t, map[string]string{"ci.yml": `
on: [push]
jobs:
  test:
    if: github.event_name == 'push'
    runs-on: [linux]
    steps: [{run: make}]
`}, ev, PlanOptions{Vars: none.plan})
	if none.asked != 0 {
		t.Errorf("asked for the variables %d times by a plan that reads none", none.asked)
	}

	some := &countingVars{vars: map[string]string{"RUNNER": "linux"}}
	files := map[string]string{"ci.yml": `
on: [push]
jobs:
  a:
    runs-on: ${{ vars.RUNNER }}
    steps: [{run: make}]
  b:
    runs-on: ${{ vars.RUNNER }}
    steps: [{run: make}]
`}
	plan := mustPlan(t, files, ev, PlanOptions{Vars: some.plan})
	if some.asked != 1 {
		t.Errorf("asked %d times, want once for the whole plan", some.asked)
	}
	if len(plan) != 1 || len(plan[0].Jobs) != 2 || !slices.Equal(plan[0].Jobs[1].Labels, []string{"linux"}) {
		t.Errorf("plan = %+v, want both jobs on the label vars.RUNNER names", plan)
	}
}

// A96: a plan that reads a variable it cannot read is refused whole, with
// the reason, rather than made with the variable empty: `runs-on: ${{
// vars.RUNNER }}` would otherwise place the job on no label at all.
func TestPlan_AVariableThatCannotBeReadRefusesThePlan(t *testing.T) {
	ev := protocol.Event{Name: EventPush, Ref: "refs/heads/main", SHA: planSHA}
	down := &countingVars{err: errVaultDown}
	_, err := PlanEvent(fleetFS(map[string]string{"ci.yml": `
on: [push]
jobs:
  deploy:
    runs-on: ${{ vars.RUNNER }}
    steps: [{run: make}]
`}), ev, PlanOptions{Vars: down.plan})
	if !errors.Is(err, errVaultDown) {
		t.Fatalf("err = %v, want the store's own failure", err)
	}
	if !strings.Contains(err.Error(), "variables") {
		t.Errorf("err = %q, want it to say what was being read", err)
	}
}

// A96: compiling a job asks for its variables only when an expression in it
// reads one, and a failure to read them stops the compile.
func TestCompile_VariablesAreReadOnlyByAJobThatReadsOne(t *testing.T) {
	w := parse(t, `
jobs:
  plain:
    runs-on: [linux]
    steps:
      - run: make
  reads:
    runs-on: [linux]
    environment: production
    steps:
      - run: echo ${{ vars.REGION }}
`)
	down := &countingVars{err: errVaultDown}
	if _, err := CompileWith(t.Context(), w, "plain", testScope(), Options{Vars: down.job}); err != nil {
		t.Fatalf("plain: %v", err)
	}
	if down.asked != 0 {
		t.Errorf("a job that reads no variable asked for them %d times", down.asked)
	}
	_, err := CompileWith(t.Context(), w, "reads", testScope(), Options{Vars: down.job})
	if err == nil || !strings.Contains(err.Error(), errVaultDown.Error()) {
		t.Errorf("err = %v, want the compile stopped by the failed read", err)
	}

	up := &countingVars{vars: map[string]string{"REGION": "eu"}}
	job, err := CompileWith(t.Context(), w, "reads", testScope(), Options{Vars: up.job})
	if err != nil {
		t.Fatal(err)
	}
	if job.Steps[0].Env["GHA_EXPR_1"] != "eu" {
		t.Errorf("vars.REGION = %q", job.Steps[0].Env["GHA_EXPR_1"])
	}
}
