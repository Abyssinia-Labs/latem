package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestName(t *testing.T) {
	e, _ := newExecutor(t, &fakeAPI{}, nil)
	if got := e.Name(); got != "docker" {
		t.Errorf("Name = %q, want docker", got)
	}
}

// Amendment A6: the run id becomes a container name and a filesystem path.
// A control plane sending "../../../../tmp/x" must be refused before either
// is built, not after.
func TestPrepare_ValidatesRunID(t *testing.T) {
	bad := map[string]string{
		"empty":            "",
		"traversal":        "../../../../tmp/x",
		"slash":            "a/b",
		"dot":              ".",
		"double dot":       "..",
		"leading hyphen":   "-rf",
		"shell characters": "a;rm -rf /",
		"too long":         strings.Repeat("a", protocol.MaxIDLength+1),
		"null byte":        "a\x00b",
		"newline":          "a\nb",
	}

	for name, id := range bad {
		t.Run(name, func(t *testing.T) {
			f := &fakeAPI{}
			e, ws := newExecutor(t, f, nil)
			_, err := e.Prepare(context.Background(), executor.Spec{RunID: id, Workspace: ws})
			if err == nil {
				t.Fatalf("Prepare accepted run id %q", id)
			}
			if len(f.createdNames) != 0 {
				t.Errorf("a container named %q was created before validation", f.createdNames)
			}
		})
	}
}

func TestPrepare_ContainerConfiguration(t *testing.T) {
	f := &fakeAPI{}
	e, ws := newExecutor(t, f, nil)

	_, err := e.Prepare(context.Background(), executor.Spec{
		RunID:     "run-abc",
		Workspace: ws,
		Image:     "ci:1",
		Labels:    map[string]string{"latem.repo": "acme/app"},
		Resources: executor.Resources{CPUs: 4, Memory: "8g"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	cfg, host, name := f.createdConfigs[0], f.createdHosts[0], f.createdNames[0]

	if name != "latem-run-run-abc" {
		t.Errorf("container name = %q, want latem-run-run-abc", name)
	}
	if cfg.Image != "ci:1" {
		t.Errorf("image = %q, want the job's image", cfg.Image)
	}
	if cfg.WorkingDir != WorkspacePath {
		t.Errorf("WorkingDir = %q, want %q", cfg.WorkingDir, WorkspacePath)
	}
	if cfg.Labels[LabelRunID] != "run-abc" || cfg.Labels[LabelManaged] != "true" {
		t.Errorf("labels = %v, want the reconciliation pair", cfg.Labels)
	}
	if cfg.Labels["latem.repo"] != "acme/app" {
		t.Errorf("the spec's own labels were dropped: %v", cfg.Labels)
	}

	wantMount := mount.Mount{Type: mount.TypeBind, Source: ws, Target: WorkspacePath}
	if !slices.Contains(host.Mounts, wantMount) {
		t.Errorf("mounts = %v, want the workspace bind-mounted rw at %s", host.Mounts, WorkspacePath)
	}
	if host.NanoCPUs != 4e9 {
		t.Errorf("NanoCPUs = %d, want 4e9", host.NanoCPUs)
	}
	if host.Memory != 8<<30 {
		t.Errorf("Memory = %d, want 8 GiB", host.Memory)
	}
	if host.PidsLimit == nil || *host.PidsLimit != DefaultPidsLimit {
		t.Errorf("PidsLimit = %v, want %d", host.PidsLimit, DefaultPidsLimit)
	}
	// §13.1: --rm is deliberately not used, so a failed step's artifacts are
	// still there when Fetch runs.
	if host.AutoRemove {
		t.Error("AutoRemove is set; artifacts would vanish with the first failed step")
	}
	if len(host.DeviceRequests) != 0 {
		t.Errorf("a non-GPU run asked for devices: %v", host.DeviceRequests)
	}
}

func TestPrepare_FallsBackToTheRunnerImage(t *testing.T) {
	f := &fakeAPI{}
	e, ws := newExecutor(t, f, func(o *Options) { o.DefaultImage = "runner-default:1" })

	if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := f.createdConfigs[0].Image; got != "runner-default:1" {
		t.Errorf("image = %q, want the runner default", got)
	}
}

func TestPrepare_NoImageAnywhereIsAnError(t *testing.T) {
	e, ws := newExecutor(t, &fakeAPI{}, func(o *Options) { o.DefaultImage = "" })
	_, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws})
	if err == nil {
		t.Fatal("Prepare succeeded with no image")
	}
	if got := CodeOf(err); got != protocol.ErrExecutor {
		t.Errorf("error code = %q, want %q", got, protocol.ErrExecutor)
	}
}

func TestPrepare_PullsAMissingImage(t *testing.T) {
	f := &fakeAPI{imageMissing: true}
	e, ws := newExecutor(t, f, nil)

	if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if f.imageMissing {
		t.Error("the image was never pulled")
	}
}

func TestPrepare_RejectsABadWorkspace(t *testing.T) {
	tests := map[string]string{
		"relative":    "work/run-1",
		"nonexistent": "/nonexistent/latem/workspace",
		"empty":       "",
	}
	for name, ws := range tests {
		t.Run(name, func(t *testing.T) {
			e, _ := newExecutor(t, &fakeAPI{}, nil)
			if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws}); err == nil {
				t.Fatalf("Prepare accepted workspace %q", ws)
			}
		})
	}
}

