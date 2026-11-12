package run

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/workflow"
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

// partsStore is a store keyed by a scope's parts, as OpenVault is (A86).
type partsStore struct {
	values map[string]string // parts and name joined by "/"
	asked  []string
}

func (s *partsStore) Name() string { return "parts" }

func (s *partsStore) Resolve(ctx context.Context, name string) (secrets.Value, error) {
	return s.ResolveIn(ctx, nil, name)
}

func (s *partsStore) ResolveIn(_ context.Context, scope []string, name string) (secrets.Value, error) {
	key := strings.Join(append(slices.Clone(scope), name), "/")
	s.asked = append(s.asked, key)
	if v, ok := s.values[key]; ok {
		return secrets.Value(v), nil
	}
	return nil, secrets.ErrNotFound
}

// A86: a workflow job's lookup reaches a store keyed by scope with the
// repository and the environment as they are, not flattened into one name,
// and a flat store behind it still gets the flat name.
func TestExecute_WorkflowSecretsReachAScopedStoreAsTheirParts(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	vault := &partsStore{values: map[string]string{"app/production/DEPLOY_KEY": "deploy key from the vault"}}
	h.secret.values = map[string]string{"app__NPM_TOKEN": "npm token from the file"}
	h.provider = secrets.NewChain(vault, h.secret)
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
      - run: ./deploy.sh
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
          NPM_TOKEN: ${{ secrets.NPM_TOKEN }}
`)
	res, err := o.Execute(t.Context(), r)
	if err != nil || res.State != protocol.StateSucceeded {
		t.Fatalf("Execute = %s/%s (%s), %v; want succeeded", res.State, res.ErrorCode, res.ErrorMsg, err)
	}
	env := h.env.executed()[0].Env
	if env["DEPLOY_KEY"] != "deploy key from the vault" || env["NPM_TOKEN"] != "npm token from the file" {
		t.Errorf("DEPLOY_KEY = %q, NPM_TOKEN = %q", env["DEPLOY_KEY"], env["NPM_TOKEN"])
	}
	want := []string{"app/production/DEPLOY_KEY", "app/production/NPM_TOKEN", "app/NPM_TOKEN"}
	if !slices.Equal(vault.asked, want) {
		t.Errorf("the scoped store was asked %q, want %q", vault.asked, want)
	}
}

// fileStore is a scoped store that holds files too, as OpenVault does (A88).
type fileStore struct {
	*partsStore
	files map[string]secrets.File
}

func (s *fileStore) ResolveFileIn(_ context.Context, scope []string, name string) (secrets.File, error) {
	if f, ok := s.files[strings.Join(append(slices.Clone(scope), name), "/")]; ok {
		return secrets.File{Name: f.Name, Data: append(secrets.Value(nil), f.Data...)}, nil
	}
	return secrets.File{}, secrets.ErrNotFound
}

// fileJob is a workflow job that reads a keystore, an OpenVault file, and its
// password, a secret.
const fileJob = `
on: [workflow_dispatch]
jobs:
  build:
    runs-on: [linux]
    environment: production
    steps:
      - run: ./build.sh
        env:
          UPLOAD_JKS: ${{ secrets.UPLOAD_JKS }}
          KEYSTORE_PASSWORD: ${{ secrets.KEYSTORE_PASSWORD }}
`

func fileHarness(t *testing.T, executor string) *harness {
	h := newHarness(t)
	h.source.registry = ""
	h.exec.name = executor
	h.provider = secrets.NewChain(&fileStore{
		partsStore: &partsStore{values: map[string]string{"app/production/KEYSTORE_PASSWORD": "a password"}},
		files: map[string]secrets.File{
			"app/production/UPLOAD_JKS": {Name: "upload.jks", Data: secrets.Value("\x00\xfekeystore\x00bytes")},
		},
	}, h.secret)
	return h
}

// A88: a secret that is an OpenVault file reaches a step as the path of a
// copy in the job's temporary directory, written before the first step, and
// never as its bytes.
func TestExecute_AFileSecretReachesAStepAsThePathOfItsCopy(t *testing.T) {
	h := fileHarness(t, "native")
	o := h.build()
	req := h.request("build")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/build.yml", fileJob)
	res, err := o.Execute(t.Context(), r)
	if err != nil || res.State != protocol.StateSucceeded {
		t.Fatalf("Execute = %s/%s (%s), %v; want succeeded", res.State, res.ErrorCode, res.ErrorMsg, err)
	}
	env := h.env.executed()[0].Env
	if env["UPLOAD_JKS"] != "/tmp/latem/temp/latem-secret-upload.jks" || env["KEYSTORE_PASSWORD"] != "a password" {
		t.Errorf("UPLOAD_JKS = %q, KEYSTORE_PASSWORD = %q", env["UPLOAD_JKS"], env["KEYSTORE_PASSWORD"])
	}
	h.env.mu.Lock()
	copied := string(h.env.puts["latem-secret-upload.jks"])
	h.env.mu.Unlock()
	if copied != "\x00\xfekeystore\x00bytes" {
		t.Errorf("the copy holds %q", copied)
	}
}

// A88: a runner whose temporary files other users can read gets no file
// secret: the job fails before its environment starts, saying why.
func TestExecute_AFileSecretIsRefusedWhereItsCopyWouldNotBePrivate(t *testing.T) {
	h := fileHarness(t, "docker")
	o := h.build()
	req := h.request("build")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/build.yml", fileJob)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorCode != protocol.ErrMissingSecret || !strings.Contains(res.ErrorMsg, "native runner") {
		t.Errorf("Execute = %s/%s (%s); want E_MISSING_SECRET naming the native runner", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if len(h.env.executed()) > 0 || h.exec.prepared > 0 {
		t.Error("the job's environment was prepared, or a step ran")
	}
}

// A51: a workflow job gets its runner's tool cache — a volume for docker, a
// directory under the agent's state on the host — and a jobs.yaml job none.
func TestToolCacheIsTheRunners(t *testing.T) {
	cfg := &config.Config{Paths: config.Paths{Dir: "/home/ci/.latem"}}
	for kind, want := range map[string]string{
		"docker": "latem-toolcache-mini",
		"native": "/home/ci/.latem/toolcache/mini",
		"fake":   "",
	} {
		x := &execution{
			o:        &Orchestrator{cfg: cfg},
			workflow: &workflow.Source{Path: ".github/workflows/ci.yml"},
			runner:   &Runner{Name: "mini", Executor: &fakeExecutor{name: kind}},
		}
		if got := x.toolCache(); got != want {
			t.Errorf("%s: tool cache = %q, want %q", kind, got, want)
		}
		x.workflow = nil
		if got := x.toolCache(); got != "" {
			t.Errorf("%s: a jobs.yaml job got a tool cache %q", kind, got)
		}
	}
}
