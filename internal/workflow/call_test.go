package workflow

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// calls is sokie's release path, cut down: a dispatched deploy whose cli job
// calls release.yml, which calls package.yml (A81).
var calls = map[string]string{
	"deploy.yml": `
name: Deploy
on:
  workflow_dispatch:
    inputs:
      ref: {type: string, default: ''}
permissions:
  contents: read
jobs:
  convex:
    runs-on: ubuntu-latest
    outputs:
      sha: ${{ steps.s.outputs.sha }}
    steps: [{id: s, run: ./deploy.sh}]
  cli:
    needs: convex
    if: inputs.ref == ''
    permissions:
      contents: read
      id-token: write
    uses: ./.github/workflows/release.yml
    with:
      channel: production
      ref: ${{ needs.convex.outputs.sha }}
    secrets: inherit
  after:
    needs: cli
    runs-on: ubuntu-latest
    steps:
      - run: echo "$TARBALL"
        env:
          TARBALL: ${{ needs.cli.outputs.tarball }}
          RESULT: ${{ needs.cli.result }}
`,
	"release.yml": `
name: Release
on:
  workflow_call:
    inputs:
      channel: {type: string, required: true}
      ref: {type: string, default: main}
    outputs:
      tarball:
        value: ${{ jobs.package.outputs.tarball }}
permissions:
  contents: read
jobs:
  plan:
    runs-on: ubuntu-latest
    outputs:
      version: ${{ steps.p.outputs.version }}
      publish: ${{ steps.p.outputs.publish }}
    steps:
      - id: p
        run: ./plan.sh
        env:
          CHANNEL: ${{ inputs.channel }}
          REF: ${{ inputs.ref }}
  package:
    needs: plan
    if: needs.plan.outputs.publish == 'true'
    uses: ./.github/workflows/package.yml
    with:
      ref: ${{ inputs.ref }}
      version: ${{ needs.plan.outputs.version }}
      smoke: true
    secrets:
      NPM: ${{ secrets.NPM_TOKEN }}
  publish:
    needs: [plan, package]
    runs-on: ubuntu-latest
    environment: ${{ inputs.channel }}
    permissions:
      contents: read
      id-token: write
    steps:
      - run: npm publish "$TARBALL"
        env:
          TARBALL: ${{ needs.package.outputs.tarball }}
          VERSION: ${{ needs.package.outputs.version }}
`,
	"package.yml": `
name: Package
on:
  workflow_call:
    inputs:
      ref: {type: string, default: ''}
      version: {type: string, default: ''}
      smoke: {type: boolean}
      retries: {type: number, default: 2}
    secrets:
      NPM: {required: false}
    outputs:
      tarball:
        value: ${{ jobs.pack.outputs.tarball }}
      version:
        value: v${{ jobs.pack.outputs.version }}
jobs:
  pack:
    runs-on: ubuntu-latest
    outputs:
      tarball: ${{ steps.pack.outputs.tarball }}
      version: ${{ steps.pack.outputs.version }}
    steps:
      - id: pack
        run: npm pack
        env:
          REF: ${{ inputs.ref }}
          VERSION: ${{ inputs.version }}
          JOB: ${{ github.job }}
          NPM_TOKEN: ${{ secrets.NPM }}
      - if: inputs.smoke && inputs.retries == 2
        run: ./smoke.sh
`,
}

