package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

func testScope() expr.Scope {
	return expr.Scope{
		Contexts: map[string]expr.Value{
			"github": map[string]any{
				"ref":        "refs/heads/main",
				"sha":        "deadbeef",
				"run_number": float64(42),
				"event_name": "push",
				"event": map[string]any{
					"pull_request": map[string]any{"title": "; rm -rf / #"},
				},
			},
			"env":  map[string]any{},
			"vars": map[string]any{},
		},
		Status: expr.StatusSuccess,
	}
}

func parse(t *testing.T, src string) *Workflow {
	t.Helper()
	w, err := Parse([]byte(src), ".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return w
}

const simple = `
name: CI
on: [push]
jobs:
  test:
    runs-on: [self-hosted, macos, arm64]
    steps:
      - name: unit
        run: make test
`

func TestParseAndCompile(t *testing.T) {
	w := parse(t, simple)
	if !w.TriggersAreNotHonoured() {
		t.Error("an `on:` block should be reported as unhonoured")
	}
	job, err := Compile(w, "test", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// self-hosted carries no information on a machine you own and is dropped.
	if got := strings.Join(job.RunsOn, ","); got != "macos,arm64" {
		t.Errorf("runs_on = %q", got)
	}
	if len(job.Steps) != 1 || job.Steps[0].Run != "make test" {
		t.Errorf("steps = %+v", job.Steps)
	}
	// The whole point of compiling to a registry job: it is the same object
	// the native path produces, so it passes the same validation.
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"test": job}}
	if err := reg.Validate(); err != nil {
		t.Fatalf("a compiled job must satisfy registry validation: %v", err)
	}
}

// A42's normative rule. This is the test that has to survive every later
// refactor of the compiler.
func TestExpressionNeverBecomesShellText(t *testing.T) {
	w := parse(t, `
jobs:
  build:
    runs-on: [macos]
    steps:
      - run: echo ${{ github.event.pull_request.title }}
`)
	job, err := Compile(w, "build", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	st := job.Steps[0]

	if strings.Contains(st.Run, "rm -rf") {
		t.Fatalf("the title was substituted into the command: %q", st.Run)
	}
	if !strings.Contains(st.Run, `"$GHA_EXPR_1"`) {
		t.Errorf("run should reference a quoted variable, got %q", st.Run)
	}
	if st.Env["GHA_EXPR_1"] != "; rm -rf / #" {
		t.Errorf("the value should reach the step as data: %q", st.Env["GHA_EXPR_1"])
	}
	// And the compiled step is still a legal registry step — run: carries no
	// ${...} of this system's, so rejectInterpolation is satisfied.
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"build": job}}
	if err := reg.Validate(); err != nil {
		t.Errorf("hoisted step failed registry validation: %v", err)
	}
}

func TestHoistAvoidsCollisionWithAuthorEnv(t *testing.T) {
	w := parse(t, `
jobs:
  build:
    runs-on: [macos]
    env:
      GHA_EXPR_1: mine
    steps:
      - run: echo ${{ github.sha }}
`)
	job, err := Compile(w, "build", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	st := job.Steps[0]
	if st.Env["GHA_EXPR_1"] != "mine" {
		t.Errorf("the author's variable was overwritten: %q", st.Env["GHA_EXPR_1"])
	}
	if st.Env["GHA_EXPR_2"] != "deadbeef" {
		t.Errorf("the hoisted value should have moved to a free name: %+v", st.Env)
	}
}

func TestUsesIsRefusedByName(t *testing.T) {
	w := parse(t, `
jobs:
  build:
    runs-on: [macos]
    steps:
      - uses: actions/checkout@v5
      - run: make
`)
	_, err := Compile(w, "build", testScope())
	if err == nil {
		t.Fatal("a uses: step must be refused in phase 1")
	}
	if !strings.Contains(err.Error(), "actions/checkout@v5") {
		t.Errorf("the error should name the action: %v", err)
	}
	if !strings.Contains(err.Error(), "phase 5") {
		t.Errorf("the error should say when it will work: %v", err)
	}
}

// Every unsupported construct at once, because that is how a real workflow
// arrives.
func TestUnsupportedConstructsAreListedTogether(t *testing.T) {
	w := parse(t, `
jobs:
  build:
    runs-on: [macos]
    needs: [other]
    strategy:
      matrix:
        go: [1.24, 1.25]
    steps:
      - run: make
`)
	_, err := Compile(w, "build", testScope())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"strategy:", "needs:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %s: %v", want, err)
		}
	}
}

func TestRuntimeOnlyExpressionsAreRefused(t *testing.T) {
	cases := map[string]string{
		"step outputs": `run: echo ${{ steps.x.outputs.y }}`,
		"job status":   `run: echo ${{ job.status }}`,
		"runner os":    `run: echo ${{ runner.os }}`,
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			w := parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - "+step+"\n")
			_, err := Compile(w, "b", testScope())
			if err == nil {
				t.Fatal("a run-time context must not be compiled to a value")
			}
			if !strings.Contains(err.Error(), "does not exist until the run is under way") {
				t.Errorf("unhelpful error: %v", err)
			}
		})
	}
}

