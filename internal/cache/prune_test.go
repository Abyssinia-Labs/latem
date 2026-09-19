package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// recorder captures the git and tart command lines a prune would run, so the
// destructive sweeps are exercised on a machine with neither installed. These
// are the code paths that delete 90 GB VMs; "tested by hand, once" is not good
// enough for them.
type recorder struct {
	mu    sync.Mutex
	calls []string
	reply func(name string, args []string) ([]byte, error)
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	reply := r.reply
	r.mu.Unlock()
	if reply != nil {
		return reply(name, args)
	}
	return nil, nil
}

func (r *recorder) ran(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func writeSized(t *testing.T, path string, size int, stamp time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestPrune_DropsOldBuildLayers(t *testing.T) {
	docker := newFakeDocker()
	docker.buildFreed = 4096
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.BytesReclaimed != 4096 {
		t.Errorf("BytesReclaimed = %d, want the build cache figure", report.BytesReclaimed)
	}
	// §14.4 names the window: `buildx prune --filter until=168h`.
	if docker.buildKeep != 168*time.Hour {
		t.Errorf("keep = %v, want 168h", docker.buildKeep)
	}
}

// §14.4: evict by access time, oldest first, down to 80% of the cap. Access
// time rather than modification time because a package store is written once
// and read on every build — mtime would evict the dependency fifty builds have
// used since it was downloaded.
func TestPrune_EvictsInsideAnOversizedDirectoryCache(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := newLocal(t, root, time.Second, func(o *Options) { o.Now = func() time.Time { return now } })

	dir := filepath.Join(root, "caches", "linux", "pnpm")
	for i, name := range []string{"oldest", "older", "old", "recent", "newest"} {
		writeSized(t, filepath.Join(dir, name), 300, now.Add(-time.Duration(5-i)*24*time.Hour))
	}

	report, err := l.Prune(context.Background(), PruneOptions{MaxVolumeBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"oldest", "older", "old"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived eviction", gone)
		}
	}
	for _, kept := range []string{"recent", "newest"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was evicted before older entries", kept)
		}
	}
	if report.BytesReclaimed != 900 {
		t.Errorf("BytesReclaimed = %d, want 900", report.BytesReclaimed)
	}
	// The report is the deliverable as much as the space is: a prune that
	// silently drops a warm cache looks like a bug from the next build.
	if len(report.VolumesEvicted) != 1 || report.VolumesEvicted[0] != dir {
		t.Errorf("VolumesEvicted = %v", report.VolumesEvicted)
	}
}

func TestPrune_LeavesCachesUnderTheCapAlone(t *testing.T) {
	root := t.TempDir()
	l := newLocal(t, root, time.Second)
	dir := filepath.Join(root, "caches", "linux", "pnpm")
	writeSized(t, filepath.Join(dir, "entry"), 500, time.Now().Add(-500*time.Hour))

	report, err := l.Prune(context.Background(), PruneOptions{MaxVolumeBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if report.BytesReclaimed != 0 || len(report.VolumesEvicted) != 0 {
		t.Errorf("report = %+v, want an untouched cache", report)
	}
}

// Evicting under a running build produces exactly the intermittent corruption
// §14.3 exists to prevent — and from the daily ticker, so it would look like a
// flaky build rather than a bug in the pruner.
func TestPrune_SkipsACacheARunIsHolding(t *testing.T) {
	root := t.TempDir()
	holder := newLocal(t, root, time.Second)
	pruner := newLocal(t, root, time.Second)
	ctx := context.Background()

	if _, err := holder.Acquire(ctx, "linux", []Request{{Name: "pnpm", Target: "/cache/pnpm"}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "caches", "linux", "pnpm")
	writeSized(t, filepath.Join(dir, "entry"), 4000, time.Now().Add(-500*time.Hour))

	report, err := pruner.Prune(ctx, PruneOptions{MaxVolumeBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.VolumesEvicted) != 0 {
		t.Errorf("VolumesEvicted = %v, want the held cache left alone", report.VolumesEvicted)
	}
	if _, err := os.Stat(filepath.Join(dir, "entry")); err != nil {
		t.Error("a running build's cache was evicted from under it")
	}
}

// A volume cannot be opened from the host — on Docker Desktop for macOS it
// lives inside the daemon's VM — so an oversized one goes whole and the next
// run repopulates it.
func TestPrune_RemovesOversizedVolumesWhole(t *testing.T) {
	docker := newFakeDocker()
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })
	ctx := context.Background()

	if _, err := l.Acquire(ctx, "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx, "docker", []string{"gradle"}); err != nil {
		t.Fatal(err)
	}
	docker.volumes["latem-cache-docker-gradle"].SizeBytes = 30 << 30

	report, err := l.Prune(ctx, PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.VolumesEvicted) != 1 || report.VolumesEvicted[0] != "latem-cache-docker-gradle" {
		t.Fatalf("VolumesEvicted = %v", report.VolumesEvicted)
	}
	if report.BytesReclaimed != 30<<30 {
		t.Errorf("BytesReclaimed = %d", report.BytesReclaimed)
	}
}

// "Unknown" and "empty" lead to opposite decisions. A volume the daemon cannot
// size must not be evicted on the assumption that it is enormous, nor kept on
// the assumption that it is empty — it is simply not a candidate.
func TestPrune_IgnoresVolumesOfUnknownSize(t *testing.T) {
	docker := newFakeDocker()
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })
	ctx := context.Background()

	if _, err := l.Acquire(ctx, "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}}); err != nil {
		t.Fatal(err)
	}
	docker.volumes["latem-cache-docker-gradle"].SizeBytes = -1

	report, err := l.Prune(ctx, PruneOptions{MaxVolumeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.VolumesEvicted) != 0 {
		t.Errorf("VolumesEvicted = %v", report.VolumesEvicted)
	}
}

func TestPrune_StaleWorkspaces(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := newLocal(t, root, time.Second, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Live = func(runID string) bool { return runID == "run_live" }
	})

	for _, tc := range []struct {
		id  string
		age time.Duration
	}{
		{"run_old", 48 * time.Hour},
		{"run_fresh", time.Hour},
		{"run_live", 72 * time.Hour},
	} {
		dir := filepath.Join(root, "work", tc.id)
		writeSized(t, filepath.Join(dir, "file"), 100, now.Add(-tc.age))
		stamp := now.Add(-tc.age)
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.WorkspacesGone) != 1 || report.WorkspacesGone[0] != "run_old" {
		t.Fatalf("WorkspacesGone = %v, want only the stale one", report.WorkspacesGone)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "run_live")); err != nil {
		t.Error("a live run's workspace was deleted underneath it")
	}
	if _, err := os.Stat(filepath.Join(root, "work", "run_fresh")); err != nil {
		t.Error("an hour-old workspace was deleted")
	}
	if report.BytesReclaimed != 100 {
		t.Errorf("BytesReclaimed = %d", report.BytesReclaimed)
	}
}

