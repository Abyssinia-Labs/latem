package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
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
	job, err := Compile(t.Context(), w, "test", testScope())
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
	job, err := Compile(t.Context(), w, "build", testScope())
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
	job, err := Compile(t.Context(), w, "build", testScope())
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

// A remote action meets two gates, and they are separate because their fixes
// are: the allowlist is a line in config.toml, and "not implemented yet" is a
// workflow edit until phase 5 finishes.
func TestUsesIsRefusedByName(t *testing.T) {
	src := `
jobs:
  build:
    runs-on: [macos]
    steps:
      - uses: actions/setup-go@v6
      - run: make
`
	// The shipped default refuses everything, and says which file to edit.
	_, err := Compile(t.Context(), parse(t, src), "build", testScope())
	if err == nil {
		t.Fatal("the default policy admitted a remote action")
	}
	if !strings.Contains(err.Error(), "actions/setup-go") || !strings.Contains(err.Error(), "allowed") {
		t.Errorf("the refusal should name the action and the allowlist: %v", err)
	}

	// Allowed, and still not implemented — a different sentence.
	_, err = CompileWith(t.Context(), parse(t, src), "build", testScope(),
		Options{Policy: actions.Policy{Allowed: []string{"actions/*"}}})
	if err == nil {
		t.Fatal("an allowed remote action compiled")
	}
	if !strings.Contains(err.Error(), "actions/setup-go@v6") {
		t.Errorf("the error should name the action: %v", err)
	}
	if !strings.Contains(err.Error(), "no action cache") {
		t.Errorf("with no fetcher the error should say so: %v", err)
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
    services:
      db: {image: postgres}
    steps:
      - run: make
`)
	_, err := Compile(t.Context(), w, "build", testScope())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"services:", "needs:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %s: %v", want, err)
		}
	}
}

// steps.* is late, and the registry's ordering check is what makes that safe:
// a reference to a step that is not earlier is refused at admission rather
// than resolving to an empty string that looks like the step produced nothing.
func TestStepsContextIsLateAndOrdered(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - id: build
        run: make
      - run: echo ${{ steps.build.outputs.sha }}
`)
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	late := job.Steps[1].Late["GHA_EXPR_1"]
	if late == nil {
		t.Fatalf("steps.* should be a late binding: %+v", job.Steps[1])
	}
	got, err := late.Resolve(registry.StepScope{Outputs: map[string]map[string]string{"build": {"sha": "abc"}}})
	if err != nil || got != "abc" {
		t.Errorf("resolved %q, %v", got, err)
	}
	// Absent is the empty string — GitHub's answer, and what
	// `if: steps.x.outputs.y != ''` depends on.
	got, _ = late.Resolve(registry.StepScope{})
	if got != "" {
		t.Errorf("an absent output should be empty, got %q", got)
	}
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job}}
	if err := reg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}

	w2 := parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - run: echo ${{ steps.later.outputs.x }}\n      - id: later\n        run: make\n")
	job2, err := Compile(t.Context(), w2, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	reg2 := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job2}}
	if err := reg2.Validate(); err == nil || !strings.Contains(err.Error(), "not an earlier step") {
		t.Errorf("a forward reference must be refused at admission: %v", err)
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
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 1 || job.Steps[0].Name != "kept" {
		t.Errorf("steps = %+v, want only the one whose condition held", job.Steps)
	}
}

