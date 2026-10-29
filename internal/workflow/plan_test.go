package workflow

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const planSHA = "0123456789abcdef0123456789abcdef01234567"

// fleet is a template repository's workflows, cut down to what planning reads.
var fleet = map[string]string{
	"checks.yml": `
name: Checks
on:
  push:
    branches: [main]
  pull_request:
concurrency:
  group: checks-${{ github.ref }}
  cancel-in-progress: true
jobs:
  lint:
    runs-on: blacksmith-4vcpu-ubuntu-2404
    steps: [{run: bun run lint}]
  test:
    runs-on: ubuntu-latest
    steps: [{run: bun test}]
  web:
    name: Web build
    needs: [lint, test]
    runs-on: ubuntu-latest
    steps: [{run: bun run build}]
`,
	"deploy-staging.yml": `
name: Deploy staging
on:
  push:
    branches: [main]
    paths: ['apps/web/**', '!apps/web/**/*.md']
permissions:
  contents: read
  deployments: write
concurrency: deploy-staging
jobs:
  web:
    environment: staging
    runs-on: ubuntu-latest
    permissions: {contents: write}
    steps: [{run: ./deploy.sh}]
`,
	"deploy-production.yml": `
name: Deploy production
run-name: Production from ${{ inputs.ref }}${{ inputs.dry && ' (dry run)' || '' }}
on:
  workflow_dispatch:
    inputs:
      ref: {required: true, type: string}
      dry: {type: boolean, default: false}
      target: {type: choice, options: [web, api], default: web}
jobs:
  web:
    name: Deploy ${{ inputs.target }}
    environment: production
    runs-on: ubuntu-latest
    steps: [{run: ./deploy.sh}]
`,
	"release.yml": `
on:
  push:
    tags: ['v[0-9]+.[0-9]+.[0-9]+']
jobs:
  publish:
    runs-on: macos-latest
    timeout-minutes: 30
    steps: [{run: ./publish.sh}]
`,
	"changelog.yml": `
on:
  pull_request:
    types: [closed]
    branches: [main]
jobs:
  stamp:
    if: github.event.pull_request.merged == true
    runs-on: ubuntu-latest
    steps: [{run: ./stamp.sh}]
`,
}

func fleetFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[Dir+"/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func changedFiles(paths ...string) func() ([]string, error) {
	return func() ([]string, error) { return paths, nil }
}

func planned(t *testing.T, plan *Plan) []string {
	t.Helper()
	var out []string
	for _, w := range plan.Workflows {
		if w.Error != "" {
			t.Errorf("%s: %s", w.File, w.Error)
		}
		out = append(out, strings.TrimPrefix(w.File, Dir+"/"))
	}
	return out
}

func mustPlan(t *testing.T, files map[string]string, ev Event, opts PlanOptions) *Plan {
	t.Helper()
	if ev.SHA == "" {
		ev.SHA = planSHA
	}
	plan, err := PlanEvent(fleetFS(files), ev, opts)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPlanTriggers(t *testing.T) {
	cases := []struct {
		name    string
		ev      Event
		changed []string
		want    []string
	}{
		{"push to main touching the app",
			Event{Name: EventPush, Ref: "refs/heads/main"}, []string{"apps/web/src/a.ts"},
			[]string{"checks.yml", "deploy-staging.yml"}},
		{"push to main touching only the app's docs",
			Event{Name: EventPush, Ref: "refs/heads/main"}, []string{"apps/web/docs/a.md", "README.md"},
			[]string{"checks.yml"}},
		{"push to a feature branch",
			Event{Name: EventPush, Ref: "refs/heads/feature/x"}, []string{"apps/web/src/a.ts"},
			nil},
		{"a release tag",
			Event{Name: EventPush, Ref: "refs/tags/v1.2.3"}, nil,
			[]string{"release.yml"}},
		{"a tag the release filter does not match",
			Event{Name: EventPush, Ref: "refs/tags/nightly"}, nil,
			nil},
		{"a pull request opened",
			Event{Name: EventPullRequest, Action: "opened", Ref: "refs/pull/7/merge", BaseRef: "main", HeadRef: "x"}, nil,
			[]string{"checks.yml"}},
		{"a pull request labeled",
			Event{Name: EventPullRequest, Action: "labeled", Ref: "refs/pull/7/merge", BaseRef: "main"}, nil,
			nil},
		{"a pull request closed into main",
			Event{Name: EventPullRequest, Action: "closed", Ref: "refs/pull/7/merge", BaseRef: "main"}, nil,
			[]string{"changelog.yml"}},
		{"a pull request closed into another base",
			Event{Name: EventPullRequest, Action: "closed", Ref: "refs/pull/7/merge", BaseRef: "next"}, nil,
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := mustPlan(t, fleet, c.ev, PlanOptions{Changed: changedFiles(c.changed...)})
			if got := planned(t, plan); !slices.Equal(got, c.want) {
				t.Errorf("planned %v, want %v", got, c.want)
			}
		})
	}
}

