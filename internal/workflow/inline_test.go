package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
)

// mapLoader serves actions from memory, keyed by directory.
type mapLoader map[string]string

func (m mapLoader) Load(dir string) (*Action, error) {
	clean := strings.TrimPrefix(dir, "./")
	body, ok := m[clean]
	if !ok {
		return nil, fmt.Errorf("no action at %s", dir)
	}
	return ParseAction([]byte(body), clean)
}

// compileWith compiles against a set of local actions, with every remote
// action allowed — the policy has tests of its own, and these are about
// inlining.
func compileWith(t *testing.T, wf string, acts mapLoader) (*registry.Job, error) {
	t.Helper()
	opts := Options{Policy: actions.Policy{Allowed: []string{"*/*"}}}
	if acts != nil {
		// An untyped nil would be a loader that exists and knows no actions,
		// which is a different test.
		opts.Actions = acts
	}
	return CompileWith(t.Context(), parse(t, wf), "b", testScope(), opts)
}

func mustValidate(t *testing.T, job *registry.Job) {
	t.Helper()
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job}}
	if err := reg.Validate(); err != nil {
		t.Fatalf("the compiled job must be an ordinary valid registry job: %v", err)
	}
}

const greet = `
name: greet
inputs:
  who:
    required: true
  greeting:
    default: hello
outputs:
  line:
    value: ${{ steps.say.outputs.line }}
runs:
  using: composite
  steps:
    - id: say
      shell: bash
      run: echo "${{ inputs.greeting }}, ${{ inputs.who }}" from ${{ github.action_path }}
    - name: twice
      shell: bash
      run: echo again
`

func TestCompositeIsInlined(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: ./.github/actions/greet
        id: hi
        name: Greet
        with:
          who: world
`, mapLoader{".github/actions/greet": greet})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	if len(job.Steps) != 2 {
		t.Fatalf("steps = %d, want the action's two", len(job.Steps))
	}
	say := job.Steps[0]
	if say.ID != "hi/say" {
		t.Errorf("inner ids are namespaced under the outer step's: %q", say.ID)
	}
	if say.Name != "Greet › step 1" || job.Steps[1].Name != "Greet › twice" {
		t.Errorf("names = %q, %q", say.Name, job.Steps[1].Name)
	}
	// Inputs are values like any other: hoisted, never command text.
	if strings.Contains(say.Run, "world") || strings.Contains(say.Run, "hello") {
		t.Errorf("an input was written into the command: %q", say.Run)
	}
	got := []string{say.Env["GHA_EXPR_1"], say.Env["GHA_EXPR_2"], say.Env["GHA_EXPR_3"]}
	if strings.Join(got, "|") != "hello|world|.github/actions/greet" {
		t.Errorf("default, with-value and action_path = %q", got)
	}
}

// The injection case, through an action. A PR title handed to an input is
// still data when the action's step runs.
func TestCompositeInputNeverBecomesShellText(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: ./a
        with:
          who: ${{ github.event.pull_request.title }}
`, mapLoader{"a": greet})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(job.Steps[0].Run, "rm -rf") {
		t.Fatalf("the title reached the command through an input: %q", job.Steps[0].Run)
	}
	if job.Steps[0].Env["GHA_EXPR_2"] != "; rm -rf / #" {
		t.Errorf("env = %+v", job.Steps[0].Env)
	}
}

// Because the tree is substituted, a late `with:` makes the action's own
// reference late — and the caller's step id is not moved into the action's
// namespace on the way.
func TestLateWithValueAndOutputs(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - id: prev
        run: make
      - uses: ./a
        id: act
        with:
          who: ${{ steps.prev.outputs.name }}
      - run: echo ${{ steps.act.outputs.line }}
`, mapLoader{"a": greet})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)

	say := job.Steps[1]
	late := say.Late["GHA_EXPR_2"]
	if late == nil {
		t.Fatalf("with: a late value should make the input late: %+v", say)
	}
	if refs := late.References(); len(refs) != 1 || refs[0] != "prev" {
		t.Errorf("the caller's step must stay the caller's: %v", refs)
	}

	// The action's output is its inner step's real output, by substitution.
	use := job.Steps[3].Late["GHA_EXPR_1"]
	if use == nil {
		t.Fatalf("steps.act.outputs.line should be late: %+v", job.Steps[3])
	}
	if refs := use.References(); len(refs) != 1 || refs[0] != "act/say" {
		t.Errorf("refs = %v, want the namespaced inner step", refs)
	}
	got, err := use.Resolve(registry.StepScope{Outputs: map[string]map[string]string{"act/say": {"line": "hello, x"}}})
	if err != nil || got != "hello, x" {
		t.Errorf("resolved %q, %v", got, err)
	}
}

func TestOuterConditionGovernsEveryInlinedStep(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - run: make
      - uses: ./a
        if: failure()
        with: {who: x}
`, mapLoader{"a": greet})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range job.Steps[1:] {
		if st.If == nil {
			t.Fatalf("%q lost the uses: step's condition", st.Name)
		}
		if ok, _ := st.If.Holds(registry.StepScope{Status: registry.StatusSuccess}); ok {
			t.Errorf("%q should not run on a green job", st.Name)
		}
		if ok, _ := st.If.Holds(registry.StepScope{Status: registry.StatusFailure}); !ok {
			t.Errorf("%q should run after a failure", st.Name)
		}
	}
}

