package run

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// stepScope is what the run loop knows as a step is about to run and the
// registry, compiled at gate 11, could not: the job's status so far, every
// earlier step's outputs, and the runner gate 13 chose.
//
// Outputs is empty until the $GITHUB_OUTPUT channel is wired; the compiler
// refuses steps.* until then, so nothing reads it yet.
func (x *execution) stepScope(status registry.StepStatus) registry.StepScope {
	actions := ""
	temp := ""
	if x.env != nil {
		actions = x.env.ActionsDir()
		temp = x.env.TempDir()
	}
	return registry.StepScope{
		Status:     status,
		Outputs:    x.outputs,
		Results:    x.results,
		Runner:     runnerFacts(x.runner),
		ActionsDir: actions,
		TempDir:    temp,
	}
}

// runnerFacts describes the runner in the terms an expression reads.
//
// OS and arch come from the labels the job matched on, because those are what
// the operator said this machine is and what admission believed. The agent's
// own GOOS/GOARCH is the fallback, not the source: a docker runner on an
// arm64 Mac runs linux steps, and its labels say so where the binary cannot.
func runnerFacts(r *Runner) registry.RunnerFacts {
	f := registry.RunnerFacts{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if r == nil {
		return f
	}
	f.Name = r.Name
	for _, l := range r.Labels {
		switch strings.ToLower(l) {
		case "macos", "darwin":
			f.OS = "macos"
		case "linux":
			f.OS = "linux"
		case "arm64", "aarch64":
			f.Arch = "arm64"
		case "amd64", "x64", "x86_64":
			f.Arch = "amd64"
		}
	}
	return f
}

// resolveLate computes a step's late-bound env values.
//
// A failure here is the registry's, not the step's: the expression was
// admitted, and what refused is its evaluation against this run's state. It
// is reported under E_BAD_REGISTRY so the operator looks at the workflow, not
// at the command that never ran.
func (x *execution) resolveLate(s registry.ResolvedStep, sc registry.StepScope) (map[string]string, error) {
	if len(s.Late) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(s.Late))
	for k, v := range s.Late {
		val, err := v.Resolve(sc)
		if err != nil {
			return nil, &registry.Error{
				Code: protocol.ErrBadRegistry,
				Msg:  fmt.Sprintf("late env %s", k),
				Err:  err,
			}
		}
		out[k] = val
	}
	return out, nil
}

// stopsTheRun reports whether an outcome ends the job now, before any later
// step's condition is consulted.
//
// A failure does not: GitHub's model, which A42 adopted, is that a step
// failing leaves later steps to their conditions — `always()` and `failure()`
// run, the default `success()` does not. Cancellation and the job's own
// budget do: §11's signalling contract is that a cancelled run stops, and a
// job past its budget has no time left to spend on a cleanup step that would
// itself be cut short. A single step's timeout is a failure like any other.
func stopsTheRun(out outcome) bool {
	if out.state == protocol.StateCancelled {
		return true
	}
	return out.code == protocol.ErrTimeout
}

// statusAfter folds a step's outcome into the job's status for later steps.
func statusAfter(current registry.StepStatus, out outcome) registry.StepStatus {
	if current != registry.StatusSuccess {
		return current
	}
	switch out.state {
	case protocol.StateCancelled:
		return registry.StatusCancelled
	case protocol.StateFailed:
		return registry.StatusFailure
	}
	return current
}