// Below a runner's floor the machine is already refusing runs with E_DISK. A
// workspace nothing is reading is worth more as free space than as a
// debugging aid.
func TestPrune_AggressiveTakesFreshWorkspacesToo(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	l := newLocal(t, root, time.Second, func(o *Options) { o.Now = func() time.Time { return now } })
	writeSized(t, filepath.Join(root, "work", "run_fresh", "file"), 100, now)

	report, err := l.Prune(context.Background(), PruneOptions{Aggressive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.WorkspacesGone) != 1 {
		t.Errorf("WorkspacesGone = %v, want the fresh workspace under pressure", report.WorkspacesGone)
	}
}

// Mirrors run with gc.auto disabled (§12, F11) so nothing collects the commit
// a build is checking out after a force-push. Explicit gc is therefore this
// sweep's job, and skipping a busy mirror is the same correctness rule rather
// than an optimisation.
func TestPrune_CollectsIdleMirrorsAndSkipsBusyOnes(t *testing.T) {
	root := t.TempDir()
	rec := &recorder{}
	l := newLocal(t, root, time.Second, func(o *Options) {
		o.Exec = rec.run
		o.MirrorBusy = func(mirror string) bool { return strings.HasSuffix(mirror, "busy.git") }
	})

	for _, name := range []string{"idle.git", "busy.git"} {
		writeSized(t, filepath.Join(root, "mirrors", name, "FETCH_HEAD"), 10, time.Now())
	}

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !rec.ran("gc --prune=30.days.ago") {
		t.Errorf("no gc ran; calls = %v", rec.calls)
	}
	if rec.ran("busy.git") {
		t.Error("gc ran inside a mirror a live run is reading")
	}
	if len(report.MirrorsCollected) != 1 || report.MirrorsCollected[0] != "idle.git" {
		t.Errorf("MirrorsCollected = %v", report.MirrorsCollected)
	}
}

// A leaked VM grows toward its image's provisioned 140 GB, the fastest way to fill a
// Mac. They leak whenever the agent dies between `tart clone` and `tart
// delete`, which is what happens the first time someone restarts the agent
// during a build.
func TestPrune_ReapsLeakedTartVMs(t *testing.T) {
	rec := &recorder{
		reply: func(_ string, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "list" {
				return []byte(`[
					{"Name":"latem-run-abc123","Size":90,"Disk":120,"State":"stopped"},
					{"Name":"latem-run-live1","Size":40,"State":"running"},
					{"Name":"latem-xcode-26","Size":90,"State":"stopped"},
					{"Name":"someones-other-vm","Size":10,"State":"stopped"}
				]`), nil
			}
			return nil, nil
		},
	}
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) {
		o.Exec = rec.run
		o.Runners = map[string]RunnerConfig{"ios": {Executor: ExecutorTart}}
		o.Live = func(runID string) bool { return runID == "live1" }
	})

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.VMsReaped) != 1 || report.VMsReaped[0] != "latem-run-abc123" {
		t.Fatalf("VMsReaped = %v", report.VMsReaped)
	}
	if !rec.ran("delete latem-run-abc123") {
		t.Errorf("the leaked VM was reported but not deleted; calls = %v", rec.calls)
	}
	if rec.ran("delete latem-run-live1") {
		t.Error("a live run's VM was deleted mid-build")
	}
	if rec.ran("delete latem-xcode-26") {
		t.Error("the base image was deleted; a rebuild costs hours")
	}
	if rec.ran("delete someones-other-vm") {
		t.Error("a VM the agent did not create was deleted")
	}
	if report.BytesReclaimed != 90<<30 {
		t.Errorf("BytesReclaimed = %d, want 90 GB", report.BytesReclaimed)
	}
}

