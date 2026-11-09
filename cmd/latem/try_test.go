package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	trySHA  = "1111111111111111111111111111111111111111"
	tryBase = "2222222222222222222222222222222222222222"
)

// take-one's shape: a job that needs another and runs only when that one's
// output says so, which a try runs anyway (A76).
const tryChecks = `name: Checks
on:
  pull_request:
  push:
    branches: [main]
jobs:
  changes:
    runs-on: ubuntu-latest
    outputs:
      beacon: ${{ steps.filter.outputs.beacon }}
    steps:
      - id: filter
        run: echo beacon=true >> "$GITHUB_OUTPUT"
  beacon:
    needs: changes
    if: needs.changes.outputs.beacon == 'true'
    runs-on: ubuntu-latest
    steps:
      - run: bun test
  mac:
    runs-on: macos-latest
    steps:
      - run: xcodebuild -version
  legs:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        node: [20, 22]
    steps:
      - run: node --version
`

const tryDeploy = `on:
  workflow_dispatch:
    inputs:
      target: {required: true, type: choice, options: [staging, production]}
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: ./deploy.sh}]
  changes:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
`

// tryCalls records what the seams were asked.
type tryCalls struct {
	added []string // key top sha
}

// fakeTry stands in for git and the mirror: the checkout is top, its tree is
// tc, and adding the commit to the mirror is recorded.
func fakeTry(t *testing.T, top string, tc treeCommit) *tryCalls {
	t.Helper()
	calls := &tryCalls{}
	prevTop, prevCommit, prevAdd := tryTopLevel, tryCommit, addTryCommit
	tryTopLevel = func(context.Context, string) (string, error) { return top, nil }
	tryCommit = func(context.Context, string) (treeCommit, error) { return tc, nil }
	addTryCommit = func(_ context.Context, _ *config.Config, key, dir, sha string) error {
		calls.added = append(calls.added, key+" "+dir+" "+sha)
		return nil
	}
	t.Cleanup(func() { tryTopLevel, tryCommit, addTryCommit = prevTop, prevCommit, prevAdd })
	return calls
}

func tryFixture(t *testing.T) (dir, repo string, calls *tryCalls) {
	t.Helper()
	dir = configDirWith(t, baseConfig)
	fakeStores(t, true)
	fakeOrigin(t, "git@github.com:me/app.git")
	repo = checkout(t, map[string]string{"checks.yml": tryChecks, "deploy.yml": tryDeploy})
	calls = fakeTry(t, repo, treeCommit{SHA: trySHA, Base: tryBase, Branch: "feature/x",
		Changed: []string{"M .github/workflows/checks.yml", "A new.ts"}})
	return dir, repo, calls
}

func tryRow(t *testing.T, dir, out string) store.Run {
	t.Helper()
	id := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
	return mustGetCLI(t, clientStore(t, dir), id)
}

func mustGetCLI(t *testing.T, db *store.DB, id string) store.Run {
	t.Helper()
	r, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %q: %v", id, err)
	}
	return r
}