func TestIfIsEvaluatedAtCompileTime(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - name: kept
        if: github.event_name == 'push'
        run: make a
      - name: dropped
        if: github.event_name == 'schedule'
        run: make b
`)
	job, err := Compile(w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Name != "kept" {
		t.Errorf("steps = %+v, want only the one whose condition held", job.Steps)
	}
}

// `if: always()` cannot compile: a registry step has no condition, so a
// compile-time answer would silently change when the step runs.
func TestStatusConditionsAreRefused(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - if: failure()
        run: ./cleanup.sh
`)
	if _, err := Compile(w, "b", testScope()); err == nil {
		t.Fatal("a status-function condition must be refused, not compiled to false")
	}
}

func TestStrictDecodingRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("jobs:\n  b:\n    runs-on: [macos]\n    stpes: []\n"), "ci.yml")
	if err == nil {
		t.Fatal("a misspelled key must be an error, not a silently empty job")
	}
}

func TestRunsOnForms(t *testing.T) {
	// A bare string is the common form.
	w := parse(t, "jobs:\n  b:\n    runs-on: macos\n    steps:\n      - run: make\n")
	job, err := Compile(w, "b", testScope())
	if err != nil || len(job.RunsOn) != 1 || job.RunsOn[0] != "macos" {
		t.Errorf("scalar runs-on = %+v, %v", job, err)
	}

	// GitHub writes macOS; runner labels are lowercase.
	w2 := parse(t, "jobs:\n  b:\n    runs-on: [self-hosted, macOS]\n    steps:\n      - run: make\n")
	job2, err := Compile(w2, "b", testScope())
	if err != nil || len(job2.RunsOn) != 1 || job2.RunsOn[0] != "macos" {
		t.Errorf("mixed-case runs-on = %+v, %v", job2, err)
	}

	// self-hosted alone says nothing about which machine.
	w3 := parse(t, "jobs:\n  b:\n    runs-on: [self-hosted]\n    steps:\n      - run: make\n")
	if _, err := Compile(w3, "b", testScope()); err == nil {
		t.Error("runs-on: [self-hosted] should be refused as carrying no label")
	}
}

func TestWorkingDirectoryBecomesACd(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - run: make
        working-directory: sub dir
`)
	job, err := Compile(w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasPrefix(job.Steps[0].Run, "cd 'sub dir'\n") {
		t.Errorf("run = %q, want a quoted cd prefix", job.Steps[0].Run)
	}
}

func TestUnknownJobNamesTheOnesThatExist(t *testing.T) {
	w := parse(t, simple)
	_, err := Compile(w, "nope", testScope())
	if err == nil || !strings.Contains(err.Error(), "test") {
		t.Errorf("the error should list the jobs that do exist: %v", err)
	}
}

func TestIsWorkflowPath(t *testing.T) {
	yes := []string{".github/workflows/ci.yml", ".github/workflows/release.yaml"}
	no := []string{".github/ci.yml", ".latem/jobs.yaml", ".github/workflows/README.md", "workflows/ci.yml"}
	for _, p := range yes {
		if !IsWorkflowPath(p) {
			t.Errorf("%s should be a workflow path", p)
		}
	}
	for _, p := range no {
		if IsWorkflowPath(p) {
			t.Errorf("%s should not be a workflow path", p)
		}
	}
}

// The parser is aimed at files nobody wrote for it, so it is pointed at the
// ones in this repository. They are committed, so this is deterministic — and
// they are the workflows A42 measured, which makes them a fair sample of what
// a real adopter brings.
//
// Parsing must succeed. Compiling is expected to refuse, because both of these
// are majority-`uses:` — and the refusal is the product of this phase, so the
// assertion is that it names the action rather than failing vaguely.
func TestAgainstThisRepositorysOwnWorkflows(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("no workflows checked out")
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			w, err := Parse(data, p)
			if err != nil {
				t.Fatalf("a real workflow must parse: %v", err)
			}
			if len(w.JobNames()) == 0 {
				t.Fatal("no jobs found")
			}
			for _, name := range w.JobNames() {
				_, err := Compile(w, name, testScope())
				if err == nil {
					continue // a job with no uses: is allowed to compile
				}
				if !strings.Contains(err.Error(), "latem does not implement yet") {
					t.Errorf("job %q refused for an unexpected reason: %v", name, err)
				}
			}
		})
	}
}

// INV-4: a secret is declared and arrives by name. Substituting one would put
// the value in the compiled registry, which is persisted and logged.
func TestSecretsAreNeverSubstituted(t *testing.T) {
	sc := testScope()
	sc.Contexts["secrets"] = map[string]any{"NPM_TOKEN": "s3cret"}

	for _, src := range []string{
		"jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - run: npm publish --token ${{ secrets.NPM_TOKEN }}\n",
		"jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - if: secrets.NPM_TOKEN != ''\n        run: make\n",
	} {
		w := parse(t, src)
		_, err := Compile(w, "b", sc)
		if err == nil {
			t.Fatalf("a secrets expression must be refused:\n%s", src)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("the refusal leaked the value: %v", err)
		}
		if !strings.Contains(err.Error(), "secrets:") {
			t.Errorf("the error should point at the declaration: %v", err)
		}
	}
}
