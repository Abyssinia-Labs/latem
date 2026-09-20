package docker

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

func lintStep() executor.Step {
	return executor.Step{
		Name: "lint",
		Env:  map[string]string{"INPUT_LEVEL": "error"},
		Container: &executor.Container{
			Image:      "ghcr.io/me/lint:1",
			Entrypoint: "/bin/lint",
			Args:       []string{"--msg", "; rm -rf /"},
		},
	}
}

// A container step is a sibling of the run container, not a stranger: same
// workspace, same per-run volume — so $GITHUB_OUTPUT is the same file — same
// labels for crash reaping, and an argv that is an argv.
func TestExec_ContainerStep(t *testing.T) {
	f := &fakeAPI{waitStatus: 3, execStdout: []string{"linting\n"}, repoDigests: []string{"ghcr.io/me/lint@sha256:abc"}}
	e, ws := newExecutor(t, f, nil)
	env, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	removedBefore := len(f.removed)

	var stdout, stderr bytes.Buffer
	res, err := env.Exec(context.Background(), lintStep(), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want the container's 3", res.ExitCode)
	}

	if len(f.createdConfigs) != 2 {
		t.Fatalf("containers created = %d, want the run's and the step's", len(f.createdConfigs))
	}
	cfg, host, name := f.createdConfigs[1], f.createdHosts[1], f.createdNames[1]
	if cfg.Image != "ghcr.io/me/lint:1" || !slices.Equal(cfg.Entrypoint, []string{"/bin/lint"}) {
		t.Errorf("image/entrypoint = %q %v", cfg.Image, cfg.Entrypoint)
	}
	// INV-3: element for element, nothing joined, nothing re-split.
	if !slices.Equal(cfg.Cmd, []string{"--msg", "; rm -rf /"}) {
		t.Errorf("cmd = %q", cfg.Cmd)
	}
	if cfg.WorkingDir != WorkspacePath || cfg.Labels[LabelRunID] != "r1" || cfg.Labels[LabelManaged] != "true" {
		t.Errorf("workdir/labels = %q %v", cfg.WorkingDir, cfg.Labels)
	}
	if cfg.User != "" {
		t.Errorf("a container step runs as its image's user, got %q", cfg.User)
	}
	if !strings.HasSuffix(name, "-step-1") {
		t.Errorf("name = %q", name)
	}
	if !slices.Contains(host.Mounts, mount.Mount{Type: mount.TypeVolume, Source: RunVolumePrefix + "r1", Target: RunFilesDir}) {
		t.Errorf("the step cannot see the run's files: %v", host.Mounts)
	}
	if !slices.Contains(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: ws, Target: WorkspacePath}) {
		t.Errorf("the step cannot see the workspace: %v", host.Mounts)
	}
	for _, want := range []string{"INPUT_LEVEL=error", "GITHUB_OUTPUT=" + OutputFilePath, "LATEM_ENV=" + EnvFilePath} {
		if !slices.Contains(cfg.Env, want) {
			t.Errorf("env is missing %s: %v", want, cfg.Env)
		}
	}

	// It ran as a container, not as an exec in the run container.
	if len(f.execOpts) != 0 {
		t.Errorf("a container step must not exec in the run container: %v", f.execOpts)
	}
	// The step's container is gone afterwards; the run's is not.
	if len(f.removed) != removedBefore+1 {
		t.Errorf("removed %d containers during the step, want exactly the step's", len(f.removed)-removedBefore)
	}
	// A tag is mutable, so the run says what it resolved to.
	if !strings.Contains(stdout.String(), "image ghcr.io/me/lint:1 is ghcr.io/me/lint@sha256:abc") {
		t.Errorf("the resolved digest should be in the step's log: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "linting") {
		t.Errorf("the container's output should be streamed: %q", stdout.String())
	}
}

func TestExec_ContainerStepRequirePinned(t *testing.T) {
	f := &fakeAPI{}
	e, ws := newExecutor(t, f, func(o *Options) { o.RequirePinnedImages = true })
	env, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}

	res, err := env.Exec(context.Background(), lintStep(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "require_pinned_images") {
		t.Fatalf("a tag should be refused by name: %+v, %v", res, err)
	}
	if len(f.createdConfigs) != 1 {
		t.Errorf("a refused image must not get as far as a container: %d created", len(f.createdConfigs))
	}

	pinned := lintStep()
	pinned.Container.Image = "ghcr.io/me/lint@sha256:" + strings.Repeat("a", 64)
	if _, err := env.Exec(context.Background(), pinned, nil, nil); err != nil {
		t.Errorf("a digest should be accepted: %v", err)
	}
}

// The per-run volume has the opposite lifecycle to a cache: it goes when the
// run does, including when Prepare fails before there is an Env to close.
func TestRunVolumeLifecycle(t *testing.T) {
	f := &fakeAPI{}
	e, ws := newExecutor(t, f, nil)
	env, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !slices.Equal(f.removedVolumes, []string{RunVolumePrefix + "r1"}) {
		t.Errorf("Close removed volumes %v, want the run's", f.removedVolumes)
	}

	failing := &fakeAPI{createErr: context.DeadlineExceeded}
	e2, ws2 := newExecutor(t, failing, nil)
	if _, err := e2.Prepare(context.Background(), executor.Spec{RunID: "r2", Workspace: ws2}); err == nil {
		t.Fatal("Prepare should have failed")
	}
	if !slices.Equal(failing.removedVolumes, []string{RunVolumePrefix + "r2"}) {
		t.Errorf("a failed Prepare leaked its run volume: removed %v", failing.removedVolumes)
	}
}

// §6: a crashed run's volume is collected with its container, and a live
// run's is left alone.
func TestReap_RunVolumes(t *testing.T) {
	f := &fakeAPI{listedVolumes: []*volume.Volume{
		{Name: RunVolumePrefix + "dead", Labels: map[string]string{LabelRunID: "dead", LabelRunVolume: "true"}},
		{Name: RunVolumePrefix + "live", Labels: map[string]string{LabelRunID: "live", LabelRunVolume: "true"}},
	}}
	e, _ := newExecutor(t, f, nil)
	reaped, err := e.Reap(context.Background(), func(id string) bool { return id == "live" })
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !slices.Equal(f.removedVolumes, []string{RunVolumePrefix + "dead"}) {
		t.Errorf("removed %v, want only the dead run's volume", f.removedVolumes)
	}
	found := false
	for _, r := range reaped {
		if r.Kind == "volume" && r.RunID == "dead" {
			found = true
		}
	}
	if !found {
		t.Errorf("the reaped volume should be reported: %+v", reaped)
	}
}