// `if: failure()` is late: the registry step carries the condition and the run
// loop answers it against the job's status at that moment.
func TestStatusConditionsBecomeLate(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - run: make
      - if: failure()
        run: ./cleanup.sh
      - if: ${{ always() }}
        run: ./report.sh
`)
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(job.Steps) != 3 {
		t.Fatalf("steps = %d, want all three carried", len(job.Steps))
	}
	if job.Steps[0].If != nil {
		t.Error("an unconditioned step should have no If")
	}
	cleanup, ok := job.Steps[1].If.(*lateExpr)
	if !ok {
		t.Fatalf("If = %T, want a late expression", job.Steps[1].If)
	}
	if run, _ := cleanup.Holds(registry.StepScope{Status: registry.StatusSuccess}); run {
		t.Error("failure() should be false while the job is succeeding")
	}
	if run, _ := cleanup.Holds(registry.StepScope{Status: registry.StatusFailure}); !run {
		t.Error("failure() should be true once a step has failed")
	}
	report := job.Steps[2].If.(*lateExpr)
	if run, _ := report.Holds(registry.StepScope{Status: registry.StatusCancelled}); !run {
		t.Error("always() should be true even when cancelled")
	}
	// And it is still a registry job that validates.
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job}}
	if err := reg.Validate(); err != nil {
		t.Errorf("a job with late conditions failed validation: %v", err)
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
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil || len(job.RunsOn) != 1 || job.RunsOn[0] != "macos" {
		t.Errorf("scalar runs-on = %+v, %v", job, err)
	}

	// GitHub writes macOS; runner labels are lowercase.
	w2 := parse(t, "jobs:\n  b:\n    runs-on: [self-hosted, macOS]\n    steps:\n      - run: make\n")
	job2, err := Compile(t.Context(), w2, "b", testScope())
	if err != nil || len(job2.RunsOn) != 1 || job2.RunsOn[0] != "macos" {
		t.Errorf("mixed-case runs-on = %+v, %v", job2, err)
	}

	// self-hosted alone says nothing about which machine.
	w3 := parse(t, "jobs:\n  b:\n    runs-on: [self-hosted]\n    steps:\n      - run: make\n")
	if _, err := Compile(t.Context(), w3, "b", testScope()); err == nil {
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
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasPrefix(job.Steps[0].Run, "cd 'sub dir'\n") {
		t.Errorf("run = %q, want a quoted cd prefix", job.Steps[0].Run)
	}
}

// A50: every compiled step carries GitHub's shell. No shell is GitHub's
// default, `bash -e` — which is not the same as naming bash.
func TestStepsCarryGitHubsShell(t *testing.T) {
	w := parse(t, `
jobs:
  plain:
    runs-on: [linux]
    steps:
      - run: make
  defaulted:
    runs-on: [linux]
    defaults:
      run:
        shell: bash
    steps:
      - run: make
      - run: make
        shell: sh
`)
	job, err := Compile(t.Context(), w, "plain", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := job.Steps[0].Shell; got != string(executor.ShellGitHub) {
		t.Errorf("no shell: got %q, want %q", got, executor.ShellGitHub)
	}

	job, err = Compile(t.Context(), w, "defaulted", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := job.Steps[0].Shell; got != string(executor.ShellGitHubBash) {
		t.Errorf("job default bash: got %q", got)
	}
	if got := job.Steps[1].Shell; got != string(executor.ShellGitHubSh) {
		t.Errorf("the step's own shell should win: got %q", got)
	}

	w = parse(t, `
defaults:
  run:
    shell: sh
jobs:
  plain:
    runs-on: [linux]
    steps:
      - run: make
`)
	job, err = Compile(t.Context(), w, "plain", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := job.Steps[0].Shell; got != string(executor.ShellGitHubSh) {
		t.Errorf("workflow default sh: got %q", got)
	}
}

func TestUnknownJobNamesTheOnesThatExist(t *testing.T) {
	w := parse(t, simple)
	_, err := Compile(t.Context(), w, "nope", testScope())
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
			// Allowed by policy, so what is left to refuse them is the
			// implementation — which is the fact A42 measured these files for.
			opts := Options{Policy: actions.Policy{Allowed: []string{"*/*"}}}
			for _, name := range w.JobNames() {
				// A matrix job is compiled a leg at a time, as a run builds
				// it (A55); every leg is tried.
				legs := []json.RawMessage{nil}
				if ms, err := MatrixLegs(w, name, testScope()); err != nil {
					t.Errorf("job %q: the matrix does not expand: %v", name, err)
				} else if ms != nil {
					legs = ms
				}
				for _, leg := range legs {
					opts.Matrix = leg
					_, err := CompileWith(t.Context(), w, name, testScope(), opts)
					if err == nil {
						continue // a job with no uses: is allowed to compile
					}
					// Three honest refusals remain with no cache wired: an
					// action that would have to be fetched; this repository's
					// own release.yml asking checkout for a different ref —
					// which is `latem run --ref` here, and the shim says so; and
					// its jobs that download what an earlier job built (A77).
					msg := err.Error()
					if !strings.Contains(msg, "no action cache") &&
						!strings.Contains(msg, "latem has already checked this repository out") &&
						!strings.Contains(msg, "latem does not implement yet") &&
						!strings.Contains(msg, "artifacts across jobs are not implemented") {
						t.Errorf("job %q (leg %s) refused for an unexpected reason: %v", name, leg, err)
					}
				}
			}
		})
	}
}

// INV-4: a secret is declared and arrives by name. Substituting one would put
// the value in the compiled registry, which is persisted and logged.
func TestSecretsAreNeverSubstituted(t *testing.T) {
	// Even with a value in the admission scope — which a run never puts there
	// — a secret compiles to a late binding to its name (A50): the registry,
	// which is persisted, never holds a value.
	sc := testScope()
	sc.Contexts["secrets"] = map[string]any{"NPM_TOKEN": "s3cret"}

	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - run: npm publish --token ${{ secrets.NPM_TOKEN }}
      - if: secrets.DEPLOY_KEY != ''
        run: make
        env:
          AUTH: Bearer ${{ secrets['API_TOKEN'] }}
`)
	job, err := Compile(t.Context(), w, "b", sc)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := strings.Join(job.Secrets, ","); got != "API_TOKEN,DEPLOY_KEY,NPM_TOKEN" {
		t.Errorf("job secrets = %q, want every name the job reads", got)
	}
	publish := job.Steps[0]
	if publish.Run != `npm publish --token "$GHA_EXPR_1"` {
		t.Errorf("run = %q — a secret must reach the step as an environment variable", publish.Run)
	}
	if _, late := publish.Late["GHA_EXPR_1"]; !late {
		t.Error("the secret should be bound late, by name")
	}
	for _, st := range job.Steps {
		for k, v := range st.Env {
			if strings.Contains(v, "s3cret") {
				t.Errorf("a secret value reached the compiled registry in %s", k)
			}
		}
	}
	if job.Steps[1].If == nil {
		t.Error("a condition reading a secret should be evaluated when the step runs")
	}

	// A name chosen at run time could be any secret on the machine.
	w = parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - run: echo ${{ secrets[env.WHICH] }}\n")
	if _, err := Compile(t.Context(), w, "b", sc); err == nil || !strings.Contains(err.Error(), "has to be named") {
		t.Errorf("a computed secret name should be refused, got %v", err)
	}
}

