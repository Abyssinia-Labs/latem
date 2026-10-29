package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
)

const pinned = "0123456789abcdef0123456789abcdef01234567"

// fetchedAction writes an action into a fake cache and hands back a fetcher.
type fakeFetcher struct {
	root    string
	fetched []string
}

func (f *fakeFetcher) Fetch(_ context.Context, r actions.Ref) (actions.Resolved, error) {
	f.fetched = append(f.fetched, r.String())
	rel := filepath.Join(r.Host, r.Owner, r.Repo, pinned)
	return actions.Resolved{Ref: r, SHA: pinned, Dir: filepath.Join(f.root, rel), Rel: filepath.ToSlash(rel)}, nil
}

func newFetcher(t *testing.T, r actions.Ref, actionYML string) *fakeFetcher {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, r.Host, r.Owner, r.Repo, pinned)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte(actionYML), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fakeFetcher{root: root}
}

func compileRemote(t *testing.T, wf string, f *fakeFetcher) (*registry.Job, error) {
	t.Helper()
	return CompileWith(t.Context(), parse(t, wf), "b", testScope(), Options{
		Policy:  actions.Policy{Allowed: []string{"*/*"}},
		Fetcher: f,
	})
}

const remoteComposite = `
inputs:
  who:
    default: world
runs:
  using: composite
  steps:
    - id: greet
      shell: bash
      run: ${{ github.action_path }}/greet.sh "${{ inputs.who }}"
`

