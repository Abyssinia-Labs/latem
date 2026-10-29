package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

const runFlow = `
name: CI
on: push
jobs:
  build:
    runs-on: [test]
    outputs:
      tag: v-${{ github.run_number }}
    steps: [{run: make}]
  deploy:
    needs: build
    if: github.event_name == 'release'
    runs-on: [test]
    steps: [{run: ./deploy.sh}]
  report:
    needs: build
    runs-on: [test]
    outputs:
      got: ${{ needs.build.outputs.tag }}
    steps: [{run: ./report.sh}]
`

// A workflow run, the way a control plane drives one (A52): plan the push,
// then send each job as its needs finish, carrying their results and
// outputs, a token, and the event. The token never reaches the database.
func TestAgent_RunsAWorkflowRunFromItsPlan(t *testing.T) {
	if !hasGit(t) {
		t.Skip("git is not installed; a plan is read from a real mirror (INV-5)")
	}
	cp := newControlPlane(t)
	repo := shortTempDir(t)
	newRepo(t, repo, testJobs)
	path := filepath.Join(repo, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(runFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "ci")
	sha := runGit(t, repo, "rev-parse", "HEAD")

	cfg := newConfigDir(t, configOptions{repoURL: repo, remoteURL: cp.URL()})
	ta := newTestAgent(t, cfg, nil)
	ta.run(t)
	waitFor(t, "the agent to connect", 20*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		return len(cp.resumes) > 0
	})

	event := protocol.Event{Name: "push", Ref: "refs/heads/main", SHA: sha, Actor: "octocat",
		Payload: []byte(`{"head_commit":{"message":"ci"}}`)}
	cp.push(t, protocol.NewPlanCommand(protocol.PlanRequest{ID: "p1", Repo: "app", Event: event}))
	var plan protocol.PlanResult
	waitFor(t, "the plan", 60*time.Second, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if len(cp.plans) > 0 {
			plan = cp.plans[0]
		}
		return len(cp.plans) > 0
	})
	if len(plan.Workflows) != 1 || len(plan.Workflows[0].Jobs) != 3 || plan.Workflows[0].Jobs[0].Key != "build" {
		t.Fatalf("plan %+v", plan)
	}

	const token = "ghs_agentTestToken0123456789"
	terminal := func(id string) protocol.Update {
		var last protocol.Update
		waitFor(t, id+" to finish", 120*time.Second, func() bool {
			cp.mu.Lock()
			defer cp.mu.Unlock()
			for _, u := range cp.updates {
				if u.RunID == id && u.Update.State.IsTerminal() {
					last = u.Update
					return true
				}
			}
			return false
		})
		return last
	}
	send := func(id, job string, needs map[string]protocol.NeedResult) {
		ev := event
		cp.push(t, protocol.NewRunCommand(protocol.RunRequest{
			ID: id, Repo: "app", Job: job, Ref: "refs/heads/main",
			Origin:   protocol.Origin{Kind: "webhook", Actor: "octocat"},
			Workflow: plan.Workflows[0].File, SHA: sha, Event: &ev, Needs: needs,
			WorkflowRunID: "wr1", RunNumber: 12, RunAttempt: 1, Token: token,
		}))
	}

	send("r-build", "build", nil)
	build := terminal("r-build")
	if build.State != protocol.StateSucceeded || build.Outputs["tag"] != "v-12" {
		t.Fatalf("build: %+v", build)
	}
	needs := map[string]protocol.NeedResult{"build": {Result: protocol.NeedSuccess, Outputs: build.Outputs}}
	send("r-deploy", "deploy", needs)
	send("r-report", "report", needs)
	if u := terminal("r-deploy"); u.State != protocol.StateSkipped {
		t.Errorf("deploy: %+v, want skipped — its if: is false on a push", u)
	}
	if u := terminal("r-report"); u.State != protocol.StateSucceeded || u.Outputs["got"] != "v-12" {
		t.Errorf("report: %+v, want build's output through needs", u)
	}

	raw, err := os.ReadFile(cfg.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		if b, err := os.ReadFile(cfg.Paths.DB + suffix); err == nil && strings.Contains(string(b), token) {
			t.Errorf("the token reached %s", filepath.Base(cfg.Paths.DB+suffix))
		}
	}
	if !strings.Contains(string(raw)+mustRead(cfg.Paths.DB+"-wal"), ".github/workflows/ci.yml") {
		t.Error("the workflow run's fields were not kept with the run")
	}
}

func mustRead(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
