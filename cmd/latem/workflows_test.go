package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// fakeOrigin answers `git remote get-url origin` for the checkout under test.
func fakeOrigin(t *testing.T, url string) {
	t.Helper()
	prev := originURL
	originURL = func(context.Context, string) string { return url }
	t.Cleanup(func() { originURL = prev })
}

// checkout writes a repository's workflow files into a fresh directory.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, ".github", "workflows", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const ciWorkflow = `name: CI
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 22
      - run: npm test
        env:
          NPM_TOKEN: ${{ secrets.NPM_TOKEN }}
  e2e:
    runs-on: blacksmith-4vcpu-ubuntu-2404
    steps:
      - run: echo "${{ vars.REGION }}"
  mac:
    runs-on: macos-latest
    steps:
      - run: xcodebuild -version
  legs:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-latest, blacksmith-2vcpu-ubuntu-2204]
        node: [22]
    steps:
      - run: node --version
  db:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:16
    steps:
      - run: psql --version
`

const deployWorkflow = `name: Deploy
on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      target:
        required: true
        type: choice
        options: [staging, production]
jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: npx convex deploy
        env:
          CONVEX_DEPLOY_KEY: ${{ secrets.CONVEX_DEPLOY_KEY }}
          GH: ${{ secrets.GITHUB_TOKEN }}
`

const releaseWorkflow = `on:
  release:
    types: [published]
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - run: echo publish
`

func checkFixture(t *testing.T) (dir, repo string, kc *memoryKeychain) {
	t.Helper()
	dir = configDirWith(t, baseConfig)
	kc = fakeStores(t, true)
	noAgentRunning(t)
	fakeOrigin(t, "git@github.com:me/app.git")
	repo = checkout(t, map[string]string{
		"ci.yml":      ciWorkflow,
		"deploy.yml":  deployWorkflow,
		"release.yml": releaseWorkflow,
		"broken.yml":  "on: [push\njobs: {",
	})
	return dir, repo, kc
}

func TestWorkflowsCheck_NamesEveryProblemAndItsFix(t *testing.T) {
	dir, repo, _ := checkFixture(t)

	out, err := latemIn(t, dir, "", "workflows", "check", repo, "--no-color")
	if exitCode(err) != ExitFailure {
		t.Fatalf("exit %d (%v), want 1: jobs here cannot run\n%s", exitCode(err), err, out)
	}
	for _, want := range []string{
		"repos.app, me/app",
		// The alias the Blacksmith label needs, written out.
		`runs-on "blacksmith-4vcpu-ubuntu-2404": no runner here carries it`,
		`[label_aliases] "blacksmith-4vcpu-ubuntu-2404" = ["linux"]`,
		// Remote actions against the policy, both ways they fail.
		"uses actions/setup-node@v4: actions/setup-node is not in [actions] allowed",
		`[actions] allowed += "actions/setup-node"`,
		`"v4" is a tag or branch`,
		"require_pinned = false",
		// A platform no alias supplies.
		`needs a runner labelled "macos"`,
		// A matrix is compiled leg by leg (A55), and runs-on read per leg:
		// one leg runs here, the other needs an alias.
		"[ ok ] legs (ubuntu-latest, 22)",
		"[FAIL] legs (blacksmith-2vcpu-ubuntu-2204, 22)",
		`runs-on "blacksmith-2vcpu-ubuntu-2204": no runner here carries it`,
		// Refused features, by name.
		"services:",
		// Secrets: names only, scoped as the run looks them up.
		"secrets with no value on this machine: NPM_TOKEN",
		"latem secrets set --repo app NPM_TOKEN",
		"secrets with no value for environment production: CONVEX_DEPLOY_KEY",
		"latem secrets set --repo app --env production CONVEX_DEPLOY_KEY",
		// A var that reads as empty.
		"vars with no value here, which read as empty: REGION",
		// A dispatch, planned with its required input filled.
		"planned for: push to main, workflow_dispatch",
		// What the events do not plan, and what does not parse.
		"not planned for a push to main, a pull request, or a dispatch (on: release)",
		".github/workflows/broken.yml",
		"`latem workflows check --fix`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, "GITHUB_TOKEN") {
		t.Error("GITHUB_TOKEN is minted per job and must not be reported missing")
	}
	if strings.Contains(out, "names none (A55)") {
		t.Error("a matrix job was compiled without its leg")
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
	// Reading is all a check does without --fix.
	if readConfigFile(t, dir) != baseConfig {
		t.Error("check without --fix changed config.toml")
	}
}

// A secret stored for the repository's environment satisfies the job that
// runs in it; the machine-wide name satisfies every job.
func TestWorkflowsCheck_FindsSecretsInEveryScopeARunReads(t *testing.T) {
	dir, repo, kc := checkFixture(t)
	kc.items[secrets.ScopedName("CONVEX_DEPLOY_KEY", "app", "production")] = "x"
	kc.items["NPM_TOKEN"] = "x"

	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--no-color")
	if strings.Contains(out, "CONVEX_DEPLOY_KEY") || strings.Contains(out, "NPM_TOKEN") {
		t.Errorf("stored secrets are still reported missing:\n%s", out)
	}
}

func TestWorkflowsCheck_FixWritesTheAliasAndTheAllowlist(t *testing.T) {
	dir, repo, _ := checkFixture(t)

	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--fix", "--no-color")
	cfg := loadConfigIn(t, dir)
	if got := cfg.LabelAliases["blacksmith-4vcpu-ubuntu-2404"]; !slices.Equal(got, []string{"linux"}) {
		t.Errorf("label alias = %v\n%s", got, out)
	}
	// The label only a matrix leg's runs-on named is fixed too.
	if got := cfg.LabelAliases["blacksmith-2vcpu-ubuntu-2204"]; !slices.Equal(got, []string{"linux"}) {
		t.Errorf("the matrix leg's alias = %v", got)
	}
	if !slices.Equal(cfg.Actions.Allowed, []string{"actions/setup-node"}) {
		t.Errorf("[actions] allowed = %v", cfg.Actions.Allowed)
	}
	if !strings.Contains(readConfigFile(t, dir), "# The operator's own note") {
		t.Error("--fix lost the operator's comment")
	}
	for _, want := range []string{"fixed in", `[label_aliases] blacksmith-4vcpu-ubuntu-2404 = ["linux"]`,
		`[actions] allowed = ["actions/setup-node"]`, "no agent is running"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The report is of the machine after the fix: e2e now runs.
	if strings.Contains(out, `runs-on "blacksmith-4vcpu-ubuntu-2404": no runner`) {
		t.Errorf("the report predates the fix:\n%s", out)
	}
	// Pinning is not --fix's to decide.
	if cfg.Actions.RequirePinned != nil {
		t.Error("--fix changed require_pinned")
	}
}

func TestWorkflowsCheck_AJobThatCanRunPasses(t *testing.T) {
	dir := configDirWith(t, baseConfig+"\n[label_aliases]\n\"blacksmith-4vcpu-ubuntu-2404\" = [\"linux\"]\n")
	fakeStores(t, true)
	fakeOrigin(t, "")
	repo := checkout(t, map[string]string{"ci.yml": `on: push
