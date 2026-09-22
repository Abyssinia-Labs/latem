package run

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

type namedExecutor struct {
	executor.Executor
	name string
}

func (n namedExecutor) Name() string { return n.name }

// The first runner satisfying labels *and* executor wins. Failing that, the
// first label match is returned so gate 13 can refuse with the executor named
// rather than with a list of labels that were all present.
func TestSelectRunnerConsidersTheExecutor(t *testing.T) {
	o := &Orchestrator{runners: []Runner{
		{Name: "mac", Labels: []string{"linux"}, Executor: namedExecutor{name: "native"}},
		{Name: "box", Labels: []string{"linux"}, Executor: namedExecutor{name: "docker"}},
	}}
	reg := &registry.Registry{Jobs: map[string]*registry.Job{
		"plain":     {RunsOn: []string{"linux"}, Steps: []registry.Step{{Run: "make"}}},
		"container": {RunsOn: []string{"linux"}, Steps: []registry.Step{{Container: &registry.ContainerStep{Image: "alpine"}}}},
		"elsewhere": {RunsOn: []string{"windows"}, Steps: []registry.Step{{Run: "make"}}},
	}}

	if r := o.selectRunner(reg, "plain"); r == nil || r.Name != "mac" {
		t.Errorf("config order is preference order for an ordinary job: %+v", r)
	}
	if r := o.selectRunner(reg, "container"); r == nil || r.Name != "box" {
		t.Errorf("a container job should skip the native runner: %+v", r)
	}
	if r := o.selectRunner(reg, "elsewhere"); r != nil {
		t.Errorf("no label match should stay nil: %+v", r)
	}

	onlyNative := &Orchestrator{runners: o.runners[:1]}
	if r := onlyNative.selectRunner(reg, "container"); r == nil || r.Name != "mac" {
		t.Errorf("with no docker runner the label match is returned, for gate 13 to refuse by name: %+v", r)
	}
	if executorName(nil) != "" || executorName(&Runner{}) != "" {
		t.Error("a runner without an executor has no name")
	}
}

const dockerStepWorkflow = `
on: [push]
jobs:
  test:
    runs-on: [linux]
    steps:
      - name: lint
        uses: docker://ghcr.io/me/lint:1
        with:
          args: ["--msg", "${{ github.event_name }} ; rm -rf /"]
          level: error
`

// The whole chain for a docker:// step: compiled from a workflow at gate 11,
// held to a docker runner at gate 13, and handed to the executor as a
// container with its argv intact.
func TestExecute_DockerStepReachesTheExecutorAsAContainer(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.runners[0].Executor = namedExecutor{Executor: h.exec, name: "docker"}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", dockerStepWorkflow)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.State != protocol.StateSucceeded {
		t.Fatalf("got %s/%s: %s", res.State, res.ErrorCode, res.ErrorMsg)
	}
	steps := h.env.executed()
	if len(steps) != 1 || steps[0].Container == nil {
		t.Fatalf("executed = %+v, want one container step", steps)
	}
	c := steps[0].Container
	if c.Image != "ghcr.io/me/lint:1" || steps[0].Run != "" {
		t.Errorf("container = %+v, run = %q", c, steps[0].Run)
	}
	// One argument, whatever it contains.
	if len(c.Args) != 2 || c.Args[1] != "workflow_dispatch ; rm -rf /" {
		t.Errorf("args = %q", c.Args)
	}
	if steps[0].Env["INPUT_LEVEL"] != "error" {
		t.Errorf("env = %+v", steps[0].Env)
	}
}

// And on a machine whose only matching runner is native, the refusal comes
// from admission with both executors named — before anything runs.
func TestExecute_DockerStepIsRefusedAtAdmissionOnANativeRunner(t *testing.T) {
	h := newHarness(t)
	h.source.registry = ""
	h.runners[0].Executor = namedExecutor{Executor: h.exec, name: "native"}
	o := h.build()

	req := h.request("test")
	r := h.admitAndClaim(o, req)
	h.writeWorkspaceFile(req.ID, ".github/workflows/ci.yml", dockerStepWorkflow)

	res, err := o.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ErrorCode != protocol.ErrNoRunner {
		t.Fatalf("got %s/%s, want E_NO_RUNNER", res.State, res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMsg, "docker") || !strings.Contains(res.ErrorMsg, "native") {
		t.Errorf("the refusal should name both executors: %q", res.ErrorMsg)
	}
	if len(h.env.executed()) != 0 {
		t.Errorf("nothing should have run: %+v", h.env.executed())
	}
}

// A job carrying third-party code prefers an isolating runner, and when there
// is none the label match is still returned so gate 13 refuses by name rather
// than reporting "no runner" with no reason.
func TestSelectRunnerPrefersAnIsolatingRunner(t *testing.T) {
	o := &Orchestrator{runners: []Runner{
		{Name: "mac", Labels: []string{"linux"}, Executor: namedExecutor{name: "native"}},
		{Name: "box", Labels: []string{"linux"}, Executor: namedExecutor{name: "docker"}},
	}}
	reg := &registry.Registry{Jobs: map[string]*registry.Job{
		"third": {RunsOn: []string{"linux"}, Steps: []registry.Step{{Run: "make"}}, RequiresIsolation: true},
	}}
	if r := o.selectRunner(reg, "third"); r == nil || r.Name != "box" {
		t.Errorf("want the isolating runner, got %+v", r)
	}

	onlyNative := &Orchestrator{runners: o.runners[:1]}
	if r := onlyNative.selectRunner(reg, "third"); r == nil || r.Name != "mac" {
		t.Errorf("with no isolating runner the label match is returned for gate 13: %+v", r)
	}

	if executorIsolates(&o.runners[0]) {
		t.Error("native does not confine a step, on any OS")
	}
	if !executorIsolates(&o.runners[1]) {
		t.Error("docker does")
	}
	if executorIsolates(&Runner{Name: "x", Executor: namedExecutor{name: "something-new"}}) {
		t.Error("an unknown executor must not inherit isolation by default")
	}
}

// The policy comes from config on every run, so narrowing an allowlist takes
// effect on the next build rather than the next restart.
func TestActionPolicyComesFromConfig(t *testing.T) {
	yes := true
	o := &Orchestrator{cfg: &config.Config{Actions: config.Actions{
		Allowed: []string{"actions/*"}, RequirePinned: &yes, AllowUnisolated: true,
	}}}
	p := o.actionPolicy()
	if len(p.Allowed) != 1 || !p.RequirePinned || !p.AllowUnisolated {
		t.Errorf("policy = %+v", p)
	}
	// Pinning defaults on, so a config written before the block existed does
	// not silently start accepting moving references.
	bare := &Orchestrator{cfg: &config.Config{}}
	if !bare.actionPolicy().RequirePinned {
		t.Error("require_pinned must default on")
	}
	if len(bare.actionPolicy().Allowed) != 0 {
		t.Error("and nothing is allowed by default")
	}
	if (&Orchestrator{}).actionPolicy().RequirePinned != true {
		t.Error("no config at all is the refusing default")
	}
}