func TestCompositeRefusals(t *testing.T) {
	const wf = "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: ./a\n        id: act\n%s      - run: echo ${{ steps.act.outputs.%s }}\n"
	cases := map[string]struct {
		with, output, action, want string
	}{
		"a required input is missing": {"", "line", greet, `requires the input "who"`},
		"an undeclared output":        {"        with: {who: x}\n", "nope", greet, `declares no output "nope"`},
		"an undeclared input": {"", "line",
			"runs:\n  using: composite\n  steps:\n    - shell: bash\n      run: echo ${{ inputs.ghost }}\n", `declares no input "ghost"`},
		"a run step without a shell": {"", "line",
			"runs:\n  using: composite\n  steps:\n    - run: echo hi\n", "must name its shell"},
		"a javascript action with no main": {"", "line", "runs:\n  using: node20\n", "needs runs.main"},
		"a javascript action outside itself": {"", "line",
			"runs:\n  using: node20\n  main: ../../steal.js\n", "must be a path inside the action"},
		"an action that uses itself": {"", "line",
			"runs:\n  using: composite\n  steps:\n    - uses: ./a\n", "uses itself"},
		"a dockerfile image": {"", "line", "runs:\n  using: docker\n  image: Dockerfile\n", "would have to be built"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileWith(t, fmt.Sprintf(wf, c.with, c.output), mapLoader{"a": c.action})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error containing %q, got %v", c.want, err)
			}
		})
	}

	// No loader: said plainly, not a nil dereference.
	if _, err := compileWith(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: ./a\n", nil); err == nil ||
		!strings.Contains(err.Error(), "no checkout") {
		t.Errorf("a local action with no loader: %v", err)
	}
	// A remote action that policy allows still needs somewhere to fetch into.
	_, err := compileWith(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: actions/setup-go@v6\n", nil)
	if err == nil || !strings.Contains(err.Error(), "no action cache") {
		t.Errorf("remote action refusal: %v", err)
	}

	// And one policy refuses is refused for that reason instead, which is a
	// different fix in a different file.
	_, err = CompileWith(t.Context(), parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: evil/thing@v1\n"),
		"b", testScope(), Options{Policy: actions.DefaultPolicy()})
	if err == nil || !strings.Contains(err.Error(), "allowed") {
		t.Errorf("a refused action should name the allowlist: %v", err)
	}
}

func TestNestedCompositeAndDepth(t *testing.T) {
	outer := "outputs:\n  v:\n    value: ${{ steps.in.outputs.line }}\nruns:\n  using: composite\n  steps:\n    - uses: ./a\n      id: in\n      with: {who: nested}\n"
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: ./outer
        id: o
      - run: echo ${{ steps.o.outputs.v }}
`, mapLoader{"outer": outer, "a": greet})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	if job.Steps[0].ID != "o/in/say" {
		t.Errorf("a nested id carries both namespaces: %q", job.Steps[0].ID)
	}
	last := job.Steps[len(job.Steps)-1].Late["GHA_EXPR_1"]
	if last == nil || last.References()[0] != "o/in/say" {
		t.Errorf("an output of an output should resolve to the innermost step: %+v", last)
	}

	chain := mapLoader{}
	for i := 0; i < maxActionDepth+2; i++ {
		chain[fmt.Sprintf("c%d", i)] = fmt.Sprintf("runs:\n  using: composite\n  steps:\n    - uses: ./c%d\n", i+1)
	}
	if _, err := compileWith(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: ./c0\n", chain); err == nil ||
		!strings.Contains(err.Error(), "deep") {
		t.Errorf("a very long chain should hit the depth cap: %v", err)
	}
}

func TestDockerStep(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: docker://ghcr.io/me/lint:1.2
        name: lint
        with:
          args: --level "very strict" ${{ github.event.pull_request.title }}
          entrypoint: /bin/lint
          config: .lint.yml
        env:
          MODE: ci
`, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	if job.RequiredExecutor() != registry.ExecutorDocker {
		t.Error("a docker:// step should make the job require the docker executor")
	}
	c := job.Steps[0].Container
	if c == nil || c.Image != "ghcr.io/me/lint:1.2" || c.Entrypoint != "/bin/lint" {
		t.Fatalf("container = %+v", c)
	}
	// Split like a shell would, then never seen by one — and an expression is
	// atomic, so the title is exactly one argument however many words it has.
	// It cannot become a flag by containing a space.
	want := []string{"--level", "very strict", "; rm -rf / #"}
	if strings.Join(c.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q, want %q", c.Args, want)
	}
	if job.Steps[0].Env["INPUT_CONFIG"] != ".lint.yml" || job.Steps[0].Env["MODE"] != "ci" {
		t.Errorf("env = %+v", job.Steps[0].Env)
	}
	if job.Steps[0].Run != "" {
		t.Errorf("a container step has no command: %q", job.Steps[0].Run)
	}
}

func TestDockerStepRefusals(t *testing.T) {
	cases := map[string]string{
		"a late argument":       "      - id: p\n        run: make\n      - uses: docker://alpine\n        with:\n          args: [\"${{ steps.p.outputs.x }}\"]\n",
		"a hyphenated input":    "      - uses: docker://alpine\n        with:\n          repo-token: x\n",
		"a working-directory":   "      - uses: docker://alpine\n        working-directory: sub\n",
		"an unterminated quote": "      - uses: docker://alpine\n        with:\n          args: \"--msg 'oops\"\n",
	}
	for name, steps := range cases {
		if _, err := compileWith(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n"+steps, nil); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestLocalDockerAction(t *testing.T) {
	action := `
inputs:
  level:
    default: warn
runs:
  using: docker
  image: docker://ghcr.io/me/lint@sha256:` + strings.Repeat("a", 64) + `
  args: ["--level", "${{ inputs.level }}"]
  env:
    FROM_ACTION: "yes"
`
	job, err := compileWith(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: ./lint\n        with: {level: error}\n", mapLoader{"lint": action})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	st := job.Steps[0]
	if !st.Container.Pinned() || strings.Join(st.Container.Args, " ") != "--level error" {
		t.Errorf("container = %+v", st.Container)
	}
	if st.Env["INPUT_LEVEL"] != "error" || st.Env["FROM_ACTION"] != "yes" {
		t.Errorf("env = %+v", st.Env)
	}
}

func TestSplitArgs(t *testing.T) {
	got, err := splitArgs(`a "b c" 'd "e"' f\ g "h\"i" ''`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b c", `d "e"`, "f g", `h"i`, ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("= %q, want %q", got, want)
	}
	// An expression stays whole inside its word, spaces and quotes and all.
	if got, _ := splitArgs(`--title=${{ format('{0} x', github.ref) }} next`); len(got) != 2 ||
		got[0] != `--title=${{ format('{0} x', github.ref) }}` {
		t.Errorf("an expression was split: %q", got)
	}
	// Nothing is expanded: this is an argv, never a shell's input.
	if got, _ := splitArgs("$HOME *.go `id`"); strings.Join(got, "|") != "$HOME|*.go|`id`" {
		t.Errorf("= %q", got)
	}
}

// End to end through the resolver, with real files: the action is read from
// the same checkout through the same os.Root, and one linked out of the tree
// is refused by the kernel.
func TestResolveReadsLocalActionsFromTheCheckout(t *testing.T) {
	ws := workspace(t, map[string]string{
		".github/workflows/ci.yml":         "on: [push]\njobs:\n  test:\n    runs-on: [macos]\n    steps:\n      - uses: ./.github/actions/greet\n        with: {who: disk}\n",
		".github/actions/greet/action.yml": greet,
	})
	reg, src, err := Resolve(t.Context(), ws, "test", testScope())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if src.Native || len(reg.Jobs["test"].Steps) != 2 {
		t.Errorf("src = %+v, steps = %d", src, len(reg.Jobs["test"].Steps))
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "action.yml"), []byte(greet), 0o600); err != nil {
		t.Fatal(err)
	}
	ws2 := workspace(t, map[string]string{
		".github/workflows/ci.yml": "jobs:\n  test:\n    runs-on: [macos]\n    steps:\n      - uses: ./evil\n        with: {who: x}\n",
	})
	if err := os.Symlink(outside, filepath.Join(ws2, "evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := Resolve(t.Context(), ws2, "test", testScope()); err == nil {
		t.Error("an action directory linked out of the workspace must not be read")
	}
	if _, err := (rootLoader{}).Load("./../x"); err == nil {
		t.Error("a path that climbs out of the repository is refused before any read")
	}
}

// A51: a local JavaScript action is repository code: its directory is a path
// in the workspace, known now, and it asks for no isolation.
func TestLocalJavaScriptAction(t *testing.T) {
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - uses: ./.github/actions/hello
        id: hi
        with:
          who: world
`, mapLoader{".github/actions/hello": "inputs:\n  who:\n    required: true\nruns:\n  using: node24\n  main: index.js\n"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	st := job.Steps[0]
	if st.Env["GHA_ACTION_DIR"] != ".github/actions/hello" || st.Env["INPUT_WHO"] != "world" {
		t.Errorf("env = %v", st.Env)
	}
	if _, set := st.Env["GITHUB_ACTION_REPOSITORY"]; set {
		t.Error("a local action has no action repository")
	}
	if job.RequiresIsolation {
		t.Error("a local action is the repository's own code")
	}

	_, err = compileWith(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: ./.github/actions/hello\n",
		mapLoader{".github/actions/hello": "inputs:\n  who:\n    required: true\nruns:\n  using: node24\n  main: index.js\n"})
	if err == nil || !strings.Contains(err.Error(), `requires the input "who"`) {
		t.Errorf("a missing required input should be refused, got %v", err)
	}
}

// A51: pre runs before the job's first step under pre-if, post after its last
// in reverse under post-if and only if main ran, and all three steps of one
// use share the use that carries $GITHUB_STATE between them.
func TestJavaScriptActionPreAndPost(t *testing.T) {
	hooks := "runs:\n  using: node20\n  pre: pre.js\n  main: main.js\n  post: post.js\n  post-if: success()\n"
	job, err := compileWith(t, `
jobs:
  b:
    runs-on: [linux]
    steps:
      - run: first
      - uses: ./a
        id: one
      - uses: ./b
      - run: last
`, mapLoader{"a": hooks, "b": "runs:\n  using: node20\n  main: main.js\n  post: post.js\n"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	var order []string
	for _, st := range job.Steps {
		order = append(order, st.Name+"="+st.Env["GHA_ACTION_ENTRY"])
	}
	want := []string{
		"Pre Run ./a=pre.js", "=", "Run ./a=main.js", "Run ./b=main.js", "=",
		"Post Run ./b=post.js", "Post Run ./a=post.js",
	}
	if strings.Join(order, " | ") != strings.Join(want, " | ") {
		t.Errorf("order:\n got %v\nwant %v", order, want)
	}
	pre, mainA, mainB, postB, postA := job.Steps[0], job.Steps[2], job.Steps[3], job.Steps[5], job.Steps[6]
	if pre.ActionUse == "" || pre.ActionUse != mainA.ActionUse || mainA.ActionUse != postA.ActionUse {
		t.Errorf("one use's steps should share it: %q %q %q", pre.ActionUse, mainA.ActionUse, postA.ActionUse)
	}
	if mainB.ActionUse == mainA.ActionUse || mainB.ActionUse != postB.ActionUse {
		t.Error("each use of an action has its own")
	}
	if mainA.ID != "one" || mainB.ID == "" {
		t.Errorf("main keeps the uses: step's id, or gets one for post to name: %q, %q", mainA.ID, mainB.ID)
	}
	for _, c := range []struct {
		st   registry.Step
		want string
	}{
		{pre, "always()"},
		{postA, "success()"},
		{postA, "steps.one.outcome != 'skipped'"},
		{postB, "always()"},
	} {
		if c.st.If == nil || !strings.Contains(c.st.If.Source(), c.want) {
			t.Errorf("%s: condition should contain %q", c.st.Name, c.want)
		}
	}
}
