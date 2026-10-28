package workflow

import (
	"strings"
	"testing"
	"time"
)

// A50: defaults.run.working-directory reaches the workflow's own run steps,
// a step's own setting wins, and an action's steps are left alone.
func TestDefaultWorkingDirectory(t *testing.T) {
	job, err := compileWith(t, `
defaults:
  run:
    working-directory: packages/web
jobs:
  b:
    runs-on: [linux]
    defaults:
      run:
        working-directory: packages/mobile
    steps:
      - run: make
      - run: make
        working-directory: tools
      - uses: ./.github/actions/greet
        with:
          who: world
`, mapLoader{".github/actions/greet": greet})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasPrefix(job.Steps[0].Run, "cd 'packages/mobile'\n") {
		t.Errorf("the job's default should win over the workflow's: %q", job.Steps[0].Run)
	}
	if !strings.HasPrefix(job.Steps[1].Run, "cd 'tools'\n") {
		t.Errorf("the step's own working-directory should win: %q", job.Steps[1].Run)
	}
	for _, st := range job.Steps[2:] {
		if strings.HasPrefix(st.Run, "cd ") {
			t.Errorf("a job default reached into an action's step: %q", st.Run)
		}
	}
}

// A50: ${{ env.X }} reads the workflow's and the job's env, the step's own
// over them, and a value only known when the step runs is not guessed at.
func TestEnvContext(t *testing.T) {
	w := parse(t, `
env:
  STAGE: staging
  WHERE: workflow
jobs:
  b:
    runs-on: [linux]
    env:
      WHERE: job
      SHA: ${{ github.sha }}
    steps:
      - id: first
        run: echo first
      - run: echo "${{ env.STAGE }} ${{ env.WHERE }} ${{ env.SHA }}"
      - run: echo "${{ env.WHERE }}"
        env:
          WHERE: step
      - run: echo "[${{ env.LATER }}]"
        env:
          LATER: ${{ steps.first.outputs.x }}
`)
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	s := job.Steps[1]
	if got := []string{s.Env["GHA_EXPR_1"], s.Env["GHA_EXPR_2"], s.Env["GHA_EXPR_3"]}; strings.Join(got, "|") != "staging|job|deadbeef" {
		t.Errorf("env context = %q, want staging|job|deadbeef", got)
	}
	if got := job.Steps[2].Env["GHA_EXPR_1"]; got != "step" {
		t.Errorf("a step's own env should win in its expressions: %q", got)
	}
	if got := job.Steps[3].Env["GHA_EXPR_1"]; got != "" {
		t.Errorf("a late env value should not be guessed at compile time: %q", got)
	}
	if _, late := job.Steps[3].Late["LATER"]; !late {
		t.Error("the late value itself should still reach the step")
	}
}

// A50: GitHub's 360 minutes unless the job says otherwise.
func TestWorkflowJobTimeout(t *testing.T) {
	w := parse(t, `
jobs:
  plain:
    runs-on: [linux]
    steps:
      - run: make
  short:
    runs-on: [linux]
    timeout-minutes: 15
    steps:
      - run: make
`)
	for job, want := range map[string]time.Duration{"plain": 360 * time.Minute, "short": 15 * time.Minute} {
		j, err := Compile(t.Context(), w, job, testScope())
		if err != nil {
			t.Fatalf("Compile %s: %v", job, err)
		}
		if got := j.Timeout.Duration(); got != want {
			t.Errorf("%s: timeout = %s, want %s", job, got, want)
		}
	}
}

// A50: a step's continue-on-error is a boolean, its string spelling, or an
// expression answered now; one only answerable when the step runs is refused.
func TestStepContinueOnError(t *testing.T) {
	w := parse(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - run: a
        continue-on-error: true
      - run: b
        continue-on-error: "false"
      - run: c
        continue-on-error: ${{ github.event_name == 'push' }}
      - run: d
`)
	job, err := Compile(t.Context(), w, "b", testScope())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []bool{true, false, true, false}
	for i, st := range job.Steps {
		if st.ContinueOnError != want[i] {
			t.Errorf("step %d: continue-on-error = %v, want %v", i+1, st.ContinueOnError, want[i])
		}
	}

	w = parse(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - id: x
        run: a
      - run: b
        continue-on-error: ${{ steps.x.outputs.flaky }}
`)
	if _, err := Compile(t.Context(), w, "b", testScope()); err == nil ||
		!strings.Contains(err.Error(), "only known when the step runs") {
		t.Errorf("a late continue-on-error should be refused, got %v", err)
	}

	w = parse(t, `
jobs:
  b:
    runs-on: [linux]
    continue-on-error: true
    steps:
      - run: a
`)
	if _, err := Compile(t.Context(), w, "b", testScope()); err == nil ||
		!strings.Contains(err.Error(), "continue-on-error: on a job") {
		t.Errorf("a job's continue-on-error is still refused, got %v", err)
	}
}