jobs:
  build:
    runs-on: [self-hosted, blacksmith-4vcpu-ubuntu-2404]
    steps:
      - uses: actions/checkout@v4
      - run: make
`})
	out, err := latemIn(t, dir, "", "workflows", "check", repo, "--repo", "app", "--no-color")
	if err != nil {
		t.Fatalf("a job this machine can run failed the check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[ ok ] build") || !strings.Contains(out, "runs on linux") || !strings.Contains(out, "1 of 1 jobs") {
		t.Errorf("output:\n%s", out)
	}
}

// A job that calls a workflow of the repository is checked as the called
// workflow's jobs, each by its job path, with the secrets its caller passes
// (A81).
func TestWorkflowsCheck_ACalledWorkflowsJobs(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	kc := fakeStores(t, true)
	kc.items[secrets.ScopedName("NPM_TOKEN", "app")] = "x"
	fakeOrigin(t, "")
	repo := checkout(t, map[string]string{
		"ci.yml": `on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: make}]
  release:
    needs: build
    uses: ./.github/workflows/release.yml
    with: {version: "1.2.3"}
    secrets: {NPM: "${{ secrets.NPM_TOKEN }}"}
`,
		"release.yml": `on:
  workflow_call:
    inputs:
      version: {type: string, required: true}
    secrets:
      NPM: {required: true}
jobs:
  pack:
    runs-on: ubuntu-latest
    steps:
      - run: npm publish
        env:
          VERSION: ${{ inputs.version }}
          NPM: ${{ secrets.NPM }}
          OTHER: ${{ secrets.OTHER }}