func TestPrepare_CachesBecomeMountsAndVolumes(t *testing.T) {
	f := &fakeAPI{}
	e, ws := newExecutor(t, f, nil)

	_, err := e.Prepare(context.Background(), executor.Spec{
		RunID:     "r1",
		Workspace: ws,
		Caches: []executor.CacheMount{
			{Name: "gradle", Source: "latem-cache-linux-gradle", Target: "/home/runner/.gradle"},
			{Name: "pnpm", Source: "/srv/caches/pnpm", Target: "/home/runner/.pnpm-store"},
		},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// §14.1: a name is a Docker volume, created if it does not exist, because
	// caches are mounted rather than restored and the first run warms them.
	// Since A44 there is also the per-run volume, created first.
	if len(f.volumes) != 2 || f.volumes[0].Name != RunVolumePrefix+"r1" || f.volumes[1].Name != "latem-cache-linux-gradle" {
		t.Fatalf("volumes created = %v, want the run volume and one named cache volume", f.volumes)
	}
	cache := f.volumes[1]
	if cache.Labels[LabelManaged] != "true" {
		t.Errorf("cache volume is unlabelled: %v", cache.Labels)
	}
	// The label Reap deletes by. A cache must never carry it: a cache is
	// supposed to outlive every run, and this is the only thing standing
	// between a crashed run's cleanup and somebody's warm ~/.gradle.
	if _, marked := cache.Labels[LabelRunVolume]; marked {
		t.Errorf("a cache volume carries the run-volume label: %v", cache.Labels)
	}
	if f.volumes[0].Labels[LabelRunVolume] != "true" || f.volumes[0].Labels[LabelRunID] != "r1" {
		t.Errorf("run volume labels = %v", f.volumes[0].Labels)
	}

	mounts := f.createdHosts[0].Mounts
	if !slices.Contains(mounts, mount.Mount{
		Type: mount.TypeVolume, Source: "latem-cache-linux-gradle", Target: "/home/runner/.gradle",
	}) {
		t.Errorf("mounts = %v, want the named volume", mounts)
	}
	// §14.2: an absolute source is a host directory.
	if !slices.Contains(mounts, mount.Mount{
		Type: mount.TypeBind, Source: "/srv/caches/pnpm", Target: "/home/runner/.pnpm-store",
	}) {
		t.Errorf("mounts = %v, want the host directory bind", mounts)
	}
}

func TestValidateCacheMounts(t *testing.T) {
	tests := map[string]struct {
		caches  []executor.CacheMount
		wantErr bool
	}{
		"ok": {[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: "/cache/a"}}, false},
		"relative target": {
			[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: "cache/a"}}, true},
		"root target": {
			[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: "/"}}, true},
		// A cache mounted over the workspace hides the git clone §12 exists to
		// produce, and the resulting "my repo is empty" points nowhere near
		// the cache declaration that caused it.
		"over the workspace": {
			[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: WorkspacePath}}, true},
		"inside the workspace": {
			[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: WorkspacePath + "/node_modules"}}, true},
		"workspace prefix is fine": {
			[]executor.CacheMount{{Name: "a", Source: "vol-a", Target: "/workspace-cache"}}, false},
		"no source": {
			[]executor.CacheMount{{Name: "a", Target: "/cache/a"}}, true},
		"duplicate targets": {[]executor.CacheMount{
			{Name: "a", Source: "vol-a", Target: "/cache"},
			{Name: "b", Source: "vol-b", Target: "/cache"},
		}, true},
		"none": {nil, false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateCacheMounts(tc.caches)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateCacheMounts = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && CodeOf(err) != protocol.ErrCache {
				t.Errorf("error code = %q, want %q", CodeOf(err), protocol.ErrCache)
			}
		})
	}
}