func dispatchDeploy(t *testing.T, files map[string]string, opts PlanOptions) []protocol.PlannedJob {
	t.Helper()
	ev := protocol.Event{Name: EventWorkflowDispatch, Ref: "refs/heads/main", SHA: planSHA, Workflow: Dir + "/deploy.yml"}
	plan, err := PlanEvent(fleetFS(files), ev, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Error != "" {
		t.Fatalf("plan: %+v", plan)
	}
	return plan[0].Jobs
}

// A called workflow's jobs are planned into the caller's run, under job
// paths, named as GitHub names them, after the caller's needs, with tokens no
// wider than the caller grants. A job needing the caller needs every job of
// the call (A81).
func TestPlanCalls(t *testing.T) {
	jobs := dispatchDeploy(t, calls, PlanOptions{})
	type row struct {
		key, name string
		needs     []string
		checks    bool
		env       string
		perms     map[string]string
	}
	read := map[string]string{"contents": "read"}
	oidc := map[string]string{"contents": "read", "id-token": "write"}
	want := []row{
		{"convex", "convex", nil, false, "", read},
		{"cli/plan", "cli / plan", []string{"convex"}, false, "", read},
		{"cli/package/pack", "cli / package / pack", []string{"convex", "cli/plan"}, false, "", read},
		{"cli/publish", "cli / publish", []string{"convex", "cli/plan", "cli/package/pack"}, false, "production", oidc},
		// needs.cli is three jobs' result together, which the machine works
		// out: the control plane would skip after any one of them skipped.
		{"after", "after", []string{"cli/plan", "cli/package/pack", "cli/publish"}, true, "", read},
	}
	var got []row
	for _, pj := range jobs {
		if pj.Error != "" {
			t.Errorf("%s: %s", pj.Key, pj.Error)
		}
		got = append(got, row{pj.Key, pj.Name, pj.Needs, pj.IfChecksStatus, pj.Environment, pj.Permissions})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planned\n%+v\nwant\n%+v", got, want)
	}
	for _, pj := range jobs {
		if !protocol.IsJobPath(pj.Key) {
			t.Errorf("%q is not a key the protocol carries", pj.Key)
		}
	}
	// The caller's with: and if: decide the called jobs' inputs and whether
	// they run, so the variables they read place them (A68).
	files := maps.Clone(calls)
	files["deploy.yml"] = strings.Replace(files["deploy.yml"], "channel: production", "channel: ${{ vars.CHANNEL }}", 1)
	for _, pj := range dispatchDeploy(t, files, PlanOptions{Vars: map[string]string{"CHANNEL": "staging"}}) {
		if strings.HasPrefix(pj.Key, "cli/") && !slices.Contains(pj.ReadsVars, "CHANNEL") {
			t.Errorf("%s reads vars %v, not CHANNEL", pj.Key, pj.ReadsVars)
		}
		if pj.Key == "cli/publish" && pj.Environment != "staging" {
			t.Errorf("cli/publish is in %q, not the staging its caller's with: gives", pj.Environment)
		}
	}
}

// planPush plans ci.yml, and the workflows it calls, for a push to main.
func planPush(t *testing.T, files map[string]string, opts PlanOptions) map[string]protocol.PlannedJob {
	t.Helper()
	plan := mustPlan(t, files, protocol.Event{Name: EventPush, Ref: "refs/heads/main"}, opts)
	if len(plan) != 1 || plan[0].Error != "" {
		t.Fatalf("plan: %+v", plan)
	}
	out := map[string]protocol.PlannedJob{}
	for _, pj := range plan[0].Jobs {
		out[pj.Key] = pj
	}
	return out
}

const calledPack = `
on:
  workflow_call:
    inputs:
      v: {type: string}
      flag: {type: boolean}
      count: {type: number, required: true}
    secrets:
      S: {required: true}
jobs:
  pack:
    runs-on: ubuntu-latest
    steps: [{run: x}]
`

// What GitHub refuses about a call, and what latem does not plan, fails the
// call by name, as one job under the caller's key; the rest of the run plans.
func TestPlanCallRefusals(t *testing.T) {
	caller := func(lines string) string {
		return "on: push\njobs:\n  first:\n    runs-on: ubuntu-latest\n    outputs: {v: x}\n    steps: [{run: x}]\n" +
			"  call:\n    needs: first\n" + lines
	}
	ok := "    uses: ./.github/workflows/x.yml\n    with: {count: 1}\n    secrets: {S: '${{ secrets.T }}'}\n"
	cases := []struct {
		name, ci, x, want string
		opts              PlanOptions
	}{
		{"another repository", "    uses: octo/tools/.github/workflows/x.yml@v1\n", calledPack,
			"a workflow in another repository, which latem does not run", PlanOptions{}},
		{"a ref on a local call", "    uses: ./.github/workflows/x.yml@main\n", calledPack, "without @ref", PlanOptions{}},
		{"not a workflow file", "    uses: ./ci/x.yml\n", calledPack, "not a file directly under .github/workflows", PlanOptions{}},
		{"no such file", "    uses: ./.github/workflows/y.yml\n", calledPack, "there is no .github/workflows/y.yml", PlanOptions{}},
		{"not callable", ok, "on: pull_request\njobs:\n  pack:\n    runs-on: x\n    steps: [{run: x}]\n",
			"has no on.workflow_call", PlanOptions{}},
		{"steps on a caller", ok + "    runs-on: ubuntu-latest\n", calledPack, "has no runs-on", PlanOptions{}},
		{"a matrix of calls", ok + "    strategy: {matrix: {a: [1]}}\n", calledPack, "strategy: is not planned yet", PlanOptions{}},
		{"an input it does not declare", strings.Replace(ok, "{count: 1}", "{count: 1, w: 2}", 1), calledPack,
			"declares no input w", PlanOptions{}},
		{"a required input left out", strings.Replace(ok, "{count: 1}", "{v: a}", 1), calledPack,
			"requires the input count", PlanOptions{}},
		{"a literal of another type", strings.Replace(ok, "{count: 1}", "{count: 1, flag: maybe}", 1), calledPack,
			`the input flag is a boolean, and "maybe" is not one`, PlanOptions{}},
		{"a number that is not one", strings.Replace(ok, "{count: 1}", "{count: lots}", 1), calledPack,
			"the input count is a number", PlanOptions{}},
		{"a secret it does not declare", strings.Replace(ok, "{S:", "{U: x, S:", 1), calledPack,
			"declares no secret U", PlanOptions{}},
		{"a required secret left out", strings.Replace(ok, "    secrets: {S: '${{ secrets.T }}'}\n", "", 1), calledPack,
			"requires the secret S", PlanOptions{}},
		{"a secret that is not a secret", strings.Replace(ok, "'${{ secrets.T }}'", "hunter2", 1), calledPack,
			"latem passes a secret written ${{ secrets.NAME }}, and nothing else", PlanOptions{}},
		{"an output reading inputs", ok, strings.Replace(calledPack, "jobs:",
			"    outputs:\n      o: {value: '${{ inputs.v }}'}\njobs:", 1),
			"a called workflow's output reads its jobs", PlanOptions{}},
		{"an output of no job", ok, strings.Replace(calledPack, "jobs:",
			"    outputs:\n      o: {value: '${{ jobs.build.outputs.x }}'}\njobs:", 1),
			"reads jobs.build, which is not a job of", PlanOptions{}},
		{"a planned field only a run can answer", strings.Replace(ok, "{count: 1}", "{count: 1, v: '${{ needs.first.outputs.v }}'}", 1),
			strings.Replace(calledPack, "  pack:\n", "  pack:\n    name: pack ${{ inputs.v }}\n", 1),
			"reads inputs.v, which its caller gives as ${{ needs.first.outputs.v }}, and that is only known once the job runs",
			PlanOptions{}},
		{"a wider token than the caller's", ok + "    permissions: {contents: read}\n",
			strings.Replace(calledPack, "  pack:\n", "  pack:\n    permissions: {contents: write}\n", 1),
			"permissions: asks for contents: write, and call, which calls it, grants less", PlanOptions{}},
		{"concurrency on the called workflow", ok, calledPack + "concurrency: one\n",
			"concurrency: on a called workflow is not planned yet", PlanOptions{}},
		{"a workflow that calls itself", ok, strings.Replace(calledPack, "  pack:\n    runs-on: ubuntu-latest\n    steps: [{run: x}]\n",
			"  again:\n    uses: ./.github/workflows/x.yml\n    with: {count: 2}\n    secrets: inherit\n", 1),
			"calls .github/workflows/x.yml, which is already calling down to here", PlanOptions{}},
		{"a control plane before 2.8", ok, calledPack, "that takes protocol 2.8", PlanOptions{NoCalls: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			jobs := planPush(t, map[string]string{"ci.yml": caller(c.ci), "x.yml": c.x}, c.opts)
			var called []string
			for key, pj := range jobs {
				if pj.Error != "" && key != "call" && !strings.HasPrefix(key, "call/") {
					t.Errorf("%s: %s", key, pj.Error)
				}
				if key == "call" || strings.HasPrefix(key, "call/") {
					called = append(called, key)
					if !strings.Contains(pj.Error, c.want) {
						t.Errorf("%s: %q, want it to contain %q", key, pj.Error, c.want)
					}
					if !slices.Contains(pj.Needs, "first") {
						t.Errorf("%s needs %v; a refused call still waits for the caller's needs", key, pj.Needs)
					}
				}
			}
			if len(called) != 1 {
				t.Errorf("planned %v; want the refused call as one job", called)
			}
		})
	}
}

// latem plans calls three deep, and refuses the fourth by name (A81).
func TestPlanCallDepth(t *testing.T) {
	level := func(next string) string {
		return "on: workflow_call\njobs:\n  leaf:\n    runs-on: ubuntu-latest\n    steps: [{run: x}]\n" +
			"  down:\n    uses: ./.github/workflows/" + next + "\n"
	}
	files := map[string]string{
		"ci.yml": "on: push\njobs:\n  a:\n    uses: ./.github/workflows/l1.yml\n",
		"l1.yml": level("l2.yml"), "l2.yml": level("l3.yml"), "l3.yml": level("l4.yml"),
		"l4.yml": "on: workflow_call\njobs:\n  leaf:\n    runs-on: ubuntu-latest\n    steps: [{run: x}]\n",
	}
	jobs := planPush(t, files, PlanOptions{})
	for _, key := range []string{"a/leaf", "a/down/leaf", "a/down/down/leaf"} {
		if pj, ok := jobs[key]; !ok || pj.Error != "" {
			t.Errorf("%s: %+v", key, pj)
		}
	}
	deep := jobs["a/down/down/down"]
	if !strings.Contains(deep.Error, "latem plans calls 3 deep") || !protocol.IsJobPath(deep.Key) {
		t.Errorf("the fourth call: %+v", deep)
	}
	if _, ok := jobs["a/down/down/down/leaf"]; ok {
		t.Error("a job four calls down was planned")
	}
}

// A run of a called workflow's job: its callers' if: answered against their
// own needs, its inputs from their with:, typed as declared, its needs by the
// ids it needs them by, and its secrets as its callers pass them (A81).
func TestCompileCalledJob(t *testing.T) {
	fsys := fleetFS(calls)
	deploy, err := Parse([]byte(calls["deploy.yml"]), Dir+"/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	success := func(outputs map[string]string) protocol.NeedResult {
		return protocol.NeedResult{Result: protocol.NeedSuccess, Outputs: outputs}
	}
	needs := func(publish string) map[string]protocol.NeedResult {
		return map[string]protocol.NeedResult{
			"convex":   success(map[string]string{"sha": "abc123"}),
			"cli/plan": success(map[string]string{"version": "1.2.3-staging.4", "publish": publish}),
		}
	}
	compile := func(t *testing.T, w *Workflow, files map[string]string, path string, needs map[string]protocol.NeedResult,
		inputs map[string]any) (*registry.Job, *Workflow, string) {
		t.Helper()
		if inputs == nil {
			inputs = map[string]any{"ref": ""}
		}
		job, declaredIn, key, err := CompileJobPath(t.Context(), fleetFS(files), w, path, runScope(needs, inputs), Options{})
		if err != nil {
			t.Fatal(err)
		}
		return job, declaredIn, key
	}

	job, declaredIn, key := compile(t, deploy, calls, "cli/package/pack", needs("true"), nil)
	if job.Skip || declaredIn.Path != Dir+"/package.yml" || key != "pack" {
		t.Fatalf("cli/package/pack: %+v in %v as %q", job, declaredIn, key)
	}
	env := job.Steps[0].Env
	// release's ref is needs.convex.outputs.sha, which it passes on; version
	// is a need of its own; github.job is the job's own id, as on GitHub.
	for name, want := range map[string]string{"REF": "abc123", "VERSION": "1.2.3-staging.4", "JOB": "pack"} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	// smoke is the boolean true and retries its default, the number 2.
	if len(job.Steps) != 2 {
		t.Errorf("%d steps; the smoke step's if: is true", len(job.Steps))
	}
	if !slices.Contains(job.Secrets, "NPM") || !reflect.DeepEqual(job.SecretsFrom, map[string]string{"NPM": "NPM_TOKEN"}) {
		t.Errorf("secrets %v from %v; NPM is NPM_TOKEN, inherited from the top", job.Secrets, job.SecretsFrom)
	}
	if _, ok := job.Inputs["ref"]; !ok || len(job.Inputs) != 1 {
		t.Errorf("inputs %v; admission gives the job the dispatch's", job.Inputs)
	}

	// A caller's false if: skips every job of its call, and says which.
	job, _, _ = compile(t, deploy, calls, "cli/package/pack", needs("false"), nil)
	if !job.Skip || !strings.Contains(job.SkipReason, "needs.plan.outputs.publish == 'true'") ||
		!strings.Contains(job.SkipReason, "cli/package") {
		t.Errorf("publish=false: %+v", job)
	}
	job, _, _ = compile(t, deploy, calls, "cli/plan", needs("true"), map[string]any{"ref": "v1.0.0"})
	if !job.Skip {
		t.Error("a rollback ran the CLI release, whose caller's if: is inputs.ref == ''")
	}
	// The caller's if: has no status function, so it is success() && …,
	// over the caller's needs.
	failed := needs("true")
	failed["convex"] = protocol.NeedResult{Result: protocol.NeedFailure}
	if job, _, _ = compile(t, deploy, calls, "cli/plan", failed, nil); !job.Skip {
		t.Error("cli/plan ran after its caller's need failed")
	}

	// A need on a call is the call's jobs together, and its outputs the
	// called workflow's, nested calls included.
	pack := success(map[string]string{"tarball": "sokie-1.2.3.tgz", "version": "1.2.3"})
	publishNeeds := needs("true")
	publishNeeds["cli/package/pack"] = pack
	job, _, _ = compile(t, deploy, calls, "cli/publish", publishNeeds, nil)
	if env := job.Steps[0].Env; env["TARBALL"] != "sokie-1.2.3.tgz" || env["VERSION"] != "v1.2.3" {
		t.Errorf("publish's env %v", env)
	}
	if job.Environment != "production" || job.SecretsFrom != nil {
		t.Errorf("publish is in %q with secrets from %v", job.Environment, job.SecretsFrom)
	}
	for _, c := range []struct {
		results map[string]string
		want    string
		skip    bool
	}{
		{map[string]string{"plan": "success", "package/pack": "success", "publish": "success"}, "success", false},
		// One job of the call skipped and none failed: the call succeeded.
		{map[string]string{"plan": "success", "package/pack": "skipped", "publish": "skipped"}, "success", false},
		{map[string]string{"plan": "success", "package/pack": "failure", "publish": "skipped"}, "failure", true},
		{map[string]string{"plan": "cancelled", "package/pack": "skipped", "publish": "skipped"}, "cancelled", true},
		{map[string]string{"plan": "skipped", "package/pack": "skipped", "publish": "skipped"}, "skipped", true},
	} {
		raw := map[string]protocol.NeedResult{}
		for k, r := range c.results {
			raw["cli/"+k] = protocol.NeedResult{Result: r}
		}
		raw["cli/package/pack"] = protocol.NeedResult{Result: c.results["package/pack"], Outputs: pack.Outputs}
		job, _, _ := compile(t, deploy, calls, "after", raw, nil)
		if job.Skip != c.skip {
			t.Errorf("%v: skip = %v, want %v", c.results, job.Skip, c.skip)
			continue
		}
		if !c.skip && (job.Steps[0].Env["RESULT"] != c.want || job.Steps[0].Env["TARBALL"] != "sokie-1.2.3.tgz") {
			t.Errorf("%v: needs.cli is %v", c.results, job.Steps[0].Env)
		}
	}

	// Without secrets: at the top nothing is passed, so NPM_TOKEN reaches
	// package as nothing.
	files := maps.Clone(calls)
	files["deploy.yml"] = strings.Replace(files["deploy.yml"], "    secrets: inherit\n", "", 1)
	top, err := Parse([]byte(files["deploy.yml"]), Dir+"/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	if job, _, _ := compile(t, top, files, "cli/package/pack", needs("true"), nil); !reflect.DeepEqual(job.SecretsFrom, map[string]string{"NPM": ""}) {
		t.Errorf("secrets from %v; nothing was passed to release", job.SecretsFrom)
	}
	if job, _, _ := compile(t, top, files, "cli/plan", needs("true"), nil); job.SecretsFrom == nil || len(job.SecretsFrom) != 0 {
		t.Errorf("secrets from %v; release was passed none", job.SecretsFrom)
	}

	// An output that is not the type its input declares fails the job.
	files = maps.Clone(calls)
	files["release.yml"] = strings.Replace(files["release.yml"], "smoke: true", "smoke: ${{ needs.plan.outputs.version }}", 1)
	if _, _, _, err := CompileJobPath(t.Context(), fleetFS(files), deploy, "cli/package/pack", runScope(needs("true"), map[string]any{"ref": ""}), Options{}); err == nil ||
		!strings.Contains(err.Error(), "the input smoke is a boolean") {
		t.Errorf("a version passed as a boolean: %v", err)
	}
	// A path that names no call, or no job, is refused.
	for path, want := range map[string]string{
		"convex/plan":    "calls no workflow",
		"cli/nope":       "declares no job \"nope\"",
		"cli/plan/x":     "calls no workflow",
		"a/b/c/d/e":      "more than 3 calls deep",
		"cli/package/xx": "declares no job \"xx\"",
	} {
		if _, _, _, err := CompileJobPath(t.Context(), fsys, deploy, path, runScope(nil, nil), Options{}); err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	// Compiled outside a workflow run, a call is refused, and so is with:
	// on a job that calls nothing.
	if _, err := CompileWith(t.Context(), deploy, "cli", testScope(), Options{}); err == nil ||
		!strings.Contains(err.Error(), "a call is planned into a workflow run") {
		t.Errorf("a call outside a workflow run: %v", err)
	}
	stray := "on: push\njobs:\n  a:\n    runs-on: [linux]\n    with: {x: 1}\n    steps: [{run: x}]\n"
	if _, err := CompileWith(t.Context(), parse(t, stray), "a", testScope(), Options{}); err == nil ||
		!strings.Contains(err.Error(), "only a job that calls a workflow has them") {
		t.Errorf("with: on a job with steps: %v", err)
	}
}

// What a secret of a called workflow reads as, through the levels above it.
func TestPassSecrets(t *testing.T) {
	inherit := &callSpec{inherit: true}
	none := &callSpec{}
	mapped := &callSpec{secrets: map[string]string{"npm": "${{ secrets.NPM_TOKEN }}", "CF": "${{secrets.CLOUDFLARE}}"}}
	for _, c := range []struct {
		name string
		spec *callSpec
		from map[string]string
		want map[string]string
	}{
		{"inherit at the top reads each as itself", inherit, nil, nil},
		{"inherit passes on what was passed", inherit, map[string]string{"A": "X"}, map[string]string{"A": "X"}},
		{"none passes none", none, nil, map[string]string{}},
		{"a map passes one each, upper-cased", mapped, nil, map[string]string{"NPM": "NPM_TOKEN", "CF": "CLOUDFLARE"}},
		{"a map of what was passed", mapped, map[string]string{"NPM_TOKEN": "TOP"}, map[string]string{"NPM": "TOP", "CF": ""}},
	} {
		got, err := passSecrets(c.spec, c.from)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

// A called workflow's input arrives as its type, from a literal, a default,
// or an expression's string.
func TestCallValue(t *testing.T) {
	for _, c := range []struct {
		typ  string
		in   any
		want any
	}{
		{"string", "a", "a"}, {"string", true, "true"}, {"string", float64(3), "3"}, {"string", nil, ""},
		{"boolean", true, true}, {"boolean", "false", false},
		{"number", float64(2), float64(2)}, {"number", " 2.5 ", 2.5},
	} {
		got, err := callValue("x", c.typ, c.in)
		if err != nil || got != c.want {
			t.Errorf("%s %#v: %#v, %v; want %#v", c.typ, c.in, got, err, c.want)
		}
	}
	for _, c := range []struct {
		typ string
		in  any
	}{{"boolean", "yes"}, {"boolean", float64(1)}, {"number", "two"}, {"number", true}, {"string", []any{"a"}}} {
		if _, err := callValue("x", c.typ, c.in); err == nil {
			t.Errorf("%s %#v was accepted", c.typ, c.in)
		}
	}
	if got := inputDefault(callInput{Type: "boolean"}); got != false {
		t.Errorf("a boolean's default is %#v", got)
	}
	if got := inputDefault(callInput{Type: "number"}); got != float64(0) {
		t.Errorf("a number's default is %#v", got)
	}
}