`,
	})
	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--repo", "app", "--no-color")
	for _, want := range []string{
		"[ ok ] build",
		"release/pack (release / pack)  runs on linux",
		"secrets the workflow calling this job does not pass, which read as empty: OTHER",
		// The called workflow is not planned for a push of its own.
		"(on: workflow_call)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NPM") {
		t.Errorf("NPM is passed as the repository's NPM_TOKEN, which is set:\n%s", out)
	}
}

// A job using a remote action is refused on the native executor unless the
// operator opts out of isolation (A47); the check says so with the opt-out.
func TestWorkflowsCheck_RemoteActionsNeedAnIsolatingRunner(t *testing.T) {
	dir := configDirWith(t, `schema_version = 1

[repos.app]
url          = "https://github.com/me/app.git"
allowed_refs = ["main"]

[actions]
allowed        = ["actions/*"]
require_pinned = false

[[runner]]
name     = "mac"
executor = "native"
labels   = ["macos", "arm64"]
`)
	fakeStores(t, false)
	fakeOrigin(t, "https://github.com/me/app.git")
	repo := checkout(t, map[string]string{"ci.yml": `on: push
jobs:
  build:
    runs-on: macos-latest
    steps:
      - uses: actions/setup-node@v4
      - run: npm test
`})
	out, _ := latemIn(t, dir, "", "workflows", "check", repo, "--no-color")
	if !strings.Contains(out, `"mac" is native`) || !strings.Contains(out, "allow_unisolated = true") {
		t.Errorf("output:\n%s", out)
	}
}

func TestWorkflowsCheck_ACheckoutWithNoWorkflows(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeStores(t, false)
	if _, err := latemIn(t, dir, "", "workflows", "check", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "has no .github/workflows") {
		t.Errorf("want a refusal naming the directory, got %v", err)
	}
}

func TestReadTriggers(t *testing.T) {
	cases := map[string][]string{
		`"push"`:                     {"push"},
		`["push","pull_request"]`:    {"push", "pull_request"},
		`{"release":{},"push":null}`: {"push", "release"},
	}
	for raw, want := range cases {
		got, _ := readTriggers([]byte(raw))
		if !slices.Equal(got, want) {
			t.Errorf("readTriggers(%s) = %v, want %v", raw, got, want)
		}
	}
	_, decl := readTriggers([]byte(`{"workflow_dispatch":{"inputs":{"t":{"required":true,"type":"choice","options":["a","b"]}}}}`))
	if in := decl["t"]; !in.Required || in.Type != "choice" || !slices.Equal(in.Options, []string{"a", "b"}) {
		t.Errorf("dispatch inputs = %+v", decl)
	}
}

// fakeRepoFiles answers readRepo with files and records where it was asked
// to read from.
func fakeRepoFiles(t *testing.T, files fstest.MapFS) *[]treeSource {
	t.Helper()
	prev := readRepo
	var asked []treeSource
	readRepo = func(_ context.Context, _ *config.Config, key string, from treeSource) (*repoFiles, error) {
		asked = append(asked, from)
		return &repoFiles{fsys: files, sha: strings.Repeat("ab", 20), where: "the files of " + key, close: func() {}}, nil
	}
	t.Cleanup(func() { readRepo = prev })
	return &asked
}

// With --repo and no path there is no checkout: the repository is read
// without one (A57), from GitHub unless --offline says the agent's mirror,
// and its .latem/jobs.yaml is checked beside its workflows.
func TestWorkflowsCheck_ARepositoryWithoutACheckout(t *testing.T) {
	dir := configDirWith(t, baseConfig+"\n[label_aliases]\n\"blacksmith-4vcpu-ubuntu-2404\" = [\"linux\"]\n")
	fakeStores(t, true)
	asked := fakeRepoFiles(t, fstest.MapFS{
		".github/workflows/ci.yml": {Data: []byte("on: push\njobs:\n  build:\n    runs-on: blacksmith-4vcpu-ubuntu-2404\n" +
			"    steps:\n      - uses: ./.github/actions/setup\n      - run: make\n")},
		".github/actions/setup/action.yml": {Data: []byte("name: setup\nruns:\n  using: composite\n  steps:\n" +
			"    - run: echo set up\n      shell: bash\n")},
		".latem/jobs.yaml": {Data: []byte("version: 1\njobs:\n  ios-smoke:\n    runs_on: [macos, xcode]\n" +
			"    steps:\n      - run: xcodebuild -version\n")},
	})

	out, err := latemIn(t, dir, "", "workflows", "check", "--repo", "app", "--no-color")
	if exitCode(err) != ExitFailure {
		t.Fatalf("exit %d (%v): the jobs.yaml job has no macos runner here\n%s", exitCode(err), err, out)
	}
	for _, want := range []string{
		"checking the files of app at abababababab (repos.app, me/app)",
		"[ ok ] build", "runs on linux",
		".latem/jobs.yaml", "[FAIL] ios-smoke", "no runner here carries all of macos, xcode",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, _ = latemIn(t, dir, "", "workflows", "check", "--repo", "app", "--offline", "--json")
	var rep struct {
		Source    string `json:"source"`
		Commit    string `json:"commit"`
		Jobs      int    `json:"jobs"`
		Failed    int    `json:"failed"`
		Workflows []struct {
			File string `json:"file"`
			Jobs []struct {
				Key, Level, Runner string
			} `json:"jobs"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json printed something that is not JSON: %v\n%s", err, out)
	}
	if rep.Source != "the files of app" || rep.Jobs != 2 || rep.Failed != 1 || len(rep.Workflows) != 2 ||
		rep.Workflows[1].File != ".latem/jobs.yaml" {
		t.Errorf("report = %+v", rep)
	}
	if !slices.Equal(*asked, []treeSource{fromRemote, fromMirror}) {
		t.Errorf("read from %v, want GitHub, then with --offline the mirror", *asked)
	}
}