// A try queues a workflow run's job pinned to the commit it made, approved for
// exactly that commit and marked local, with its needs taken as succeeded and
// its ref the checkout's branch. The commit is added to the mirror first.
func TestTry_QueuesALocalRunApprovedForItsCommit(t *testing.T) {
	dir, repo, calls := tryFixture(t)
	out, err := latemIn(t, dir, "", "try", "beacon", "--detach")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := tryRow(t, dir, out)
	w := r.Workflow
	if !r.IsLocal() || r.ApprovedSHA != trySHA || r.ApprovedAt == nil {
		t.Fatalf("local=%v approved=%q", r.IsLocal(), r.ApprovedSHA)
	}
	if r.Repo != "app" || r.Job != "beacon" || r.Ref != "refs/heads/feature/x" || r.OriginKind != "cli" ||
		r.State != protocol.StateQueued {
		t.Errorf("row %+v", r)
	}
	if w.Workflow != ".github/workflows/checks.yml" || w.SHA != trySHA || w.WorkflowRunID != r.ID || w.RunNumber != 0 {
		t.Errorf("workflow %+v", w)
	}
	if w.Event.Name != "push" || w.Event.Ref != "refs/heads/feature/x" || w.Event.Before != tryBase {
		t.Errorf("event %+v", w.Event)
	}
	var payload map[string]any
	if err := json.Unmarshal(w.Event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["after"] != trySHA || payload["before"] != tryBase || payload["head_commit"] != nil ||
		payload["repository"].(map[string]any)["full_name"] != "me/app" {
		t.Errorf("payload %v: what latem knows, and nothing it would have to invent", payload)
	}
	if n, ok := w.Needs["changes"]; !ok || n.Result != protocol.NeedSuccess || len(w.Needs) != 1 {
		t.Errorf("needs %+v", w.Needs)
	}
	if len(calls.added) != 1 || calls.added[0] != "app "+repo+" "+trySHA {
		t.Errorf("added to the mirror: %v", calls.added)
	}
	queued, err := clientStore(t, dir).ClaimNext(context.Background(), nil)
	if err != nil || queued.ID != r.ID {
		t.Errorf("the run is not in the queue: %+v, %v", queued.ID, err)
	}
	for _, want := range []string{"try checks.yml › beacon at 111111111111, as a push on refs/heads/feature/x",
		"2 path(s) differ from HEAD", "M .github/workflows/checks.yml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}
}

// Without --detach a try follows the run, so with no agent there is nothing to
// follow: exit 3, and nothing queued or added to the mirror.
func TestTry_NeedsAnAgentUnlessDetached(t *testing.T) {
	dir, _, calls := tryFixture(t)
	out, err := latemIn(t, dir, "", "try", "beacon")
	if exitCode(err) != ExitAgentDown {
		t.Fatalf("exit %d: %v\n%s", exitCode(err), err, out)
	}
	if len(calls.added) != 0 {
		t.Error("the commit was added to the mirror for a run that was never queued")
	}
	if runs, _ := clientStore(t, dir).List(context.Background(), store.Filter{}); len(runs) != 0 {
		t.Errorf("%d run(s) queued", len(runs))
	}
}

// A job no runner here takes is refused before anything is queued, with the
// findings `workflows check` gives for it.
func TestTry_RefusesAJobThisMachineCannotRun(t *testing.T) {
	dir, _, calls := tryFixture(t)
	out, err := latemIn(t, dir, "", "try", "mac", "--detach")
	if exitCode(err) != ExitFailure || !strings.Contains(err.Error(), "cannot run mac") {
		t.Fatalf("exit %d: %v\n%s", exitCode(err), err, out)
	}
	if !strings.Contains(out, "macos") {
		t.Errorf("the refusal does not say which label is missing:\n%s", out)
	}
	if len(calls.added) != 0 {
		t.Error("a refused job's commit was added to the mirror")
	}
}

// The workflow is the one with the job; two with it need --workflow, and a job
// nobody has is refused with what each file has.
func TestTry_FindsTheJobsWorkflow(t *testing.T) {
	dir, _, _ := tryFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"changes"}, "is in .github/workflows/checks.yml and .github/workflows/deploy.yml"},
		{[]string{"nope"}, ".github/workflows/checks.yml: beacon, changes, legs, mac"},
		{nil, "name a job to try"},
		{[]string{"beacon", "--workflow", "release"}, "names 0 of this checkout's workflows"},
	} {
		out, err := latemIn(t, dir, "", append([]string{"try", "--detach"}, tc.args...)...)
		if exitCode(err) != ExitUsage || !strings.Contains(out+err.Error(), tc.want) {
			t.Errorf("%v: exit %d, want usage naming %q: %v", tc.args, exitCode(err), tc.want, err)
		}
	}
	out, err := latemIn(t, dir, "", "try", "--detach", "changes", "--workflow", "checks")
	if err != nil {
		t.Fatalf("--workflow checks: %v\n%s", err, out)
	}
	if r := tryRow(t, dir, out); r.Workflow.Workflow != ".github/workflows/checks.yml" {
		t.Errorf("ran %s", r.Workflow.Workflow)
	}
}