// A fetched composite action is inlined like a local one, except that its own
// files live in a cache whose location only the executor knows — so
// github.action_path is a late binding rather than a literal.
func TestRemoteCompositeIsInlinedWithALateActionPath(t *testing.T) {
	ref, _ := actions.ParseRef("acme/greet@" + pinned)
	f := newFetcher(t, ref, remoteComposite)

	job, err := compileRemote(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - uses: acme/greet@`+pinned+`
        id: hi
        with: {who: latem}
`, f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(f.fetched) != 1 {
		t.Fatalf("fetched %v", f.fetched)
	}
	if len(job.Steps) != 1 || job.Steps[0].ID != "hi/greet" {
		t.Fatalf("steps = %+v", job.Steps)
	}
	st := job.Steps[0]

	// Still no substitution into the command: the action's own path arrives as
	// a variable like any other value.
	if strings.Contains(st.Run, "action_path") || strings.Contains(st.Run, "/greet.sh") == false {
		t.Errorf("run = %q", st.Run)
	}
	lateKey := ""
	for k, v := range st.Late {
		if strings.Contains(v.Source(), "latem.action_dir") {
			lateKey = k
		}
	}
	if lateKey == "" {
		t.Fatalf("the action path should be late: %+v", st.Late)
	}

	// Resolved against whatever the executor says the cache is reachable at —
	// a host path on native, a mount target under docker.
	for _, dir := range []string{"/var/latem/actions", "/tmp/latem/actions"} {
		got, err := st.Late[lateKey].Resolve(registry.StepScope{ActionsDir: dir})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		want := dir + "/github.com/acme/greet/" + pinned
		if got != want {
			t.Errorf("action_path = %q, want %q", got, want)
		}
	}

	// And the job now needs an executor that confines it.
	if !job.RequiresIsolation {
		t.Error("a job running third-party code should require isolation")
	}
	reg := &registry.Registry{Version: 1, Jobs: map[string]*registry.Job{"b": job}}
	if err := reg.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

// The operator's opt-out is applied where the policy lives, so the compiled job
// simply does not ask for isolation.
func TestAllowUnisolatedClearsTheRequirement(t *testing.T) {
	ref, _ := actions.ParseRef("acme/greet@" + pinned)
	f := newFetcher(t, ref, remoteComposite)
	job, err := CompileWith(t.Context(),
		parse(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: acme/greet@"+pinned+"\n"),
		"b", testScope(),
		Options{Policy: actions.Policy{Allowed: []string{"*/*"}, AllowUnisolated: true}, Fetcher: f})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if job.RequiresIsolation {
		t.Error("allow_unisolated should clear the requirement at compile time")
	}
}

// A fetched docker action is a container step, exactly as a local one is.
func TestRemoteDockerAction(t *testing.T) {
	ref, _ := actions.ParseRef("acme/lint@" + pinned)
	f := newFetcher(t, ref, `
inputs:
  level:
    default: warn
runs:
  using: docker
  image: docker://ghcr.io/acme/lint:1
  args: ["--level", "${{ inputs.level }}"]
`)
	job, err := compileRemote(t, "jobs:\n  b:\n    runs-on: [linux]\n    steps:\n      - uses: acme/lint@"+pinned+"\n        with: {level: error}\n", f)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	c := job.Steps[0].Container
	if c == nil || c.Image != "ghcr.io/acme/lint:1" || strings.Join(c.Args, " ") != "--level error" {
		t.Fatalf("container = %+v", c)
	}
	if job.Steps[0].Env["INPUT_LEVEL"] != "error" {
		t.Errorf("env = %+v", job.Steps[0].Env)
	}
}

// A remote action's own `uses: ./path` is relative to its repository, not to
// the checkout, and resolving it needs a loader rooted at the fetched tree.
// Refused by name rather than reading a path out of the wrong repo.
func TestRemoteCompositeWithANestedLocalUsesIsRefused(t *testing.T) {
	ref, _ := actions.ParseRef("acme/outer@" + pinned)
	f := newFetcher(t, ref, "runs:\n  using: composite\n  steps:\n    - uses: ./inner\n")
	_, err := compileRemote(t, "jobs:\n  b:\n    runs-on: [macos]\n    steps:\n      - uses: acme/outer@"+pinned+"\n", f)
	if err == nil || !strings.Contains(err.Error(), "its own repository") {
		t.Errorf("want a refusal naming the nesting: %v", err)
	}
}

// A JavaScript action is still refused, and says which phase it waits for.
// A51: a fetched JavaScript action becomes one step that keeps the uses:
// step's id and condition, runs latem's constant command, and carries the
// action's directory, entry file and inputs as environment values.
func TestRemoteJavaScriptActionRuns(t *testing.T) {
	ref, _ := actions.ParseRef("oven-sh/setup-bun@v2")
	f := newFetcher(t, ref, `
inputs:
  bun-version:
    description: the version
  bun-version-file:
    description: a file naming it
  no-cache:
    default: "false"
  token:
    default: ${{ github.sha }}
runs:
  using: node20
  main: dist/setup/index.js
`)
	job, err := CompileWith(t.Context(), parse(t, `
jobs:
  b:
    runs-on: [macos]
    steps:
      - id: bun
        if: github.event_name == 'push'
        uses: oven-sh/setup-bun@v2
        with:
          bun-version-file: .bun-version
          surprise: hello
`), "b", testScope(), Options{Policy: actions.Policy{Allowed: []string{"oven-sh/*"}}, Fetcher: f})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	mustValidate(t, job)
	if len(job.Steps) != 1 {
		t.Fatalf("steps = %d, want main alone", len(job.Steps))
	}
	st := job.Steps[0]
	if st.ID != "bun" || st.Name != "Run oven-sh/setup-bun@v2" || st.Run != NodeRun {
		t.Errorf("step = id %q, name %q, run %q", st.ID, st.Name, st.Run)
	}
	want := map[string]string{
		"INPUT_BUN-VERSION-FILE":   ".bun-version",
		"INPUT_NO-CACHE":           "false",
		"INPUT_TOKEN":              "deadbeef",
		"INPUT_SURPRISE":           "hello",
		"GHA_ACTION_ENTRY":         "dist/setup/index.js",
		"GITHUB_ACTION_REPOSITORY": "oven-sh/setup-bun",
		"GITHUB_ACTION_REF":        "v2",
		"GITHUB_ACTION":            "bun",
	}
	for k, v := range want {
		if st.Env[k] != v {
			t.Errorf("%s = %q, want %q", k, st.Env[k], v)
		}
	}
	if _, set := st.Env["INPUT_BUN-VERSION"]; set {
		t.Error("an input with neither a value nor a default should be absent")
	}
	if _, late := st.Late["GHA_ACTION_DIR"]; !late {
		t.Error("a fetched action's directory is only known to the executor, so it should be late")
	}
	if !job.RequiresIsolation {
		t.Error("a fetched action is third-party code and should require isolation")
	}
	notes := strings.Join(job.Notes, "\n")
	if !strings.Contains(notes, "resolved to "+pinned) || !strings.Contains(notes, "surprise") {
		t.Errorf("notes = %q, want the resolved commit and the undeclared input", job.Notes)
	}
}

// Real metadata, setup-bun's among it, writes `type:` on an input and gives
// defaults as booleans and null. GitHub accepts all three; so does latem.
func TestActionMetadataAsActionsWriteIt(t *testing.T) {
	a, err := ParseAction([]byte(`
inputs:
  no-cache:
    required: false
    type: boolean
    default: false
  retries:
    default: 3
  bun-version-file:
    default: null
runs:
  using: "node24"
  main: "dist/setup/index.js"
`), "a")
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if d := a.Inputs["no-cache"].Default; d == nil || *d != "false" {
		t.Errorf("no-cache default = %v, want \"false\"", d)
	}
	if d := a.Inputs["retries"].Default; d == nil || *d != "3" {
		t.Errorf("retries default = %v, want \"3\"", d)
	}
	if a.Inputs["bun-version-file"].Default != nil {
		t.Error("default: null is no default")
	}
}
