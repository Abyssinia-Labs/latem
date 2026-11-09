package run

import (
	"context"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const prWorkflowPath = ".github/workflows/pr.yml"

// prWorkflow reads one repository secret, as a pull request's checks often do.
const prWorkflow = `
on: [pull_request]
jobs:
  check:
    runs-on: [linux]
    steps:
      - run: ./check.sh
        env:
          NPM_TOKEN: ${{ secrets.NPM_TOKEN }}
`

// prWorkflowAsksForProduction is prWorkflow as a pull request would edit it
// to reach production's secrets: the job names the environment itself.
const prWorkflowAsksForProduction = `
on: [pull_request]
jobs:
  check:
    runs-on: [linux]
    environment: production
    steps:
      - run: ./check.sh
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
`

const prWorkflowNoSecrets = `
on: [pull_request]
jobs:
  check:
    runs-on: [linux]
    steps:
      - run: ./check.sh
`

// pullRequestHarness is a repository configured as `latem repo add` writes
// one, so pull request 7's merge ref is in policy, with a store holding a
// repository secret and a production one.
func pullRequestHarness(t *testing.T, executorName string) *harness {
	t.Helper()
	h := newHarness(t)
	h.source.registry = ""
	h.source.refs = map[string]bool{"refs/heads/main": true, "refs/pull/7/merge": true}
	h.cfg.Repos["app"].AllowedRefs = []string{"main", "refs/pull/*/merge"}
	h.exec.name = executorName
	h.secret.values = map[string]string{
		"app__NPM_TOKEN":              "npm token of the repository",
		"app__production__DEPLOY_KEY": "production deploy key",
	}
	return h
}

// prRequest is pull request 7's job as the control plane sends it. Whether it
// comes from a fork is in the event's payload, which the control plane relays
// and nothing on the machine can check.
func prRequest(h *harness, id string, fork bool) protocol.RunRequest {
	req := workflowRequest(h, "check")
	req.ID = id
	req.Workflow = prWorkflowPath
	req.Ref = "refs/pull/7/merge"
	req.Needs = nil
	head := `{"repo":{"full_name":"me/app","fork":false}}`
	if fork {
		head = `{"repo":{"full_name":"stranger/app","fork":true}}`
	}
	req.Event = &protocol.Event{
		Name: "pull_request", Action: "synchronize", Ref: req.Ref, SHA: req.SHA,
		BaseRef: "main", HeadRef: "feature", Actor: "contributor",
		Payload: []byte(`{"pull_request":{"head":` + head + `,"base":{"repo":{"full_name":"me/app"}}}}`),
	}
	return req
}

// held asserts that a pull request's run waited for approval and that nothing
// of the machine's was read for it: no store asked for any name, no
// environment prepared, no step run.
func held(t *testing.T, h *harness, res Result, wantInMsg ...string) {
	t.Helper()
	if res.State != protocol.StateAwaitingApproval {
		t.Fatalf("state = %s/%s (%s), want awaiting_approval", res.State, res.ErrorCode, res.ErrorMsg)
	}
	for _, want := range append(wantInMsg, "latem approve "+res.RunID) {
		if !strings.Contains(res.ErrorMsg, want) {
			t.Errorf("message %q does not say %q", res.ErrorMsg, want)
		}
	}
	if asked := h.secret.lookedUp(); len(asked) > 0 {
		t.Errorf("the secret store was asked for %q before anyone approved the commit", asked)
	}
	if h.exec.prepared != 0 || len(h.env.executed()) != 0 {
		t.Errorf("prepared %d environments and ran %d steps for an unapproved pull request",
			h.exec.prepared, len(h.env.executed()))
	}
}

func runPullRequest(t *testing.T, h *harness, o *Orchestrator, req protocol.RunRequest, workflowFile string) Result {
	t.Helper()
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, prWorkflowPath, workflowFile)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// A85: a fork's pull request reads none of the machine's secrets until a
// person on the machine approves its commit.
func TestPullRequest_AForksJobGetsNoSecretsUntilItsCommitIsApproved(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	o := h.build()
	res := runPullRequest(t, h, o, prRequest(h, "run1", true), prWorkflow)
	held(t, h, res, "asks for secrets NPM_TOKEN")
	if got := mustGet(t, h.db, "run1"); got.SHA != h.source.sha {
		t.Errorf("parked at %q, want the commit an approver is shown, %q", got.SHA, h.source.sha)
	}
}

// A pull request from a branch of the repository is held the same way. That
// it is not a fork is the payload's claim, and the payload is the control
// plane's; the machine decides from the ref alone.
func TestPullRequest_ASameRepositoryPullRequestIsHeldToo(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	o := h.build()
	res := runPullRequest(t, h, o, prRequest(h, "run1", false), prWorkflow)
	held(t, h, res, "asks for secrets NPM_TOKEN")
}

// A pull request that edits its own job to name `environment: production`
// reaches none of production's secrets: it is held, and the approver is told
// which environment they would be giving it.
func TestPullRequest_EditingItsJobToAskForProductionGetsItNothing(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	o := h.build()
	res := runPullRequest(t, h, o, prRequest(h, "run1", true), prWorkflowAsksForProduction)
	held(t, h, res, "asks for secrets DEPLOY_KEY of environment production")
}

// The allowed path, end to end: held, approved for its commit on the machine,
// built with its secrets. A push to the pull request is a new commit and is
// held again; the first approval does not reach it.
func TestPullRequest_AnApprovedCommitGetsItsSecretsAndTheNextPushIsAskedAgain(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	o := h.build()
	held(t, h, runPullRequest(t, h, o, prRequest(h, "run1", true), prWorkflow), "NPM_TOKEN")

	approved, err := o.Approve(t.Context(), "run1")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ApprovedSHA != h.source.sha {
		t.Fatalf("approved %q, want the pull request's commit %q", approved.ApprovedSHA, h.source.sha)
	}
	// The park tore the workspace down; the next attempt checks it out again.
	h.writeWorkspaceFile("run1", prWorkflowPath, prWorkflow)
	res := h.rerun(o, "run1")
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	steps := h.env.executed()
	if len(steps) != 1 || steps[0].Env["NPM_TOKEN"] != "npm token of the repository" {
		t.Fatalf("the approved job's step did not get its secret: %+v", steps)
	}
	if log := h.logContents("run1"); !strings.Contains(log, "approved on this machine") {
		t.Errorf("the log does not say the commit was approved:\n%s", log)
	}

	// The contributor pushes again.
	const next = "5555555555555555555555555555555555555555"
	h.source.commits = map[string]bool{next: false}
	req := prRequest(h, "run2", true)
	req.SHA, req.Event.SHA = next, next
	before := len(h.secret.lookedUp())
	res = runPullRequest(t, h, o, req, prWorkflow)
	if res.State != protocol.StateAwaitingApproval {
		t.Fatalf("the next push: state = %s (%s), want awaiting_approval", res.State, res.ErrorMsg)
	}
	if asked := h.secret.lookedUp()[before:]; len(asked) > 0 {
		t.Errorf("the next push's commit was given secrets %q on the first one's approval", asked)
	}
}

// A pull request's job that asks for nothing of the machine, on a runner that
// confines it, builds unapproved, as it did before A85: its checks still run.
func TestPullRequest_WithNoSecretsOnAnIsolatingRunnerBuildsAsBefore(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	o := h.build()
	res := runPullRequest(t, h, o, prRequest(h, "run1", true), prWorkflowNoSecrets)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if asked := h.secret.lookedUp(); len(asked) > 0 {
		t.Errorf("the store was asked for %q", asked)
	}
}

// On a native runner a step runs as the agent's own user, where secrets.env,
// the Keychain and OpenVault's socket are all in reach. Declaring no secrets
// withholds nothing there, so the job is held anyway (§13.0).
func TestPullRequest_OnARunnerThatDoesNotIsolateItIsHeldEvenWithoutSecrets(t *testing.T) {
	h := pullRequestHarness(t, "native")
	o := h.build()
	res := runPullRequest(t, h, o, prRequest(h, "run1", true), prWorkflowNoSecrets)
	held(t, h, res, `would run on runner "docker-1" as this machine's own user`)
	if strings.Contains(res.ErrorMsg, "asks for secrets") {
		t.Errorf("message %q names secrets the job did not ask for", res.ErrorMsg)
	}
}

// A jobs.yaml job run at a pull request's ref is the pull request's code too.
func TestPullRequest_AJobsYamlJobAtAPullRequestsRefIsHeld(t *testing.T) {
	h := pullRequestHarness(t, "docker")
	h.source.registry = defaultRegistryYAML
	o := h.build()
	req := h.request("secretive")
	req.Ref = "refs/pull/7/merge"
	held(t, h, h.run(o, req), "asks for secrets MATCH_PASSWORD")
}

// The rule is about pull requests. A push to the default branch, on a native
// runner and with secrets, builds as it always has.
func TestPullRequest_APushIsNotHeld(t *testing.T) {
	h := pullRequestHarness(t, "native")
	o := h.build()
	req := workflowRequest(h, "check")
	req.Workflow, req.Needs = prWorkflowPath, nil
	res := runPullRequest(t, h, o, req, strings.Replace(prWorkflow, "pull_request", "push", 1))
	if res.State != protocol.StateSucceeded {
		t.Fatalf("state = %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if steps := h.env.executed(); len(steps) != 1 || steps[0].Env["NPM_TOKEN"] == "" {
		t.Errorf("a push's step did not get its secret: %+v", steps)
	}
}

// The rule also stands where the values are read, so a path that reached
// resolution without passing the hold still resolves nothing.
func TestPullRequest_ResolutionItselfRefusesAnUnapprovedPullRequest(t *testing.T) {
	sec := &fakeSecrets{values: map[string]string{"MATCH_PASSWORD": "a value"}}
	x := &execution{
		o:             &Orchestrator{secrets: sec},
		run:           store.Run{ID: "run1"},
		refNormalized: "refs/pull/7/merge",
		sha:           "0123456789abcdef0123456789abcdef01234567",
		resolved:      &registry.Resolved{Secrets: []string{"MATCH_PASSWORD"}},
	}
	out, ok := x.resolveSecrets(context.Background())
	if ok || out.code != protocol.ErrSecretBackend || !strings.Contains(out.msg, "approved on this machine") {
		t.Fatalf("resolveSecrets = %+v, %v; want a refusal", out, ok)
	}
	if asked := sec.lookedUp(); len(asked) > 0 {
		t.Errorf("the store was asked for %q", asked)
	}
}
