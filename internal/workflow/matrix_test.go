package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// legsOf expands a one-job workflow's matrix and returns each leg's values.
func legsOf(t *testing.T, matrix string) []map[string]any {
	t.Helper()
	w := parse(t, "jobs:\n  b:\n    runs-on: [linux]\n    strategy:\n      matrix:\n"+matrix+"    steps: [{run: x}]\n")
	st, err := expandStrategy(w.Jobs["b"], testScope())
	if err != nil {
		t.Fatalf("expandStrategy: %v", err)
	}
	var out []map[string]any
	for _, l := range st.legs {
		out = append(out, l.values)
	}
	return out
}

// GitHub's own worked example of include, from its documentation, leg for
// leg: an entry extends every combination whose original values it matches,
// may overwrite what an earlier entry added, never an original value, and is
// a combination of its own when it matches none — and does not match one an
// earlier include made.
func TestMatrixIncludeAsGitHubDocumentsIt(t *testing.T) {
	got := legsOf(t, `        fruit: [apple, pear]
        animal: [cat, dog]
        include:
          - color: green
          - color: pink
            animal: cat
          - fruit: apple
            shape: circle
          - fruit: banana
          - fruit: banana
            animal: cat
`)
	want := []map[string]any{
		{"fruit": "apple", "animal": "cat", "color": "pink", "shape": "circle"},
		{"fruit": "apple", "animal": "dog", "color": "green", "shape": "circle"},
		{"fruit": "pear", "animal": "cat", "color": "pink"},
		{"fruit": "pear", "animal": "dog", "color": "green"},
		{"fruit": "banana"},
		{"fruit": "banana", "animal": "cat"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legs:\n got %v\nwant %v", got, want)
	}
}

// exclude removes every combination matching all of an entry's keys, before
// include sees them; a key the matrix does not have is the file's mistake.
func TestMatrixExclude(t *testing.T) {
	got := legsOf(t, `        os: [linux, macos]
        node: [20, 22]
        exclude:
          - os: macos
            node: 20
        include:
          - os: macos
            experimental: true
`)
	want := []map[string]any{
		{"os": "linux", "node": float64(20)},
		{"os": "linux", "node": float64(22)},
		{"os": "macos", "node": float64(22), "experimental": true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legs:\n got %v\nwant %v", got, want)
	}
}

func TestMatrixRefusals(t *testing.T) {
	for name, c := range map[string]struct{ matrix, want string }{
		"an empty dimension":  {"        os: []\n", "os has no values"},
		"everything excluded": {"        os: [a]\n        exclude: [{os: a}]\n", "no combinations"},
		"an unknown exclude":  {"        os: [a, b]\n        exclude: [{arch: x}]\n", `"arch" is not one of the matrix's keys`},
		"not a list":          {"        os: linux\n", "expected a list of values"},
		"a matrix from needs": {"        os: ${{ fromJSON(needs.a.outputs.os) }}\n", "a matrix computed by another job is not planned yet"},
		"too many legs": {"        a: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17]\n        b: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]\n",
			"more than 256"},
	} {
		t.Run(name, func(t *testing.T) {
			w := parse(t, "jobs:\n  b:\n    runs-on: [linux]\n    strategy:\n      matrix:\n"+c.matrix+"    steps: [{run: x}]\n")
			_, err := expandStrategy(w.Jobs["b"], testScope())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// A leg's name and its position follow the file's key order, not JSON's
// sorted one: branch protection requires checks by these names.
func TestPlanMatrixLegs(t *testing.T) {
	files := map[string]string{"ci.yml": `
on: push
jobs:
  test:
    strategy:
      fail-fast: false
      max-parallel: 2
      matrix:
        os: [ubuntu-latest, macos-latest]
        node: [20, 22]
    runs-on: ${{ matrix.os }}
    environment: ${{ matrix.os == 'ubuntu-latest' && 'linux-env' || '' }}
    steps: [{run: x}]
  named:
    name: Build on ${{ matrix.os }}
    strategy: {matrix: {os: [ubuntu-latest]}}
    runs-on: ${{ matrix.os }}
    steps: [{run: x}]
  after:
    needs: [test]
    runs-on: ubuntu-latest
    steps: [{run: x}]
`}
	plan := mustPlan(t, files, protocol.Event{Name: EventPush, Ref: "refs/heads/main"}, PlanOptions{})
	var names []string
	var tests []protocol.PlannedJob
	for _, j := range plan[0].Jobs {
		if j.Error != "" {
			t.Fatalf("%s: %s", j.Name, j.Error)
		}
		names = append(names, j.Name)
		if j.Key == "test" {
			tests = append(tests, j)
		}
	}
	want := []string{"Build on ubuntu-latest", "test (ubuntu-latest, 20)", "test (ubuntu-latest, 22)",
		"test (macos-latest, 20)", "test (macos-latest, 22)", "after"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	first, last := tests[0], tests[3]
	if !slices.Equal(first.Labels, []string{"linux"}) || !slices.Equal(last.Labels, []string{"macos"}) {
		t.Errorf("labels = %v and %v; runs-on reads the leg", first.Labels, last.Labels)
	}
	if first.Environment != "linux-env" || last.Environment != "" {
		t.Errorf("environments = %q and %q", first.Environment, last.Environment)
	}
	if string(last.Matrix) != `{"node":22,"os":"macos-latest"}` {
		t.Errorf("matrix = %s", last.Matrix)
	}
	if s := last.Strategy; s == nil || *s != (protocol.Strategy{JobIndex: 3, JobTotal: 4, FailFast: false, MaxParallel: 2}) {
		t.Errorf("strategy = %+v", s)
	}
}

// latem's own check job, which is what this amendment unblocks: one job on
// both platforms through runs-on: ${{ matrix.os }}, named as GitHub names it.
func TestPlanThisRepositorysCheckMatrix(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Skip("no workflows checked out")
	}
	// GitHub-hosted labels, which latem reads as linux and macos with no alias.
	plan := mustPlan(t, map[string]string{"ci.yml": string(data)},
		protocol.Event{Name: EventPush, Ref: "refs/heads/main"}, PlanOptions{})
	var checks []protocol.PlannedJob
	for _, j := range plan[0].Jobs {
		if j.Key == "check" {
			checks = append(checks, j)
		}
	}
	if len(checks) != 2 {
		t.Fatalf("check planned as %d jobs, want one per platform", len(checks))
	}
	for i, want := range []struct{ name, label string }{
		{"check (ubuntu-24.04)", "linux"}, {"check (macos-15)", "macos"},
	} {
		if c := checks[i]; c.Error != "" || c.Name != want.name || !slices.Equal(c.Labels, []string{want.label}) ||
			c.Strategy == nil || c.Strategy.FailFast {
			t.Errorf("leg %d = %+v, want %s on %s without fail-fast", i, c, want.name, want.label)
		}
	}
}

// The machine compiles the leg the run names, and only a leg its own
// expansion of the file yields: the control plane's values are checked, not
// believed.
func TestCompileMatrixLeg(t *testing.T) {
	w := parse(t, `
jobs:
  test:
    strategy:
      matrix:
        os: [linux, macos]
        node: [20, 22]
    runs-on: ${{ matrix.os }}
    steps:
      - run: make
        env:
          NODE: ${{ matrix.node }}
          INDEX: ${{ strategy.job-index }} of ${{ strategy.job-total }}
  plain:
    runs-on: [linux]
    steps: [{run: make}]
`)
	compile := func(job, leg string) error {
		t.Helper()
		_, err := CompileWith(t.Context(), w, job, testScope(), Options{Matrix: json.RawMessage(leg)})
		return err
	}
	job, err := CompileWith(t.Context(), w, "test", testScope(), Options{Matrix: json.RawMessage(`{"os":"macos","node":22}`)})
	if err != nil {
		t.Fatalf("a declared leg: %v", err)
	}
	if !slices.Equal(job.RunsOn, []string{"macos"}) {
		t.Errorf("runs_on = %v, want the leg's", job.RunsOn)
	}
	if env := job.Steps[0].Env; env["NODE"] != "22" || env["INDEX"] != "3 of 4" {
		t.Errorf("env = %v", env)
	}

	for name, c := range map[string]struct{ job, leg, want string }{
		"a leg the file does not declare": {"test", `{"os":"windows","node":22}`, "is not one of the 4 legs"},
		"a leg with an extra value":       {"test", `{"os":"macos","node":22,"label":"gpu"}`, "is not one of the 4 legs"},
		"a string for a number":           {"test", `{"os":"macos","node":"22"}`, "is not one of the 4 legs"},
		"no leg at all":                   {"test", ``, "is a matrix of 4 jobs"},
		"a leg for a job with no matrix":  {"plain", `{"os":"linux"}`, "has no matrix"},
	} {
		if err := compile(c.job, c.leg); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to say %q", name, err, c.want)
		}
	}
}
