package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeDocker stands in for the daemon. Everything the cache subsystem does to
// Docker is four calls; running a container to test them would make `go test
// ./...` require a daemon for no extra confidence. The real adapter is
// exercised by the integration build.
type fakeDocker struct {
	mu         sync.Mutex
	volumes    map[string]*VolumeInfo
	removed    []string
	buildFreed int64
	buildKeep  time.Duration
	createErr  error
	listErr    error
	removeErr  error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{volumes: map[string]*VolumeInfo{}}
}

func (f *fakeDocker) VolumeEnsure(_ context.Context, name string, labels map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.volumes[name]; !ok {
		f.volumes[name] = &VolumeInfo{
			Name:   name,
			Runner: labels[LabelRunner],
			Cache:  labels[LabelCache],
		}
	}
	return nil
}

func (f *fakeDocker) VolumeList(context.Context) ([]VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]VolumeInfo, 0, len(f.volumes))
	for _, v := range f.volumes {
		out = append(out, *v)
	}
	return out, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.volumes, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeDocker) BuildCachePrune(_ context.Context, keep time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buildKeep = keep
	return f.buildFreed, nil
}

func TestNew_RequiresAnAbsoluteRoot(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted an empty root")
	}
	if _, err := New(Options{Root: "relative/path"}); err == nil {
		t.Error("New accepted a relative root; every cache path would depend on the working directory")
	}
}

// RUNNER_SPEC §14.2: host directories for tart and native, one per
// (runner, cache).
func TestAcquire_DirectoryBackedRunner(t *testing.T) {
	root := t.TempDir()
	l := newLocal(t, root, time.Second)

	mounts, err := l.Acquire(context.Background(), "linux", []Request{{Name: "pnpm", Target: "/cache/pnpm"}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "caches", "linux", "pnpm")
	if mounts[0].Source != want {
		t.Errorf("Source = %q, want %q", mounts[0].Source, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the cache directory was not created: %v", err)
	}
	// A cache holds a repository's dependency tree, and Gradle and CocoaPods
	// both write credentials into theirs. It is not other users' business.
	if info, _ := os.Stat(want); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("cache directory mode = %o, want owner-only", info.Mode().Perm())
	}
	// Target comes through on the Request (amendment A11). Before it did, this
	// returned an empty Target and the mount silently did nothing — the build
	// simply ran cold forever, with no error anywhere to explain it.
	if mounts[0].Target != "/cache/pnpm" {
		t.Errorf("Target = %q, want the requested mount point", mounts[0].Target)
	}
}

// A cache with no mount point is refused rather than mounted nowhere.
func TestAcquire_RejectsRequestWithNoTarget(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)

	_, err := l.Acquire(context.Background(), "linux", []Request{{Name: "pnpm"}})
	if err == nil {
		t.Fatal("accepted a cache request with no mount target")
	}
	if CodeOf(err) != protocol.ErrCache {
		t.Errorf("code = %s, want %s", CodeOf(err), protocol.ErrCache)
	}
}

// RUNNER_SPEC §14.1: one named volume per (runner, cache).
func TestAcquire_VolumeBackedRunner(t *testing.T) {
	docker := newFakeDocker()
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })

	mounts, err := l.Acquire(context.Background(), "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}})
	if err != nil {
		t.Fatal(err)
	}
	if mounts[0].Source != "latem-cache-docker-gradle" {
		t.Errorf("Source = %q, want the §14.1 volume name", mounts[0].Source)
	}

	v := docker.volumes["latem-cache-docker-gradle"]
	if v == nil {
		t.Fatal("no volume was created")
	}
	// The name cannot be parsed back into (runner, cache) — both may contain a
	// hyphen — so the labels are what a prune reads.
	if v.Runner != "docker" || v.Cache != "gradle" {
		t.Errorf("labels = %+v, want the runner and cache recorded", v)
	}
}

// A docker runner with no daemon must fail rather than quietly handing the run
// a host directory the container will never see. The build would run cold
// forever and nobody would notice.
func TestAcquire_DockerRunnerWithoutADaemonFails(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)

	_, err := l.Acquire(context.Background(), "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}})
	if err == nil {
		t.Fatal("Acquire silently substituted a directory for a volume")
	}
	if CodeOf(err) != protocol.ErrCache {
		t.Errorf("CodeOf(err) = %q, want %s", CodeOf(err), protocol.ErrCache)
	}
}

func TestAcquire_ReportsVolumeCreationFailures(t *testing.T) {
	docker := newFakeDocker()
	docker.createErr = errors.New("daemon is not running")
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })

	_, err := l.Acquire(context.Background(), "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}})
	if CodeOf(err) != protocol.ErrCache {
		t.Fatalf("err = %v, CodeOf = %q, want %s", err, CodeOf(err), protocol.ErrCache)
	}
}