func TestPlanJobs(t *testing.T) {
	plan := mustPlan(t, fleet, Event{Name: EventPush, Ref: "refs/heads/main", Actor: "octo"}, PlanOptions{
		Repository:   "Abyssinia-Labs/start",
		LabelAliases: map[string][]string{"blacksmith-4vcpu-ubuntu-2404": {"linux", "big"}},
		Changed:      changedFiles("apps/web/a.ts"),
	})
	checks, staging := plan.Workflows[0], plan.Workflows[1]

	if checks.Name != "Checks" || checks.Concurrency == nil ||
		checks.Concurrency.Group != "checks-refs/heads/main" || !checks.Concurrency.CancelInProgress {
		t.Errorf("checks: %+v %+v", checks, checks.Concurrency)
	}
	var keys []string
	for _, j := range checks.Jobs {
		keys = append(keys, j.Key)
	}
	if !slices.Equal(keys, []string{"lint", "test", "web"}) {
		t.Errorf("order %v", keys)
	}
	lint, web := checks.Jobs[0], checks.Jobs[2]
	if !slices.Equal(lint.Labels, []string{"linux", "big"}) {
		t.Errorf("lint labels %v", lint.Labels)
	}
	if web.Name != "Web build" || !slices.Equal(web.Needs, []string{"lint", "test"}) ||
		!slices.Equal(web.Labels, []string{"linux"}) || web.TimeoutMinutes != 360 {
		t.Errorf("web %+v", web)
	}
	if len(web.Permissions) != 2 || web.Permissions["contents"] != "read" || web.Permissions["packages"] != "read" {
		t.Errorf("a job with no permissions anywhere gets the restricted default, got %v", web.Permissions)
	}

	if staging.Concurrency.Group != "deploy-staging" || staging.Concurrency.CancelInProgress {
		t.Errorf("staging concurrency %+v", staging.Concurrency)
	}
	sj := staging.Jobs[0]
	if sj.Environment != "staging" || len(sj.Permissions) != 1 || sj.Permissions["contents"] != "write" {
		t.Errorf("a job's own permissions replace the workflow's: %+v", sj)
	}
}

