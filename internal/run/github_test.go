package run

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const envWorkflow = `
name: Checks
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - name: build
        run: echo "${{ github.repository }}"
`

// A50: a job compiled from a workflow runs with GitHub's environment, and the
// repository is owner/name wherever a workflow can read it.
func TestExecute_WorkflowJobGetsGitHubsEnvironment(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.cfg.Repos["app"].GitHub = "acme/app"
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", envWorkflow)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s, want succeeded", res.State, res.ErrorCode)
	}

	env := h.env.runSpec().Env
	want := map[string]string{
		"CI":                      "true",
		"GITHUB_ACTIONS":          "true",
		"GITHUB_REPOSITORY":       "acme/app",
		"GITHUB_REPOSITORY_OWNER": "acme",
		"GITHUB_REF":              "refs/heads/main",
		"GITHUB_REF_NAME":         "main",
		"GITHUB_REF_TYPE":         "branch",
		"GITHUB_EVENT_NAME":       "workflow_dispatch",
		"GITHUB_ACTOR":            "tester",
		"GITHUB_JOB":              "test",
		"GITHUB_WORKFLOW":         "Checks",
		"GITHUB_RUN_ATTEMPT":      "1",
		"GITHUB_SERVER_URL":       "https://github.com",
		"GITHUB_API_URL":          "https://api.github.com",
		"RUNNER_OS":               "Linux",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	for _, k := range []string{"GITHUB_SHA", "GITHUB_RUN_ID"} {
		if env[k] == "" {
			t.Errorf("%s is not set", k)
		}
	}
	if env["GITHUB_SHA"] != env["LATEM_SHA"] {
		t.Errorf("GITHUB_SHA %q and LATEM_SHA %q disagree", env["GITHUB_SHA"], env["LATEM_SHA"])
	}

	// The expression reads the same owner/name, as data.
	if got := h.env.executed()[0].Env["GHA_EXPR_1"]; got != "acme/app" {
		t.Errorf("github.repository = %q, want acme/app", got)
	}
}

// jobs.yaml keeps §9's environment: none of GitHub's variables appear.
func TestExecute_NativeJobGetsNoGitHubEnvironment(t *testing.T) {
	h := newHarness(t)
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s, want succeeded", res.State, res.ErrorCode)
	}
	env := h.env.runSpec().Env
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_SHA", "RUNNER_OS"} {
		if _, set := env[k]; set {
			t.Errorf("%s is set for a jobs.yaml job", k)
		}
	}
}

// A URL that names no GitHub repository leaves the config key as the answer,
// as before A50, and the run log says how to fix it.
func TestExecute_RepositoryFallsBackToTheConfigKey(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", envWorkflow)

	if _, err := o.Execute(t.Context(), r); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	env := h.env.runSpec().Env
	if env["GITHUB_REPOSITORY"] != "app" {
		t.Errorf("GITHUB_REPOSITORY = %q, want the config key", env["GITHUB_REPOSITORY"])
	}
	if _, set := env["GITHUB_REPOSITORY_OWNER"]; set {
		t.Error("an owner was invented for a repository with none")
	}
	if !strings.Contains(h.logContents(req.ID), `set github = "owner/name"`) {
		t.Error("the run log should say how to name the repository")
	}
}

func TestRefParts(t *testing.T) {
	cases := map[string][2]string{
		"refs/heads/feature/x": {"feature/x", "branch"},
		"refs/tags/v1.2.3":     {"v1.2.3", "tag"},
		"refs/pull/1/head":     {"refs/pull/1/head", ""},
	}
	for ref, want := range cases {
		name, kind := refParts(ref)
		if name != want[0] || kind != want[1] {
			t.Errorf("refParts(%q) = (%q, %q), want (%q, %q)", ref, name, kind, want[0], want[1])
		}
	}
}

// A50 end to end: a workflow's secrets are found under the job's environment
// and repository before the machine-wide name, reach only the step that
// names them, and one set nowhere reads as empty with the log saying so.
// Variables come from the repository's config, by environment.
func TestExecute_WorkflowSecretsAndVars(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.secret.values = map[string]string{
		"app__production__DEPLOY_KEY": "production deploy key",
		"app__DEPLOY_KEY":             "repository deploy key",
		"CLOUDFLARE_API_TOKEN":        "machine-wide cloudflare token",
	}
	h.cfg.Repos["app"].Vars = map[string]string{"ACCOUNT": "acct-1"}
	o := h.build()

	req := h.request("deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/deploy.yml", `
on: [workflow_dispatch]
jobs:
  deploy:
    runs-on: [linux]
    environment: production
    steps:
      - name: deploy
        run: ./deploy.sh ${{ vars.ACCOUNT }}
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          AUTH: Bearer ${{ secrets.CLOUDFLARE_API_TOKEN }}
      - name: optional
        if: secrets.NOT_SET != ''
        run: never
      - name: plain
        run: make
`)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}

	steps := h.env.executed()
	if got := executedNames(steps); strings.Join(got, ",") != "deploy,plain" {
		t.Fatalf("executed %v — an unset secret should read as empty", got)
	}
	deploy := steps[0].Env
	if deploy["DEPLOY_KEY"] != "production deploy key" {
		t.Errorf("DEPLOY_KEY = %q, want the production environment's", deploy["DEPLOY_KEY"])
	}
	if deploy["AUTH"] != "Bearer machine-wide cloudflare token" {
		t.Errorf("AUTH = %q, want the machine-wide token rendered into the template", deploy["AUTH"])
	}
	if deploy["GHA_EXPR_1"] != "acct-1" {
		t.Errorf("vars.ACCOUNT = %q", deploy["GHA_EXPR_1"])
	}

	// Only the step that named a secret has it.
	for _, k := range []string{"DEPLOY_KEY", "AUTH"} {
		if _, set := steps[1].Env[k]; set {
			t.Errorf("%s reached a step that never named it", k)
		}
	}
	for k, v := range h.env.runSpec().Env {
		if strings.Contains(v, "deploy key") || strings.Contains(v, "cloudflare token") {
			t.Errorf("a secret is in the run-wide environment as %s", k)
		}
	}

	log := h.logContents(req.ID)
	if !strings.Contains(log, "secret NOT_SET is not set on this machine") ||
		!strings.Contains(log, "app__production__NOT_SET") {
		t.Errorf("the log should name the unset secret and where it looked:\n%s", log)
	}
	if strings.Contains(log, "production deploy key") {
		t.Error("a secret value reached the log")
	}
}