// A late value in run: changes an env entry, never the command. The command is
// identical to the admission-time case; only where the variable's value comes
// from differs.
func TestLateExpressionInRunIsHoistedNotSubstituted(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - run: echo status=${{ job.status }} on ${{ runner.os }}
`)
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	st := job.Steps[0]
	if st.Run != `echo status="$GHA_EXPR_1" on "$GHA_EXPR_2"` {
		t.Errorf("run = %q", st.Run)
	}
	if _, static := st.Env["GHA_EXPR_1"]; static {
		t.Error("a late value must not be pre-filled into the static env")
	}
	sc := registry.StepScope{Status: registry.StatusFailure, Runner: registry.RunnerFacts{OS: "macos", Arch: "arm64"}}
	got, err := st.Late["GHA_EXPR_1"].Resolve(sc)
	if err != nil || got != "failure" {
		t.Errorf("job.status resolved to %q, %v", got, err)
	}
	got, err = st.Late["GHA_EXPR_2"].Resolve(sc)
	if err != nil || got != "macOS" {
		t.Errorf("runner.os resolved to %q, %v (GitHub's spelling is macOS)", got, err)
	}
}

func TestLateEnvValue(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [macos]
    env:
      STATUS: ${{ job.status }}
      MIXED: prefix-${{ job.status }}
    steps:
      - run: make
`)
	// Text around a late expression is rendered whole when the step runs
	// (A50); it is still only ever an environment value.
	mixed, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	v := mixed.Steps[0].Late["MIXED"]
	if v == nil {
		t.Fatalf("MIXED should be a late binding: %+v", mixed.Steps[0])
	}
	if got, err := v.Resolve(registry.StepScope{Status: registry.StatusFailure}); err != nil || got != "prefix-failure" {
		t.Errorf("MIXED = %q, %v; want prefix-failure", got, err)
	}

	w2 := parse(t, `
jobs:
  b:
    runs-on: [macos]
    env:
      STATUS: ${{ job.status }}
    steps:
      - run: make
`)
	job, err := Compile(t.Context(), w2, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if job.Steps[0].Late["STATUS"] == nil {
		t.Fatalf("STATUS should be a late binding: %+v", job.Steps[0])
	}
	if _, static := job.Steps[0].Env["STATUS"]; static {
		t.Error("STATUS must not also be static")
	}
}

func TestStepIDIsCarried(t *testing.T) {
	w := parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - id: build\n        run: make\n")
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatal(err)
	}
	if job.Steps[0].ID != "build" {
		t.Errorf("id = %q", job.Steps[0].ID)
	}
}

// A condition is one expression. Text around it would coerce to a non-empty
// string, which is always truthy — a condition that silently always passes.
func TestConditionMustBeASingleExpression(t *testing.T) {
	w := parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - if: run when ${{ always() }}\n        run: make\n")
	if _, err := Compile(t.Context(), w, "b", testScope()); err == nil {
		t.Error("text around a condition's expression must be refused")
	}
}
