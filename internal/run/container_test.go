package run

import (
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
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
