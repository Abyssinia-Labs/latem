package agent

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/repovars"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeVault is OpenVault's variables as the agent reads them (A96).
type fakeVault struct {
	mu    sync.Mutex
	vars  map[string]secrets.Variables // collection -> what it holds
	err   error
	asked int
}

func (f *fakeVault) Variables(_ context.Context, collection string, envs func(string) bool) (secrets.Variables, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vars[collection]
	if !ok {
		return nil, secrets.ErrNoCollection
	}
	out := secrets.Variables{}
	for env, m := range v {
		if envs == nil || envs(env) {
			out[env] = maps.Clone(m)
		}
	}
	return out, nil
}

func (f *fakeVault) set(vars map[string]secrets.Variables, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vars, f.err = vars, err
}

var vaultDown = &secrets.OpenVaultError{Op: "list", NotRunning: true}

// vaultAgent is an agent, not started, that reads its variables from vault.
func vaultAgent(t *testing.T, vault *fakeVault) (*Agent, *config.Config) {
	t.Helper()
	cfg := newConfigDir(t, configOptions{})
	cfg.Secrets.OpenVault = true
	cfg.Repos["app"].Vars = map[string]string{"LATEM": "false", "REGION": "us"}
	a, err := New(Options{Config: cfg, Logger: testLogger(t), Vault: vault})
	if err != nil {
		t.Fatal(err)
	}
	a.vault = vault
	return a, cfg
}

// A96: a heartbeat's digests are of the variables a job reads, the vault's
// over config.toml's, and an environment's are not among them.
func TestHeartbeatVars_AreTheVaultsOverConfigs(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {
		"":           {"LATEM": "true"},
		"production": {"LATEM": "an environment's, which no heartbeat carries"},
	}}}
	a, cfg := vaultAgent(t, vault)
	a.refreshVars(t.Context())

	got := a.heartbeatVars("app", cfg.Repos["app"])
	set := repovars.Of(cfg.Repos["app"], secrets.Variables{"": {"LATEM": "true"}})
	if want := varDigests(set.For("")); !maps.Equal(got, want) {
		t.Errorf("digests = %v, want those of what a job reads, %v", got, want)
	}
	if got["LATEM"] != varDigests(map[string]string{"X": "true"})["X"] {
		t.Error("LATEM's digest is not the vault's value's")
	}
}

// A96: a service that stops after it has answered changes nothing the
// heartbeat says. A blip must not set this machine apart from its peers.
func TestHeartbeatVars_AServiceThatStopsKeepsTheLastDigests(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"LATEM": "true"}}}}
	a, cfg := vaultAgent(t, vault)
	a.refreshVars(t.Context())
	before := a.heartbeatVars("app", cfg.Repos["app"])

	vault.set(nil, vaultDown)
	a.refreshVars(t.Context())
	if after := a.heartbeatVars("app", cfg.Repos["app"]); !maps.Equal(before, after) {
		t.Errorf("digests moved over a blip: %v, then %v", before, after)
	}
}

// A96: an agent that starts while the service does not answer yet says what
// the last one read, kept on disk as digests, rather than config.toml's
// answer alone.
func TestHeartbeatVars_ARestartWithTheServiceDownSaysWhatWasReadBefore(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"LATEM": "true"}}}}
	a, cfg := vaultAgent(t, vault)
	a.refreshVars(t.Context())
	before := a.heartbeatVars("app", cfg.Repos["app"])

	b, err := os.ReadFile(cfg.Paths.VarDigests)
	if err != nil {
		t.Fatalf("no digests kept: %v", err)
	}
	if want := varDigests(map[string]string{"X": "true"})["X"]; !strings.Contains(string(b), want) ||
		strings.Contains(string(b), "true\"") {
		t.Errorf("kept %s, want the digest and never the value", b)
	}
	if fi, err := os.Stat(cfg.Paths.VarDigests); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the file's mode is %v, want it the user's alone", fi.Mode())
	}

	restarted, err := New(Options{Config: cfg, Logger: testLogger(t), Vault: vault})
	if err != nil {
		t.Fatal(err)
	}
	restarted.vault = vault
	restarted.loadVarDigests()
	vault.set(nil, vaultDown)
	restarted.refreshVars(t.Context())
	if after := restarted.heartbeatVars("app", cfg.Repos["app"]); !maps.Equal(before, after) {
		t.Errorf("after a restart with the service down: %v, want %v", after, before)
	}
}