// A called workflow's job is tried by its job path, with its caller's needs
// taken as succeeded; the caller itself names its jobs (A82).
func TestTry_AJobOfACalledWorkflow(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeStores(t, true)
	fakeOrigin(t, "git@github.com:me/app.git")
	repo := checkout(t, map[string]string{
		"checks.yml": tryChecks + `  package:
    needs: changes
    uses: ./.github/workflows/pack.yml
    with:
      beacon: ${{ needs.changes.outputs.beacon }}
`,
		"pack.yml": `on:
  workflow_call:
    inputs:
      beacon: {type: string, default: ''}
jobs:
  pack:
    runs-on: ubuntu-latest
    steps: [{run: npm pack}]
  smoke:
    needs: pack
    runs-on: ubuntu-latest
    steps: [{run: ./smoke.sh}]
`,
	})
	fakeTry(t, repo, treeCommit{SHA: trySHA, Base: tryBase, Branch: "feature/x"})

	out, err := latemIn(t, dir, "", "try", "package/smoke", "--detach")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := tryRow(t, dir, out)
	if r.Job != "package/smoke" || r.Workflow.Workflow != ".github/workflows/checks.yml" {
		t.Errorf("row %+v", r)
	}
	if len(r.Workflow.Needs) != 2 || r.Workflow.Needs["changes"].Result != protocol.NeedSuccess ||
		r.Workflow.Needs["package/pack"].Result != protocol.NeedSuccess {
		t.Errorf("needs %+v", r.Workflow.Needs)
	}
	if !strings.Contains(out, "package/smoke (package / smoke)") {
		t.Errorf("the summary does not name the job:\n%s", out)
	}
	out, err = latemIn(t, dir, "", "try", "package", "--detach")
	if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "package/pack, package/smoke") {
		t.Errorf("trying the caller: exit %d: %v\n%s", exitCode(err), err, out)
	}
}

// A matrix job with more than one leg names the flags that pick each, and
// --matrix picks the leg the plan made.
func TestTry_PicksAMatrixLeg(t *testing.T) {
	dir, _, _ := tryFixture(t)
	out, err := latemIn(t, dir, "", "try", "--detach", "legs")
	if exitCode(err) != ExitUsage || !strings.Contains(err.Error(), "--matrix node=20") ||
		!strings.Contains(err.Error(), "--matrix node=22") {
		t.Fatalf("exit %d: %v\n%s", exitCode(err), err, out)
	}
	out, err = latemIn(t, dir, "", "try", "--detach", "legs", "--matrix", "node=22")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if r := tryRow(t, dir, out); string(r.Workflow.Matrix) != `{"node":22}` {
		t.Errorf("matrix %s", r.Workflow.Matrix)
	}
}

// A dispatch's inputs are validated against the file and travel as a control
// plane's would; an input without --event workflow_dispatch is a usage error.
func TestTry_ADispatchCarriesItsInputs(t *testing.T) {
	dir, _, _ := tryFixture(t)
	if _, err := latemIn(t, dir, "", "try", "--detach", "deploy", "--input", "target=staging"); exitCode(err) != ExitUsage {
		t.Errorf("an input on a push: exit %d, %v", exitCode(err), err)
	}
	if _, err := latemIn(t, dir, "", "try", "--detach", "deploy", "--event", "workflow_dispatch",
		"--input", "target=nowhere"); err == nil || !strings.Contains(err.Error(), "staging, production") {
		t.Errorf("an input the file refuses: %v", err)
	}
	out, err := latemIn(t, dir, "", "try", "--detach", "deploy", "--event", "workflow_dispatch", "--input", "target=staging")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := tryRow(t, dir, out)
	if r.Inputs["target"] != "staging" || r.Workflow.Event.Name != "workflow_dispatch" ||
		r.Workflow.Event.Workflow != ".github/workflows/deploy.yml" || r.Workflow.Event.Before != "" {
		t.Errorf("inputs %v, event %+v", r.Inputs, r.Workflow.Event)
	}
}

