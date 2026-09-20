package registry

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func containerJob(steps ...Step) *Registry {
	return &Registry{Version: SchemaVersion, Jobs: map[string]*Job{"j": {RunsOn: []string{"linux"}, Steps: steps}}}
}

func TestContainerStepValidation(t *testing.T) {
	ok := Step{Container: &ContainerStep{Image: "ghcr.io/me/tool:1.2", Args: []string{"--flag", "; rm -rf /"}}}
	if err := containerJob(ok).Validate(); err != nil {
		t.Fatalf("a container step with an argv should validate — an argument is an argument: %v", err)
	}

	bad := map[string]Step{
		"both a command and a container": {Run: "make", Container: &ContainerStep{Image: "alpine"}},
		"neither":                        {},
		"image with a space":             {Container: &ContainerStep{Image: "alpine latest"}},
		"image with a leading dash":      {Container: &ContainerStep{Image: "-v/etc:/x"}},
		"empty image":                    {Container: &ContainerStep{}},
		"NUL in an arg":                  {Container: &ContainerStep{Image: "alpine", Args: []string{"a\x00b"}}},
		"newline in the entrypoint":      {Container: &ContainerStep{Image: "alpine", Entrypoint: "/bin/sh\n-c"}},
	}
	for name, s := range bad {
		if err := containerJob(s).Validate(); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestPinned(t *testing.T) {
	if (&ContainerStep{Image: "alpine:3.19"}).Pinned() {
		t.Error("a tag is a mutable reference")
	}
	if !(&ContainerStep{Image: "alpine@sha256:" + strings.Repeat("a", 64)}).Pinned() {
		t.Error("a digest is pinned")
	}
}

// Derived, so it cannot drift from the steps it describes.
func TestRequiredExecutorIsDerived(t *testing.T) {
	plain := &Job{Steps: []Step{{Run: "make"}}}
	if got := plain.RequiredExecutor(); got != "" {
		t.Errorf("a job of commands requires %q", got)
	}
	mixed := &Job{Steps: []Step{{Run: "make"}, {Container: &ContainerStep{Image: "alpine"}}}}
	if got := mixed.RequiredExecutor(); got != ExecutorDocker {
		t.Errorf("a job with a container step requires %q", got)
	}
}

// Gate 13's second half: a machine with only a native runner hears so at
// admission, with the executor named, rather than from a build.
func TestAdmitOnHoldsTheRunnerToTheExecutor(t *testing.T) {
	r := containerJob(Step{Container: &ContainerStep{Image: "alpine"}})
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	req := protocol.RunRequest{Job: "j", Ref: "refs/heads/main"}

	res, d := r.AdmitOn(req, Runner{Labels: []string{"linux"}, Executor: "docker"})
	if !d.OK() {
		t.Fatalf("a docker runner should be admitted: %+v", d)
	}
	if res.RequiresExecutor != ExecutorDocker || res.Steps[0].Container == nil {
		t.Errorf("resolved = %+v", res)
	}

	_, d = r.AdmitOn(req, Runner{Labels: []string{"linux"}, Executor: "native"})
	if d.Code != protocol.ErrNoRunner || !strings.Contains(d.Msg, "docker") || !strings.Contains(d.Msg, "native") {
		t.Errorf("a native runner should be refused naming both executors: %+v", d)
	}

	// Labels are checked first, so a machine that is simply the wrong one is
	// told that rather than something about executors.
	_, d = r.AdmitOn(req, Runner{Labels: []string{"macos"}, Executor: "native"})
	if d.Code != protocol.ErrNoRunner || !strings.Contains(d.Msg, "labels") {
		t.Errorf("the label refusal should come first: %+v", d)
	}

	// Fails closed: a caller that does not say what the executor is cannot
	// land a container step anywhere.
	if _, d = r.Admit(req, []string{"linux"}); d.Code != protocol.ErrNoRunner {
		t.Errorf("Admit without an executor must refuse a container job: %+v", d)
	}
	// And a job of commands is unaffected by any of this.
	plain := containerJob(Step{Run: "make"})
	if _, d = plain.Admit(req, []string{"linux"}); !d.OK() {
		t.Errorf("an ordinary job must still admit through Admit: %+v", d)
	}
}
