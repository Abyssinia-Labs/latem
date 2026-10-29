package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const planChecks = `
name: Checks
on:
  push:
    branches: [main]
concurrency: checks-${{ github.ref }}
jobs:
  lint:
    runs-on: ubuntu-latest
    steps: [{run: make lint}]
  web:
    needs: lint
    runs-on: ubuntu-latest
    steps: [{run: make build}]
`

const planDeploy = `
on:
  push:
    branches: [main]
    paths: ['apps/**']
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps: [{run: ./deploy.sh}]
`

// A control plane's plan command is answered from this machine's own mirror,
// at the commit the event names, with the paths filter read against the
// push's diff (A52, INV-5).
func TestAgent_PlansAnEventFromItsMirror(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; a plan is read from a real mirror (INV-5)")
	}
	cp := newControlPlane(t)
	repo := shortTempDir(t)
	before := newRepo(t, repo, testJobs)
	for name, body := range map[string]string{"checks.yml": planChecks, "deploy.yml": planDeploy} {
		path := filepath.Join(repo, ".github", "workflows", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "workflows")
	sha := runGit(t, repo, "rev-parse", "HEAD")

	cfg := newConfigDir(t, configOptions{repoURL: repo, remoteURL: cp.URL()})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)
	waitFor(t, "the agent to connect", 20*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		return len(cp.conns) > 0 && len(cp.resumes) > 0
	})

	cp.push(t, protocol.NewPlanCommand(protocol.PlanRequest{ID: "p1", Repo: "app", Event: protocol.Event{
		Name: "push", Ref: "refs/heads/main", SHA: sha, Before: before,
	}}))
	cp.push(t, protocol.NewPlanCommand(protocol.PlanRequest{ID: "p2", Repo: "nope", Event: protocol.Event{
		Name: "push", Ref: "refs/heads/main", SHA: sha,
	}}))

	var plans map[string]protocol.PlanResult
	waitFor(t, "both plans", 60*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		plans = map[string]protocol.PlanResult{}
		for _, p := range cp.plans {
			plans[p.ID] = p
		}
		return len(plans) == 2
	})

	p1 := plans["p1"]
	if p1.Error != "" || len(p1.Workflows) != 1 {
		t.Fatalf("p1: %+v — the deploy workflow's paths filter matches nothing this push changed", p1)
	}
	w := p1.Workflows[0]
	if w.Name != "Checks" || w.Concurrency == nil || w.Concurrency.Group != "checks-refs/heads/main" {
		t.Errorf("workflow %+v", w)
	}
	if len(w.Jobs) != 2 || w.Jobs[1].Key != "web" || !slices.Equal(w.Jobs[1].Needs, []string{"lint"}) ||
		!slices.Equal(w.Jobs[1].Labels, []string{"linux"}) {
		t.Errorf("jobs %+v", w.Jobs)
	}
	if p2 := plans["p2"]; p2.ErrorCode != protocol.ErrUnknownRepo || p2.Workflows != nil {
		t.Errorf("p2: %+v, want an unknown repository", p2)
	}

	waitFor(t, "a heartbeat naming the repository", 30*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		return len(cp.lastHealth.Repos) == 1 && cp.lastHealth.Repos[0].Key == "app"
	})
}
