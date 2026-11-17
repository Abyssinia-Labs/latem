package run

import (
	"context"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeVault is OpenVault's variables as the run reads them (A96).
type fakeVault struct {
	vars  map[string]secrets.Variables // collection -> what it holds
	err   error
	asked int
}

func (f *fakeVault) Variables(_ context.Context, collection string, _ func(string) bool) (secrets.Variables, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vars[collection]
	if !ok {
		return nil, secrets.ErrNoCollection
	}
	return v, nil
}

const varsDeploy = `
on: [workflow_dispatch]
jobs:
  deploy:
    runs-on: [linux]
    environment: production
    steps:
      - run: ./deploy.sh ${{ vars.ACCOUNT }} ${{ vars.REGION }}
`

const noVarsJob = `
on: [workflow_dispatch]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - run: make
`

func vaultHarness(t *testing.T, vault *fakeVault, on bool) *harness {
	t.Helper()
	h := newHarness(t)
	h.source.registry = ""
	h.cfg.Secrets.Vault = on
	h.cfg.Repos["app"].Vars = map[string]string{"ACCOUNT": "from-config", "REGION": "us"}
	h.opts.Vault = vault
	return h
}

func (h *harness) executeWorkflow(t *testing.T, o *Orchestrator, file, body string) Result {
	t.Helper()
	req := h.request("deploy")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/"+file, body)
	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// A96: a workflow job's variables come from OpenVault first, then
// config.toml, and an environment's entry over the repository's.
func TestExecute_AJobsVariablesComeFromOpenVaultBeforeConfig(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {
		"":           {"ACCOUNT": "from-vault"},
		"production": {"REGION": "eu"},
	}}}
	h := vaultHarness(t, vault, true)
	o := h.build()
	res := h.executeWorkflow(t, o, "deploy.yml", varsDeploy)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	env := h.env.executed()[0].Env
	if env["GHA_EXPR_1"] != "from-vault" || env["GHA_EXPR_2"] != "eu" {
		t.Errorf("vars.ACCOUNT = %q, vars.REGION = %q; want the vault's", env["GHA_EXPR_1"], env["GHA_EXPR_2"])
	}
	if vault.asked != 1 {
		t.Errorf("the vault was asked %d times for one job, want once", vault.asked)
	}
}

// A96: a machine that does not say `[secrets] openvault = true` never asks
// OpenVault for a variable, even with a vault here.
func TestExecute_VariablesAreConfigsUnlessTheMachineReadsOpenVault(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"ACCOUNT": "from-vault"}}}}
	h := vaultHarness(t, vault, false)
	o := h.build()
	res := h.executeWorkflow(t, o, "deploy.yml", varsDeploy)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if got := h.env.executed()[0].Env["GHA_EXPR_1"]; got != "from-config" {
		t.Errorf("vars.ACCOUNT = %q, want config.toml's", got)
	}
	if vault.asked != 0 {
		t.Errorf("the vault was asked %d times", vault.asked)
	}
}

// A96: a job that reads a variable while OpenVault does not answer fails
// with E_SECRET_BACKEND, retryable, before anything runs. It never runs with
// the variable empty, nor with config.toml's stale copy.
func TestExecute_AJobThatReadsAVariableFailsRetryablyWhileTheVaultIsDown(t *testing.T) {
	h := vaultHarness(t, &fakeVault{err: stoppedVault}, true)
	o := h.build()
	res := h.executeWorkflow(t, o, "deploy.yml", varsDeploy)
	if res.ErrorCode != protocol.ErrSecretBackend || res.State != protocol.StateFailed || !res.ErrorCode.Retryable() {
		t.Fatalf("got %s/%s (%s), want failed with a retryable E_SECRET_BACKEND", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if !strings.Contains(res.ErrorMsg, "vault service") || !strings.Contains(res.ErrorMsg, "variables") {
		t.Errorf("message = %q, want what could not be read and the command that starts the service", res.ErrorMsg)
	}
	if strings.Contains(res.ErrorMsg, "service.sock") {
		t.Errorf("message = %q names a local path, and it reaches a broker", res.ErrorMsg)
	}
	if len(h.env.executed()) > 0 || h.exec.prepared > 0 {
		t.Error("the job's environment was prepared, or a step ran")
	}
}

// A96: a job that reads no variable does not wait on OpenVault, and runs.
func TestExecute_AJobThatReadsNoVariableRunsWhileTheVaultIsDown(t *testing.T) {
	vault := &fakeVault{err: stoppedVault}
	h := vaultHarness(t, vault, true)
	o := h.build()
	res := h.executeWorkflow(t, o, "build.yml", noVarsJob)
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s (%s), want succeeded", res.State, res.ErrorCode, res.ErrorMsg)
	}
	if vault.asked != 0 {
		t.Errorf("the vault was asked %d times by a job that reads no variable", vault.asked)
	}
}