func TestPrepare_GPUAndKVM(t *testing.T) {
	t.Run("per-run gpu", func(t *testing.T) {
		f := &fakeAPI{}
		e, ws := newExecutor(t, f, nil)
		_, err := e.Prepare(context.Background(), executor.Spec{
			RunID: "r1", Workspace: ws, Resources: executor.Resources{GPU: true},
		})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		reqs := f.createdHosts[0].DeviceRequests
		if len(reqs) != 1 || reqs[0].Count != -1 || reqs[0].Driver != "nvidia" {
			t.Errorf("DeviceRequests = %v, want all nvidia GPUs", reqs)
		}
	})

	t.Run("runner-wide kvm", func(t *testing.T) {
		f := &fakeAPI{}
		e, ws := newExecutor(t, f, func(o *Options) { o.RequireKVM = true })
		if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws}); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		devs := f.createdHosts[0].Devices
		if len(devs) != 1 || devs[0].PathInContainer != "/dev/kvm" {
			t.Errorf("Devices = %v, want /dev/kvm", devs)
		}
	})
}

// A container created but never started still carries the run's labels, so
// reconciliation would find it and call it a live run.
func TestPrepare_CleansUpWhenStartFails(t *testing.T) {
	f := &fakeAPI{startErr: errors.New("no such device")}
	e, ws := newExecutor(t, f, nil)

	if _, err := e.Prepare(context.Background(), executor.Spec{RunID: "r1", Workspace: ws}); err == nil {
		t.Fatal("Prepare succeeded despite a failed start")
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %d containers, want the one that would not start", len(f.removed))
	}
}

func TestBuildLabels(t *testing.T) {
	got := buildLabels("run-1", map[string]string{
		"latem.repo": "acme/app",
		// A hostile or buggy caller must not be able to make a run invisible
		// to reconciliation.
		LabelManaged: "false",
		LabelRunID:   "someone-else",
	})
	if got[LabelManaged] != "true" {
		t.Errorf("latem.managed = %q, want true regardless of the caller", got[LabelManaged])
	}
	if got[LabelRunID] != "run-1" {
		t.Errorf("latem.run-id = %q, want the real run id", got[LabelRunID])
	}
	if got["latem.repo"] != "acme/app" {
		t.Errorf("the caller's other labels were dropped: %v", got)
	}
}

func TestBuildLabels_NilExtra(t *testing.T) {
	got := buildLabels("run-1", nil)
	if len(got) != 2 {
		t.Errorf("labels = %v, want exactly the reconciliation pair", got)
	}
}

func TestDiscover(t *testing.T) {
	f := &fakeAPI{containers: []container.Summary{
		{ID: "b", Names: []string{"/latem-run-r2"}, State: "exited",
			Labels: map[string]string{LabelRunID: "r2", LabelManaged: "true"}},
		{ID: "a", Names: []string{"/latem-run-r1"}, State: "running",
			Labels: map[string]string{LabelRunID: "r1", LabelManaged: "true"}},
	}}
	e, _ := newExecutor(t, f, nil)

	got, err := e.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2", len(got))
	}
	if got[0].RunID != "r1" || got[1].RunID != "r2" {
		t.Errorf("Discover is not ordered by run id: %v", got)
	}
	if got[0].Name != "latem-run-r1" {
		t.Errorf("name = %q, want the leading slash stripped", got[0].Name)
	}
}