func TestList_ReportsBothBackends(t *testing.T) {
	root := t.TempDir()
	docker := newFakeDocker()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := newLocal(t, root, time.Second, func(o *Options) {
		o.Docker = docker
		o.Now = func() time.Time { return now }
	})
	ctx := context.Background()

	if _, err := l.Acquire(ctx, "linux", []Request{{Name: "pnpm", Target: "/cache/pnpm"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(ctx, "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "caches", "linux", "pnpm", "blob"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	docker.volumes["latem-cache-docker-gradle"].SizeBytes = 4096

	infos, err := l.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos = %+v, want both backends", infos)
	}

	byName := map[string]Info{}
	for _, i := range infos {
		byName[i.Name] = i
	}
	if got := byName["pnpm"]; got.SizeBytes != 512 || got.Runner != "linux" {
		t.Errorf("pnpm = %+v", got)
	}
	if got := byName["gradle"]; got.SizeBytes != 4096 || got.Runner != "docker" {
		t.Errorf("gradle = %+v", got)
	}
	// "Last used" is our own stamp, written at Acquire. Directory atime is not
	// usable — noatime and relatime are the common mount options, and a
	// volume's atime lives inside the daemon's filesystem entirely.
	for _, i := range infos {
		if !i.LastUsedAt.Equal(now) {
			t.Errorf("%s LastUsedAt = %v, want %v", i.Name, i.LastUsedAt, now)
		}
	}
}

// An operator running `latem cache list` with a dead daemon should see the
// directory caches and an explanation, not one error and no data.
func TestList_ReturnsWhatItCanWhenDockerFails(t *testing.T) {
	docker := newFakeDocker()
	docker.listErr = errors.New("daemon is not running")
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })
	ctx := context.Background()

	if _, err := l.Acquire(ctx, "linux", []Request{{Name: "pnpm", Target: "/cache/pnpm"}}); err != nil {
		t.Fatal(err)
	}

	infos, err := l.List(ctx)
	if err == nil {
		t.Error("the daemon failure was swallowed")
	}
	if len(infos) != 1 {
		t.Errorf("infos = %+v, want the directory caches anyway", infos)
	}
}

func TestList_EmptyBeforeAnyRun(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	infos, err := l.List(context.Background())
	if err != nil || len(infos) != 0 {
		t.Errorf("List = %v, %v", infos, err)
	}
}

// The disk gates at §10 steps 5 and 17 need a real number from statfs, and
// they need it before work/ exists on a fresh install.
func TestFreeGB_WorksBeforeTheWorkDirectoryExists(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)

	free, err := l.FreeGB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if free <= 0 {
		t.Errorf("FreeGB = %v, want a positive figure", free)
	}
}

func TestMinFreeGB_PerRunnerFloors(t *testing.T) {
	l, err := New(Options{
		Root: t.TempDir(),
		Runners: map[string]RunnerConfig{
			"linux":      {Executor: ExecutorDocker},
			"mac-native": {Executor: ExecutorNative},
			"ios":        {Executor: ExecutorTart},
			"custom":     {Executor: ExecutorDocker, MinFreeGB: 12},
			"unknown":    {},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Finding F12: draft 01's single global 20 GB was sized for containers and
	// would admit an iOS build onto a machine that cannot hold one.
	tests := map[string]float64{
		"linux":      DockerMinFreeGB,
		"mac-native": NativeMinFreeGB,
		"ios":        TartMinFreeGB,
		"custom":     12,
		"unknown":    TartMinFreeGB, // guessing low is the error that fails a build at 80%
	}
	for runner, want := range tests {
		if got := l.MinFreeGB(runner); got != want {
			t.Errorf("MinFreeGB(%q) = %v, want %v", runner, got, want)
		}
	}

	// Gate 5 runs before a runner is chosen, so it can only enforce the
	// weakest floor; gate 17 re-checks the selected runner's own.
	if got := l.AgentFloorGB(); got != 12 {
		t.Errorf("AgentFloorGB = %v, want the weakest floor", got)
	}
}

func TestCheckFree_RefusesBelowTheFloor(t *testing.T) {
	l, err := New(Options{
		Root:    t.TempDir(),
		Runners: map[string]RunnerConfig{"ios": {Executor: ExecutorTart, MinFreeGB: 1 << 20}},
	})
	if err != nil {
		t.Fatal(err)
	}

	e := l.CheckFree(context.Background(), "ios")
	if e == nil {
		t.Fatal("CheckFree admitted a run onto a machine with a petabyte floor")
	}
	if CodeOf(e) != protocol.ErrDisk {
		t.Errorf("CodeOf(err) = %q, want %s", CodeOf(e), protocol.ErrDisk)
	}
	if !errors.Is(e, ErrBelowFloor) {
		t.Errorf("err = %v, want ErrBelowFloor", e)
	}
}

func TestCheckFree_PassesOnARealisticFloor(t *testing.T) {
	l, err := New(Options{
		Root:    t.TempDir(),
		Runners: map[string]RunnerConfig{"linux": {Executor: ExecutorDocker, MinFreeGB: 0.000001}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckFree(context.Background(), "linux"); err != nil {
		t.Errorf("CheckFree = %v", err)
	}
	if err := l.CheckFree(context.Background(), ""); err != nil {
		t.Errorf("agent-wide CheckFree = %v", err)
	}
}

// A mount with no Target is provisioned, locked, and mounted nowhere. The
// build runs cold every time and nothing below this layer notices.
func TestWithTargets(t *testing.T) {
	mounts := []executor.CacheMount{{Name: "pnpm", Source: "/caches/pnpm"}}

	got, err := WithTargets(mounts, map[string]string{"pnpm": "/home/runner/.pnpm-store"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Target != "/home/runner/.pnpm-store" {
		t.Errorf("Target = %q", got[0].Target)
	}

	if _, err := WithTargets(mounts, nil); err == nil {
		t.Error("WithTargets accepted a cache with no declared mount path")
	}
}

func TestVolumeName(t *testing.T) {
	if got := VolumeName("mac-mini", "derived-data"); got != "latem-cache-mac-mini-derived-data" {
		t.Errorf("VolumeName = %q", got)
	}
}

func TestDefaultMinFreeGB(t *testing.T) {
	tests := map[string]float64{
		ExecutorDocker: 40,
		ExecutorNative: 60,
		ExecutorTart:   150,
		"":             150,
	}
	for executor, want := range tests {
		if got := DefaultMinFreeGB(executor); got != want {
			t.Errorf("DefaultMinFreeGB(%q) = %v, want %v", executor, got, want)
		}
	}
}