// Shelling out to a binary that is not there, on a machine with no Tart runner
// configured, is noise in every prune log on every Linux host.
func TestPrune_DoesNotInvokeTartWithoutATartRunner(t *testing.T) {
	rec := &recorder{}
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Exec = rec.run })

	if _, err := l.Prune(context.Background(), PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if rec.ran("list") {
		t.Errorf("tart was invoked on a host with no tart runner: %v", rec.calls)
	}
}

// Abandoning the four sweeps that would have freed 60 GB because `tart` is not
// installed leaves the machine exactly as full as it was.
func TestPrune_ContinuesAfterASweepFails(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	rec := &recorder{
		reply: func(name string, _ []string) ([]byte, error) {
			if strings.Contains(name, "tart") {
				return []byte("command not found"), errors.New("exec: tart: not found")
			}
			return nil, nil
		},
	}
	l := newLocal(t, root, time.Second, func(o *Options) {
		o.Exec = rec.run
		o.Now = func() time.Time { return now }
		o.Runners = map[string]RunnerConfig{"ios": {Executor: ExecutorTart}}
	})
	writeSized(t, filepath.Join(root, "work", "run_old", "file"), 100, now.Add(-48*time.Hour))
	if err := os.Chtimes(filepath.Join(root, "work", "run_old"), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	report, err := l.Prune(context.Background(), PruneOptions{})
	if err == nil {
		t.Fatal("the tart failure was swallowed")
	}
	if CodeOf(err) != protocol.ErrCache {
		t.Errorf("CodeOf(err) = %q, want %s", CodeOf(err), protocol.ErrCache)
	}
	if len(report.WorkspacesGone) != 1 {
		t.Errorf("WorkspacesGone = %v, want the workspace sweep to have run anyway", report.WorkspacesGone)
	}
}

func TestPrune_MissingDirectoriesAreNotErrors(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	report, err := l.Prune(context.Background(), PruneOptions{})
	if err != nil {
		t.Fatalf("a prune on a fresh install should be a no-op: %v", err)
	}
	if report.BytesReclaimed != 0 {
		t.Errorf("report = %+v", report)
	}
}

// §14.4 reports mirrors untouched for 90 days and never deletes them: a mirror
// may be the only local copy of a repository whose origin has gone away.
func TestStaleMirrors(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	l := newLocal(t, root, time.Second, func(o *Options) { o.Now = func() time.Time { return now } })

	writeSized(t, filepath.Join(root, "mirrors", "fresh.git", "FETCH_HEAD"), 10, now.Add(-time.Hour))
	writeSized(t, filepath.Join(root, "mirrors", "forgotten.git", "FETCH_HEAD"), 10, now.Add(-200*24*time.Hour))

	stale, err := l.StaleMirrors(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "forgotten.git" {
		t.Fatalf("StaleMirrors = %v", stale)
	}
	if _, err := os.Stat(filepath.Join(root, "mirrors", "forgotten.git")); err != nil {
		t.Error("StaleMirrors deleted a mirror; it reports, it never removes")
	}
}

func TestStaleMirrors_NoMirrorDirectory(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	stale, err := l.StaleMirrors(time.Hour)
	if err != nil || len(stale) != 0 {
		t.Errorf("StaleMirrors = %v, %v", stale, err)
	}
}

func TestTreeSize(t *testing.T) {
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "a"), 100, time.Now())
	writeSized(t, filepath.Join(dir, "nested", "b"), 200, time.Now())

	if got := treeSize(dir); got != 300 {
		t.Errorf("treeSize = %d, want 300", got)
	}
	if got := treeSize(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("treeSize(missing) = %d", got)
	}
}

func TestPrune_HonoursContextCancellation(t *testing.T) {
	l := newLocal(t, t.TempDir(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.Prune(ctx, PruneOptions{}); err == nil {
		t.Fatal("Prune ignored a cancelled context")
	}
}

func TestPrune_ReportsVolumeRemovalFailures(t *testing.T) {
	docker := newFakeDocker()
	docker.removeErr = fmt.Errorf("volume is in use")
	l := newLocal(t, t.TempDir(), time.Second, func(o *Options) { o.Docker = docker })
	ctx := context.Background()

	if _, err := l.Acquire(ctx, "docker", []Request{{Name: "gradle", Target: "/cache/gradle"}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(ctx, "docker", []string{"gradle"}); err != nil {
		t.Fatal(err)
	}
	docker.volumes["latem-cache-docker-gradle"].SizeBytes = 30 << 30

	report, err := l.Prune(ctx, PruneOptions{})
	if err == nil {
		t.Fatal("a failed removal was reported as success")
	}
	if len(report.VolumesEvicted) != 0 {
		t.Errorf("VolumesEvicted = %v, want nothing claimed", report.VolumesEvicted)
	}
}