func TestPreflight(t *testing.T) {
	t.Run("daemon reachable", func(t *testing.T) {
		e, _ := newExecutor(t, &fakeAPI{}, nil)
		if err := e.Preflight(context.Background()); err != nil {
			t.Errorf("Preflight: %v", err)
		}
	})

	t.Run("daemon unreachable", func(t *testing.T) {
		e, _ := newExecutor(t, &fakeAPI{pingErr: errors.New("connection refused")}, nil)
		err := e.Preflight(context.Background())
		if err == nil {
			t.Fatal("Preflight passed with no daemon")
		}
		if got := CodeOf(err); got != protocol.ErrExecutor {
			t.Errorf("error code = %q, want %q", got, protocol.ErrExecutor)
		}
	})

	// The whole point of the GPU check is that it fails at startup. A runner
	// that discovers a missing toolkit forty minutes into a build has already
	// wasted the forty minutes and blamed the job for it.
	t.Run("gpu missing fails at startup", func(t *testing.T) {
		e, _ := newExecutor(t, &fakeAPI{waitStatus: 127}, func(o *Options) { o.GPU = true })
		err := e.Preflight(context.Background())
		if err == nil {
			t.Fatal("Preflight passed with a broken NVIDIA toolkit")
		}
		if !strings.Contains(err.Error(), "nvidia-smi") {
			t.Errorf("error %q does not name what failed", err)
		}
	})

	t.Run("gpu present", func(t *testing.T) {
		e, _ := newExecutor(t, &fakeAPI{waitStatus: 0}, func(o *Options) { o.GPU = true })
		if err := e.Preflight(context.Background()); err != nil {
			t.Errorf("Preflight: %v", err)
		}
	})

	t.Run("kvm missing", func(t *testing.T) {
		e, _ := newExecutor(t, &fakeAPI{waitStatus: 1}, func(o *Options) { o.RequireKVM = true })
		err := e.Preflight(context.Background())
		if err == nil {
			t.Fatal("Preflight passed with no usable /dev/kvm")
		}
		if !strings.Contains(err.Error(), "/dev/kvm") {
			t.Errorf("error %q does not name what failed", err)
		}
	})

	t.Run("probe containers are removed", func(t *testing.T) {
		f := &fakeAPI{waitStatus: 0}
		e, _ := newExecutor(t, f, func(o *Options) { o.GPU = true })
		if err := e.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight: %v", err)
		}
		if len(f.removed) != 1 {
			t.Errorf("removed %d probe containers, want 1", len(f.removed))
		}
	})
}