func TestPlanDispatch(t *testing.T) {
	ev := Event{Name: EventWorkflowDispatch, Ref: "refs/heads/main", Workflow: Dir + "/deploy-production.yml",
		Inputs: map[string]string{"ref": "v1.2.3", "dry": "true"}}
	plan := mustPlan(t, fleet, ev, PlanOptions{})
	if got := planned(t, plan); !slices.Equal(got, []string{"deploy-production.yml"}) {
		t.Fatalf("planned %v", got)
	}
	w := plan.Workflows[0]
	if w.RunName != "Production from v1.2.3 (dry run)" {
		t.Errorf("run-name %q: a boolean input must be a boolean in the inputs context", w.RunName)
	}
	if w.Inputs["target"] != "web" || w.Inputs["dry"] != "true" || w.Inputs["ref"] != "v1.2.3" {
		t.Errorf("inputs %v", w.Inputs)
	}
	if w.Jobs[0].Name != "Deploy web" || w.Jobs[0].Environment != "production" {
		t.Errorf("job %+v", w.Jobs[0])
	}

	refused := map[string]Event{
		"declares no input":       withInputs(ev, map[string]string{"ref": "x", "force": "1"}),
		`input "ref" is required`: withInputs(ev, map[string]string{"dry": "true"}),
		"is a boolean":            withInputs(ev, map[string]string{"ref": "x", "dry": "yes"}),
		"is one of web, api":      withInputs(ev, map[string]string{"ref": "x", "target": "db"}),
		"no workflow_dispatch trigger": {Name: EventWorkflowDispatch, SHA: planSHA, Ref: "refs/heads/main",
			Workflow: Dir + "/checks.yml"},
		"there is no workflow": {Name: EventWorkflowDispatch, SHA: planSHA, Ref: "refs/heads/main",
			Workflow: Dir + "/nope.yml"},
	}
	for want, ev := range refused {
		if _, err := PlanEvent(fleetFS(fleet), ev, PlanOptions{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
}

func withInputs(ev Event, in map[string]string) Event {
	ev.SHA = planSHA
	ev.Inputs = in
	return ev
}

// A workflow that cannot be planned is in the plan, with why.
func TestPlanErrorsAreReported(t *testing.T) {
	files := map[string]string{
		"broken.yml": "on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    stepz: []\n",
		"cycle.yml": `
on: push
jobs:
  a: {needs: b, runs-on: ubuntu-latest, steps: [{run: x}]}
  b: {needs: a, runs-on: ubuntu-latest, steps: [{run: x}]}
`,
		"missing.yml": `
on: push
jobs:
  a: {needs: nope, runs-on: ubuntu-latest, steps: [{run: x}]}
`,
		"filter.yml": `
on:
  push:
    branch: [main]
jobs:
  a: {runs-on: ubuntu-latest, steps: [{run: x}]}
`,
		"runid.yml": `
on: push
concurrency: ${{ github.workflow }}-${{ github.run_id }}
jobs:
  a: {runs-on: ubuntu-latest, steps: [{run: x}]}
`,
		"paths.yml": `
on:
  push:
    paths: [src/**]
jobs:
  a: {runs-on: ubuntu-latest, steps: [{run: x}]}
`,
	}
	plan, err := PlanEvent(fleetFS(files), Event{Name: EventPush, SHA: planSHA, Ref: "refs/heads/main"}, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"broken.yml":  "stepz",
		"cycle.yml":   "jobs a, b need each other in a cycle",
		"missing.yml": `needs "nope", which is not a job`,
		"filter.yml":  `"branch" is not something push filters on`,
		"runid.yml":   "github.run_id is not known when a run is planned",
		"paths.yml":   "no diff to read",
	}
	if len(plan.Workflows) != len(want) {
		t.Fatalf("planned %d workflows, want %d", len(plan.Workflows), len(want))
	}
	for _, w := range plan.Workflows {
		name := strings.TrimPrefix(w.File, Dir+"/")
		if !strings.Contains(w.Error, want[name]) {
			t.Errorf("%s: error %q, want it to contain %q", name, w.Error, want[name])
		}
		if len(w.Jobs) != 0 {
			t.Errorf("%s: a workflow that cannot be planned has no jobs", name)
		}
	}
}

// What latem does not plan yet fails the job, by name, and leaves the rest of
// the run to run.
func TestPlanJobErrors(t *testing.T) {
	files := map[string]string{"ci.yml": `
on: push
jobs:
  matrix:
    strategy: {matrix: {node: [20, 22]}}
    runs-on: ubuntu-latest
    steps: [{run: x}]
  called:
    uses: ./.github/workflows/reusable.yml
  win:
    runs-on: windows-latest
    steps: [{run: x}]
  late:
    name: after ${{ needs.matrix.outputs.v }}
    runs-on: ubuntu-latest
    steps: [{run: x}]
  fine:
    needs: [matrix]
    if: ${{ always() }}
    continue-on-error: ${{ github.ref == 'refs/heads/main' }}
    runs-on: [self-hosted, macOS]
    steps: [{run: x}]
`}
	plan := mustPlan(t, files, Event{Name: EventPush, Ref: "refs/heads/main"}, PlanOptions{})
	errs := map[string]string{}
	var fine PlannedJob
	for _, j := range plan.Workflows[0].Jobs {
		errs[j.Key] = j.Error
		if j.Key == "fine" {
			fine = j
		}
	}
	for key, want := range map[string]string{
		"matrix": "a matrix is not planned yet",
		"called": "reusable workflows are not planned yet",
		"win":    "no latem runner can be Windows",
		"late":   "reads needs, which is only known once the job runs",
	} {
		if !strings.Contains(errs[key], want) {
			t.Errorf("%s: %q, want it to contain %q", key, errs[key], want)
		}
	}
	if fine.Error != "" || !fine.IfChecksStatus || !fine.ContinueOnError || !slices.Equal(fine.Labels, []string{"macos"}) {
		t.Errorf("fine: %+v", fine)
	}
}

func TestPlanEventsRefused(t *testing.T) {
	for want, ev := range map[string]Event{
		"schedule events are not planned yet": {Name: "schedule", SHA: planSHA},
		`does not plan "issues" events`:       {Name: "issues", SHA: planSHA},
		"is not a commit id":                  {Name: EventPush, SHA: "main", Ref: "refs/heads/main"},
		"neither a branch nor a tag":          {Name: EventPush, SHA: planSHA, Ref: "main"},
		"its action and its base branch":      {Name: EventPullRequest, SHA: planSHA, Ref: "refs/pull/1/merge"},
		"only a workflow_dispatch has inputs": {Name: EventPush, SHA: planSHA, Ref: "refs/heads/main", Inputs: map[string]string{"a": "b"}},
	} {
		if _, err := PlanEvent(fleetFS(fleet), ev, PlanOptions{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error containing %q, got %v", want, err)
		}
	}
}

// The diff is computed once per plan, and only if a filter reads it.
func TestPlanDiffIsLazy(t *testing.T) {
	calls := 0
	opts := PlanOptions{Changed: func() ([]string, error) {
		calls++
		return nil, errors.New("no mirror")
	}}
	mustPlan(t, fleet, Event{Name: EventPush, Ref: "refs/tags/v1.0.0"}, opts)
	if calls != 0 {
		t.Errorf("a tag push read the diff %d times; no workflow it triggers filters on paths", calls)
	}
	files := map[string]string{"a.yml": fleet["deploy-staging.yml"], "b.yml": fleet["deploy-staging.yml"]}
	mustPlan(t, files, Event{Name: EventPush, Ref: "refs/heads/main"}, opts)
	if calls != 1 {
		t.Errorf("read the diff %d times, want once", calls)
	}
}