func TestWorkflowsCheck_OfflineNeedsARepositoryAndNoPath(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeStores(t, false)
	if _, err := latemIn(t, dir, "", "workflows", "check", t.TempDir(), "--offline"); exitCode(err) != ExitUsage {
		t.Errorf("--offline with a path: exit %d (%v), want usage", exitCode(err), err)
	}
}

// The real reader, against a config whose agent has made no mirror yet.
func TestReadRepo_OfflineWithNoMirrorSaysSo(t *testing.T) {
	cfg := testConfig(t.TempDir())
	_, err := realReadRepo(context.Background(), cfg, "app", fromMirror)
	if err == nil || !strings.Contains(err.Error(), "has no mirror of app yet") {
		t.Errorf("err = %v", err)
	}
}

// Each job says what it needs of a machine, the report counts the kinds,
// and --verbose says where each need was read from (A59).
func TestWorkflowsCheck_SaysWhatEachJobNeeds(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeStores(t, false)
	fakeRepoFiles(t, fstest.MapFS{
		".github/workflows/ci.yml": {Data: []byte("on: push\njobs:\n" +
			"  web:\n    runs-on: ubuntu-latest\n    steps:\n      - run: bun test\n" +
			"  ios:\n    runs-on: macos-latest\n    steps:\n" +
			"      - run: DEVELOPER_DIR=/Applications/Xcode_27.app xcodebuild -scheme App -destination 'generic/platform=iOS Simulator' build\n")},
	})
	out, _ := latemIn(t, dir, "", "workflows", "check", "--repo", "app", "--no-color", "-v")
	for _, want := range []string{
		"needs: Linux · bun",
		"needs: iOS · ios-sdk · xcode 27",
		".github/workflows/ci.yml, step 1: DEVELOPER_DIR=/Applications/Xcode_27.app xcodebuild",
		"kinds of build: 1 Linux, 1 iOS — Xcode 27",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, _ = latemIn(t, dir, "", "workflows", "check", "--repo", "app", "--json")
	var rep struct {
		Workflows []struct {
			Jobs []struct {
				Key   string `json:"key"`
				Needs struct {
					Kind     string `json:"kind"`
					Requires []struct {
						Key, Version, Strength string
						VersionStrength        string `json:"version_strength"`
					} `json:"requires"`
				} `json:"needs"`
			} `json:"jobs"`
		} `json:"workflows"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, j := range rep.Workflows[0].Jobs {
		if j.Key != "ios" {
			continue
		}
		if j.Needs.Kind != "ios" || len(j.Needs.Requires) != 2 || j.Needs.Requires[1].Version != "27" ||
			j.Needs.Requires[1].VersionStrength != "declared" || j.Needs.Requires[1].Strength != "invoked" {
			t.Errorf("ios needs = %+v", j.Needs)
		}
	}
}