func TestParseMemory(t *testing.T) {
	tests := map[string]struct {
		in      string
		want    int64
		wantErr bool
	}{
		"empty means unlimited": {"", 0, false},
		"zero":                  {"0", 0, false},
		"bytes":                 {"1024", 1024, false},
		"kilo":                  {"512k", 512 << 10, false},
		"mega":                  {"512m", 512 << 20, false},
		"giga":                  {"8g", 8 << 30, false},
		"tera":                  {"1t", 1 << 40, false},
		"long suffix":           {"8gb", 8 << 30, false},
		"uppercase":             {"8G", 8 << 30, false},
		"decimal":               {"1.5g", 1610612736, false},
		"spaces":                {" 8g ", 8 << 30, false},
		"garbage":               {"lots", 0, true},
		"unit only":             {"g", 0, true},
		"negative":              {"-8g", 0, true},
		"wrong unit":            {"8pb", 0, true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseMemory(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseMemory(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("parseMemory(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// §13.1: the uid flag is a Linux-only fix for bind-mount ownership. Applying
// it on Docker Desktop would force a uid the image may not know, which breaks
// the writable $HOME the base image contract is about.
func TestResolveUser(t *testing.T) {
	tests := map[string]struct {
		opt, goos string
		wantEmpty bool
		want      string
	}{
		"explicit wins on linux":  {"1000:1000", "linux", false, "1000:1000"},
		"explicit wins on darwin": {"1000:1000", "darwin", false, "1000:1000"},
		"image default sentinel":  {UserImageDefault, "linux", true, ""},
		"auto on darwin":          {"", "darwin", true, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := resolveUser(tc.opt, tc.goos)
			if tc.wantEmpty && got != "" {
				t.Errorf("resolveUser(%q, %q) = %q, want empty", tc.opt, tc.goos, got)
			}
			if !tc.wantEmpty && got != tc.want {
				t.Errorf("resolveUser(%q, %q) = %q, want %q", tc.opt, tc.goos, got, tc.want)
			}
		})
	}

	t.Run("auto on linux is the invoking user", func(t *testing.T) {
		got := resolveUser("", "linux")
		if !strings.Contains(got, ":") {
			t.Errorf("resolveUser on linux = %q, want uid:gid", got)
		}
	})
}

func TestPidsLimitPtr(t *testing.T) {
	if got := pidsLimitPtr(4096); got == nil || *got != 4096 {
		t.Errorf("pidsLimitPtr(4096) = %v", got)
	}
	if got := pidsLimitPtr(-1); got == nil || *got != -1 {
		t.Errorf("pidsLimitPtr(-1) = %v, want unlimited", got)
	}
}

func TestIsNotFound(t *testing.T) {
	if isNotFound(nil) {
		t.Error("nil is not a not-found error")
	}
	if isNotFound(errors.New("boom")) {
		t.Error("a plain error is not a not-found error")
	}
}

func TestErrorFormatting(t *testing.T) {
	tests := map[string]struct {
		err  *Error
		want string
	}{
		"message and cause": {
			&Error{Code: protocol.ErrExecutor, Msg: "start", Err: errors.New("boom")},
			"E_EXECUTOR: start: boom"},
		"message only": {&Error{Code: protocol.ErrCache, Msg: "locked"}, "E_CACHE: locked"},
		"cause only":   {&Error{Code: protocol.ErrArtifact, Err: errors.New("boom")}, "E_ARTIFACT: boom"},
		"code only":    {&Error{Code: protocol.ErrStep}, "E_STEP"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}

	if CodeOf(errors.New("not ours")) != "" {
		t.Error("CodeOf invented a code for a foreign error")
	}
	if !errors.Is(wrapErr(protocol.ErrExecutor, context.Canceled, "x"), context.Canceled) {
		t.Error("wrapErr broke errors.Is on the cause")
	}
}

func TestExecutorClose_ReleasesTheDaemonConnection(t *testing.T) {
	f := &fakeAPI{}
	e, _ := newExecutor(t, f, nil)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !f.closed {
		t.Error("the client was not closed")
	}
}

// Reconciliation removes containers it did not create in this process, so
// Remove works from an id alone and treats "already gone" as done.
func TestRemove(t *testing.T) {
	f := &fakeAPI{}
	e, _ := newExecutor(t, f, nil)
	if err := e.Remove(context.Background(), "container-id"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed %d containers, want 1", len(f.removed))
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo\n"); got != "one" {
		t.Errorf("firstLine = %q, want one", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q, want only", got)
	}
}

// CheckImage drives a throwaway container and turns its output into the
// contract report `latem doctor` prints.
func TestCheckImage_ReadsTheProbeOutput(t *testing.T) {
	f := &fakeAPI{execStdout: []string{strings.Join([]string{
		"passwd_name=runner", "group_name=runner",
		"home=/home/runner", "home_exists=1", "home_writable=1",
		"tool_bash=1", "tool_git=1", "ca=1", "",
	}, "\n")}}
	e, _ := newExecutor(t, f, nil)

	report, err := e.CheckImage(context.Background(), "ci:1")
	if err != nil {
		t.Fatalf("CheckImage: %v", err)
	}
	if !report.OK() {
		t.Errorf("a conforming image failed: %v", report.Failures())
	}
	// The probe must run as the uid a real step will, or $HOME resolves
	// differently here than it will in the run.
	if got := f.createdConfigs[0].User; got != "1000:1000" {
		t.Errorf("probe ran as %q, want 1000:1000", got)
	}
	if len(f.removed) != 1 {
		t.Errorf("the probe container was not removed")
	}
}