// A checkout this machine has no [repos.*] entry for cannot be tried.
func TestTry_RefusesARepositoryNotConfiguredHere(t *testing.T) {
	dir, _, calls := tryFixture(t)
	fakeOrigin(t, "git@github.com:someone/else.git")
	_, err := latemIn(t, dir, "", "try", "--detach", "beacon")
	if err == nil || !strings.Contains(err.Error(), "latem repo add someone/else") {
		t.Fatalf("got %v", err)
	}
	if len(calls.added) != 0 {
		t.Error("an unconfigured repository's commit was added to a mirror")
	}
}

func TestTryRef(t *testing.T) {
	for _, tc := range []struct {
		flag, branch, want, note string
		bad                      bool
	}{
		{branch: "feature/x", want: "refs/heads/feature/x"},
		{flag: "release/1", branch: "feature/x", want: "refs/heads/release/1"},
		{flag: "refs/tags/v1.2.3", want: "refs/tags/v1.2.3"},
		{branch: "", want: "refs/heads/main", note: "HEAD is detached"},
		{flag: "refs/pull/1/merge", bad: true},
		{flag: "--upload-pack=x", bad: true},
	} {
		ref, note, err := tryRef(tc.flag, tc.branch, "main")
		if tc.bad {
			if err == nil {
				t.Errorf("%q: accepted as %s", tc.flag, ref)
			}
			continue
		}
		if err != nil || ref != tc.want || !strings.Contains(note, tc.note) {
			t.Errorf("%q on %q: %s %q %v", tc.flag, tc.branch, ref, note, err)
		}
	}
}

// commitWorkingTree makes the commit `git add -A && git commit` would — an
// untracked file in, an ignored one out — and leaves the index, the branch and
// the working tree as they were. It signs nothing whatever the checkout's
// config says, and a clean tree is HEAD. Git here has no config but the
// repository's own.
func TestCommitWorkingTree(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "latem test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@latem.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "latem test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@latem.invalid")
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "feature/x")
	write("README.md", "hello\n")
	write(".gitignore", "*.log\n")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	head := git("rev-parse", "HEAD")
	git("config", "commit.gpgsign", "true")
	git("config", "gpg.program", filepath.Join(dir, "no-such-gpg"))

	clean, err := commitWorkingTree(context.Background(), dir)
	if err != nil || clean.SHA != head || clean.Base != head || clean.Branch != "feature/x" || len(clean.Changed) != 0 {
		t.Fatalf("a clean tree: %+v, %v", clean, err)
	}

	write("README.md", "hello, again\n")
	write(".github/workflows/new.yml", "on: push\n")
	write("build.log", "ignored\n")
	git("add", "README.md")
	statusBefore := git("status", "--porcelain")

	tc, err := commitWorkingTree(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if tc.SHA == head || tc.Base != head {
		t.Fatalf("%+v", tc)
	}
	if got := git("ls-tree", "-r", "--name-only", tc.SHA); got != ".github/workflows/new.yml\n.gitignore\nREADME.md" {
		t.Errorf("the commit holds:\n%s", got)
	}
	if got := git("rev-parse", tc.SHA+"^"); got != head {
		t.Errorf("parent %s, want HEAD %s", got, head)
	}
	if got := strings.Join(tc.Changed, "|"); got != "A .github/workflows/new.yml|M README.md" {
		t.Errorf("changed %q", got)
	}
	if git("rev-parse", "HEAD") != head || git("status", "--porcelain") != statusBefore {
		t.Error("making the commit moved HEAD or changed the index or the working tree")
	}
	if got := git("stash", "list"); got != "" {
		t.Errorf("the stash is not empty: %s", got)
	}
}