// A96: the digests are written when an answer changes, and only then.
func TestRefreshVars_WritesOnlyWhenAnAnswerChanged(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"LATEM": "true"}}}}
	a, cfg := vaultAgent(t, vault)
	a.refreshVars(t.Context())
	if err := os.Remove(cfg.Paths.VarDigests); err != nil {
		t.Fatal(err)
	}
	a.refreshVars(t.Context())
	if _, err := os.Stat(cfg.Paths.VarDigests); !os.IsNotExist(err) {
		t.Errorf("rewritten with nothing changed (%v)", err)
	}
	vault.set(map[string]secrets.Variables{"app": {"": {"LATEM": "false"}}}, nil)
	a.refreshVars(t.Context())
	if _, err := os.Stat(cfg.Paths.VarDigests); err != nil {
		t.Errorf("not written when a value changed: %v", err)
	}
}

// A96: a machine that does not read OpenVault says config.toml's digests and
// asks the vault nothing.
func TestHeartbeatVars_WithoutOpenVaultAreConfigs(t *testing.T) {
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"LATEM": "true"}}}}
	cfg := newConfigDir(t, configOptions{})
	cfg.Repos["app"].Vars = map[string]string{"LATEM": "false"}
	ta := newTestAgent(t, cfg, func(o *Options) { o.Vault = vault })
	ta.run(t)
	waitFor(t, "a heartbeat", 10*time.Second, func() bool {
		_, _, ok := ta.localBrk.Health()
		return ok
	})
	h, _, _ := ta.localBrk.Health()
	if got, want := h.Repos[0].Vars, varDigests(cfg.Repos["app"].Vars); !maps.Equal(got, want) {
		t.Errorf("digests = %v, want config.toml's", got)
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if vault.asked != 0 {
		t.Errorf("the vault was asked %d times", vault.asked)
	}
}

const planVars = `
on:
  push:
    branches: [main]
jobs:
  build:
    runs-on: ${{ vars.RUNNER }}
    steps: [{run: make}]
`

// A96: a plan reads its variables from the vault, and one it cannot read
// refuses the plan as a backend down, E_SECRET_BACKEND, not as a workflow
// that is wrong.
func TestAgent_PlansWithTheVaultsVariablesAndRefusesWhenItIsDown(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; a plan is read from a real mirror (INV-5)")
	}
	cp := newControlPlane(t)
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	path := filepath.Join(repo, ".github", "workflows", "build.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(planVars), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "workflows")
	sha := runGit(t, repo, "rev-parse", "HEAD")

	cfg := newConfigDir(t, configOptions{repoURL: repo, remoteURL: cp.URL()})
	cfg.Secrets.OpenVault = true
	cfg.Repos["app"].Vars = map[string]string{"RUNNER": "config-label"}
	vault := &fakeVault{vars: map[string]secrets.Variables{"app": {"": {"RUNNER": "vault-label"}}}}
	ta := newTestAgent(t, cfg, func(o *Options) { o.Vault = vault })
	ta.run(t)
	waitFor(t, "the agent to connect", 20*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		return len(cp.conns) > 0 && len(cp.resumes) > 0
	})

	ev := protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: sha}
	cp.push(t, protocol.NewPlanCommand(protocol.PlanRequest{ID: "up", Repo: "app", Event: ev}))
	up := waitForPlan(t, cp, "up")
	if up.Error != "" || len(up.Workflows) != 1 || len(up.Workflows[0].Jobs) != 1 ||
		len(up.Workflows[0].Jobs[0].Labels) != 1 || up.Workflows[0].Jobs[0].Labels[0] != "vault-label" {
		t.Fatalf("plan %+v, want build on the vault's label", up)
	}

	vault.set(nil, vaultDown)
	cp.push(t, protocol.NewPlanCommand(protocol.PlanRequest{ID: "down", Repo: "app", Event: ev}))
	down := waitForPlan(t, cp, "down")
	if down.ErrorCode != protocol.ErrSecretBackend || down.Workflows != nil {
		t.Errorf("plan %+v, want refused with E_SECRET_BACKEND", down)
	}
}

func waitForPlan(t *testing.T, cp *controlPlane, id string) protocol.PlanResult {
	t.Helper()
	var got protocol.PlanResult
	waitFor(t, "plan "+id, 60*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		for _, p := range cp.plans {
			if p.ID == id {
				got = p
				return true
			}
		}
		return false
	})
	return got
}