// A50: runs-on goes through the machine's aliases, then GitHub's hosted
// labels; a Windows label is refused by name.
func TestRunsOnAliases(t *testing.T) {
	aliases := map[string][]string{
		"blacksmith-4vcpu-ubuntu-2404": {"linux"},
		"macos-latest":                 {"macos", "xcode"}, // overrides the built-in
	}
	cases := map[string]string{
		"ubuntu-latest":                          "linux",
		"ubuntu-24.04":                           "linux",
		"ubuntu-24.04-arm":                       "linux,arm64",
		"macos-15":                               "macos",
		"macos-14-xlarge":                        "macos",
		"macos-latest":                           "macos,xcode",
		"blacksmith-4vcpu-ubuntu-2404":           "linux",
		"[self-hosted, ubuntu-latest, linux]":    "linux",
		"[blacksmith-4vcpu-ubuntu-2404, docker]": "linux,docker",
		"[macos, arm64]":                         "macos,arm64",
	}
	for runsOn, want := range cases {
		w := parse(t, "jobs:\n  b:\n    runs-on: "+runsOn+"\n    steps:\n      - run: make\n")
		job, err := CompileWith(t.Context(), w, "b", testScope(), Options{LabelAliases: aliases})
		if err != nil {
			t.Errorf("%s: %v", runsOn, err)
			continue
		}
		if got := strings.Join(job.RunsOn, ","); got != want {
			t.Errorf("runs-on %s compiled to %q, want %q", runsOn, got, want)
		}
	}

	w := parse(t, "jobs:\n  b:\n    runs-on: windows-latest\n    steps:\n      - run: make\n")
	if _, err := Compile(t.Context(), w, "b", testScope()); err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Errorf("windows-latest should be refused by name, got %v", err)
	}
}

// A50: vars come from the machine, the job's environment over the
// repository's, and environment: is read in both of its forms.
func TestVarsAndEnvironment(t *testing.T) {
	vars := func(env string) map[string]string {
		out := map[string]string{"ACCOUNT": "acct", "READY": "true"}
		if env == "production" {
			out["READY"] = "false"
		}
		return out
	}
	w := parse(t, `
jobs:
  plain:
    runs-on: [linux]
    steps:
      - if: vars.READY == 'true'
        run: echo ${{ vars.ACCOUNT }}
  prod:
    runs-on: [linux]
    environment:
      name: production
      url: https://example.com
    steps:
      - if: vars.READY == 'true'
        run: never
      - run: echo ${{ vars.ACCOUNT }}
  dynamic:
    runs-on: [linux]
    environment: ${{ github.event_name == 'push' && 'production' || 'staging' }}
    steps:
      - run: echo ${{ vars.READY }}
`)
	opts := Options{Vars: vars}

	plain, err := CompileWith(t.Context(), w, "plain", testScope(), opts)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if len(plain.Steps) != 1 || plain.Steps[0].Env["GHA_EXPR_1"] != "acct" || plain.Environment != "" {
		t.Errorf("plain = %+v", plain)
	}

	prod, err := CompileWith(t.Context(), w, "prod", testScope(), opts)
	if err != nil {
		t.Fatalf("prod: %v", err)
	}
	if prod.Environment != "production" {
		t.Errorf("environment = %q, want production", prod.Environment)
	}
	if len(prod.Steps) != 1 {
		t.Errorf("production's READY is false, so its first step should compile away: %d steps", len(prod.Steps))
	}

	dyn, err := CompileWith(t.Context(), w, "dynamic", testScope(), opts)
	if err != nil {
		t.Fatalf("dynamic: %v", err)
	}
	if dyn.Environment != "production" || dyn.Steps[0].Env["GHA_EXPR_1"] != "false" {
		t.Errorf("dynamic = %q, READY = %q", dyn.Environment, dyn.Steps[0].Env["GHA_EXPR_1"])
	}

	// Without a source, vars are empty, as before: the condition is false and
	// the job has nothing left to run.
	if _, err := Compile(t.Context(), w, "plain", testScope()); err == nil || !strings.Contains(err.Error(), "no steps") {
		t.Errorf("with no vars the only step should compile away, got %v", err)
	}
}
