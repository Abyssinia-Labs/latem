package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A109's two cases, as the fill script meets them. The script runs for real,
// under /bin/sh on this machine, against directories standing in for the
// image's /opt/hostedtoolcache and the volume. PATH is fixed, so nothing of
// the machine's own setup is read.

const (
	oldImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newImage = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// imageToolCache lays out what latem-ubuntu bakes into /opt/hostedtoolcache,
// with a node that prints its version, and returns the directory.
func imageToolCache(t *testing.T, nodeVersion string) string {
	t.Helper()
	src := t.TempDir()
	writeExec(t, filepath.Join(src, "node", nodeVersion, "x64", "bin", "node"), "#!/bin/sh\necho v"+nodeVersion+"\n")
	writeExec(t, filepath.Join(src, "node", nodeVersion+".complete"), "")
	writeExec(t, filepath.Join(src, "Python", "3.13.18", "x64", "bin", "python3"), "#!/bin/sh\necho 3.13.18\n")
	return src
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fill runs the fill script as the probe would, and returns what it printed.
func fill(t *testing.T, src, dst, imageID string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", toolCacheFillScript)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"LATEM_TOOLCACHE_SRC=" + src,
		"LATEM_TOOLCACHE_DST=" + dst,
		"LATEM_TOOLCACHE_IMAGE=" + imageID,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fill script: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// stepNode runs node the way a step finds it in latem-ubuntu: through a link,
// /usr/local/node, to node/<version>/<arch> in the tool cache as mounted.
func stepNode(t *testing.T, toolCache, nodeVersion string) (string, error) {
	t.Helper()
	link := filepath.Join(t.TempDir(), "usr-local-node")
	if err := os.Symlink(filepath.Join(toolCache, "node", nodeVersion, "x64"), link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(link, "bin", "node"))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func marker(t *testing.T, dst string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dst, ToolCacheMarker))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// What the MacBook's volume held on 2026-11-16: one Python's lib directory and
// nothing else. The step's node dangled. After the fill it runs.
func TestToolCacheFill_HalfFilledVolume(t *testing.T) {
	src := imageToolCache(t, "24.11.1")
	dst := t.TempDir()
	writeExec(t, filepath.Join(dst, "Python", "3.13.18", "arm64", "lib", "libpython3.13.so"), "partial")
	writeExec(t, filepath.Join(dst, ".partial-dotfile"), "x")

	if _, err := stepNode(t, dst, "24.11.1"); err == nil {
		t.Fatal("the half-filled volume should not have a node; the test is not modelling the bug")
	}
	if got := fill(t, src, dst, newImage); got != "toolcache=filled" {
		t.Errorf("fill said %q", got)
	}
	out, err := stepNode(t, dst, "24.11.1")
	if err != nil || out != "v24.11.1" {
		t.Fatalf("node in the step: %q, %v", out, err)
	}
	if marker(t, dst) != newImage {
		t.Errorf("marker = %q", marker(t, dst))
	}
	for _, gone := range []string{"Python/3.13.18/arm64", ".partial-dotfile"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s survived the fill: an unfinished fill is started over", gone)
		}
	}
}

// An image rebuilt with a new default Node: the volume filled from the old one
// has node 22, and /usr/local/node points at 24.
func TestToolCacheFill_VolumeFromAnOlderImage(t *testing.T) {
	dst := t.TempDir()
	fill(t, imageToolCache(t, "22.21.0"), dst, oldImage)
	if out, err := stepNode(t, dst, "22.21.0"); err != nil || out != "v22.21.0" {
		t.Fatalf("node from the old image: %q, %v", out, err)
	}

	if got := fill(t, imageToolCache(t, "24.11.1"), dst, newImage); got != "toolcache=filled" {
		t.Errorf("fill said %q", got)
	}
	out, err := stepNode(t, dst, "24.11.1")
	if err != nil || out != "v24.11.1" {
		t.Fatalf("node in the step: %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "node", "22.21.0")); err == nil {
		t.Error("the old image's node survived: the volume is the new image's, not a merge")
	}
	if marker(t, dst) != newImage {
		t.Errorf("marker = %q", marker(t, dst))
	}
}

// A filled volume is left as it is: what setup-* actions installed into it
// since is the point of having one.
func TestToolCacheFill_FilledVolumeIsLeftAlone(t *testing.T) {
	src := imageToolCache(t, "24.11.1")
	dst := t.TempDir()
	fill(t, src, dst, newImage)
	writeExec(t, filepath.Join(dst, "bun", "1.3.2", "x64", "bin", "bun"), "#!/bin/sh\n")

	if got := fill(t, src, dst, newImage); got != "toolcache=ready" {
		t.Errorf("fill said %q, want ready", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "bun", "1.3.2", "x64", "bin", "bun")); err != nil {
		t.Errorf("a tool installed by a step was removed: %v", err)
	}
}

// An image with no /opt/hostedtoolcache still gets a filled, empty volume, so
// it is not probed again.
func TestToolCacheFill_ImageWithNoToolCache(t *testing.T) {
	dst := t.TempDir()
	if got := fill(t, filepath.Join(t.TempDir(), "absent"), dst, newImage); got != "toolcache=filled" {
		t.Errorf("fill said %q", got)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 1 || entries[0].Name() != ToolCacheMarker {
		t.Errorf("volume holds %v, want only the marker", entries)
	}
}

func TestToolCacheVolume(t *testing.T) {
	if got := ToolCacheVolume("latem-toolcache-linux", newImage); got != "latem-toolcache-linux-222222222222" {
		t.Errorf("ToolCacheVolume = %q", got)
	}
}

// runHost is the host config of the run's own container, not a probe's.
func runHost(t *testing.T, f *fakeAPI, runID string) *container.HostConfig {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, n := range f.createdNames {
		if n == ContainerNamePrefix+runID {
			return f.createdHosts[i]
		}
	}
	t.Fatalf("no run container for %s among %v", runID, f.createdNames)
	return nil
}

// hasNoCopyVolume reports whether mounts has the named volume at target, with
// the engine's own fill turned off.
func hasNoCopyVolume(mounts []mount.Mount, name, target string) bool {
	return slices.ContainsFunc(mounts, func(m mount.Mount) bool {
		return m.Type == mount.TypeVolume && m.Source == name && m.Target == target &&
			m.VolumeOptions != nil && m.VolumeOptions.NoCopy
	})
}

// fillProbes are the fill probes created so far, in order.
func fillProbes(f *fakeAPI) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for i, c := range f.createdConfigs {
		if len(c.Entrypoint) == 3 && c.Entrypoint[2] == toolCacheFillScript {
			out = append(out, i)
		}
	}
	return out
}

func prepareToolCache(t *testing.T, e *Executor, runID string) (executor.Env, error) {
	t.Helper()
	env, err := e.Prepare(context.Background(), executor.Spec{
		RunID: runID, Workspace: t.TempDir(), ToolCache: "latem-toolcache-linux",
	})
	if err == nil {
		t.Cleanup(func() { _ = env.Close(context.Background()) })
	}
	return env, err
}

// The run's tool cache is the image's volume, filled by a probe from the
// image before the run's container is made, and mounted with NoCopy.
func TestPrepare_ToolCacheIsFilledBeforeTheRunContainer(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, imageID: newImage, volumeCreatedAt: "2026-11-17T10:00:00Z"}
	e, _ := newExecutor(t, f, nil)
	if _, err := prepareToolCache(t, e, "run-1"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := ToolCacheVolume("latem-toolcache-linux", newImage)

	probes := fillProbes(f)
	if len(probes) != 1 {
		t.Fatalf("fill probes = %d, want 1", len(probes))
	}
	cfg, host := f.createdConfigs[probes[0]], f.createdHosts[probes[0]]
	if cfg.Image != "test:latest" || cfg.User != "" {
		t.Errorf("fill probe image %q user %q, want the run's image as root", cfg.Image, cfg.User)
	}
	if !slices.Contains(cfg.Env, "LATEM_TOOLCACHE_IMAGE="+newImage) {
		t.Errorf("fill probe env = %v", cfg.Env)
	}
	if !hasNoCopyVolume(host.Mounts, want, toolCacheFillPath) {
		t.Errorf("fill probe mounts = %v", host.Mounts)
	}
	runIdx := slices.Index(f.createdNames, ContainerNamePrefix+"run-1")
	if runIdx < probes[0] {
		t.Error("the run container was made before its tool cache was filled")
	}
	if !hasNoCopyVolume(runHost(t, f, "run-1").Mounts, want, ToolCachePath) {
		t.Errorf("run mounts = %v", runHost(t, f, "run-1").Mounts)
	}

	var created *volume.CreateOptions
	for i := range f.volumes {
		if f.volumes[i].Name == want {
			created = &f.volumes[i]
		}
	}
	if created == nil || created.Labels[LabelToolCache] != "latem-toolcache-linux" ||
		created.Labels[LabelToolCacheImage] != newImage || created.Labels[LabelManaged] != "true" {
		t.Errorf("volume created as %+v", created)
	}
}

// A warm runner probes once per volume, not once per run; a volume made again
// (another creation time) is probed again, and so is every run on an engine
// that reports no creation time.
func TestPrepare_ToolCacheIsProbedOncePerVolume(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, volumeCreatedAt: "t1"}
	e, _ := newExecutor(t, f, nil)
	for _, id := range []string{"r1", "r2", "r3"} {
		if _, err := prepareToolCache(t, e, id); err != nil {
			t.Fatalf("Prepare %s: %v", id, err)
		}
	}
	if n := len(fillProbes(f)); n != 1 {
		t.Errorf("fill probes over three runs = %d, want 1", n)
	}

	f.volumeCreatedAt = "t2" // removed and made again
	if _, err := prepareToolCache(t, e, "r4"); err != nil {
		t.Fatal(err)
	}
	if n := len(fillProbes(f)); n != 2 {
		t.Errorf("a volume made again was not probed: %d probes", n)
	}

	f.volumeCreatedAt = ""
	for _, id := range []string{"r5", "r6"} {
		if _, err := prepareToolCache(t, e, id); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fillProbes(f)); n != 4 {
		t.Errorf("with no creation time, every run is probed: %d probes, want 4", n)
	}
}

// A rebuilt image is another volume, filled from it, and the old image's
// volume and A51's are removed once nothing names their image.
func TestPrepare_ARebuiltImageGetsItsOwnVolume(t *testing.T) {
	base := "latem-toolcache-linux"
	f := &fakeAPI{
		envFileMissing:  true,
		volumeCreatedAt: "t1",
		images: map[string]image.InspectResponse{
			"test:latest": {ID: oldImage, RepoTags: []string{"test:latest"}},
			oldImage:      {ID: oldImage, RepoTags: []string{"test:latest"}},
		},
	}
	e, _ := newExecutor(t, f, nil)
	if _, err := prepareToolCache(t, e, "r1"); err != nil {
		t.Fatal(err)
	}
	if got := runHost(t, f, "r1").Mounts; !slices.ContainsFunc(got, func(m mount.Mount) bool {
		return m.Source == ToolCacheVolume(base, oldImage)
	}) {
		t.Fatalf("first run mounts %v", got)
	}

	// The rebuild: the tag moves, the old image is left with none.
	f.images = map[string]image.InspectResponse{
		"test:latest": {ID: newImage, RepoTags: []string{"test:latest"}},
		newImage:      {ID: newImage, RepoTags: []string{"test:latest"}},
		oldImage:      {ID: oldImage},
		"sha256:3333": {ID: "sha256:3333", RepoTags: []string{"other:1"}},
	}
	f.listedVolumes = []*volume.Volume{
		{Name: ToolCacheVolume(base, oldImage), Labels: map[string]string{
			LabelManaged: "true", LabelToolCache: base, LabelToolCacheImage: oldImage}},
		{Name: ToolCacheVolume(base, "sha256:3333"), Labels: map[string]string{
			LabelManaged: "true", LabelToolCache: base, LabelToolCacheImage: "sha256:3333"}},
		{Name: base, Labels: map[string]string{LabelManaged: "true"}},
		{Name: "latem-toolcache-other", Labels: map[string]string{
			LabelManaged: "true", LabelToolCache: "latem-toolcache-other", LabelToolCacheImage: oldImage}},
	}
	f.probeLogs = []string{"toolcache=filled\n"}
	if _, err := prepareToolCache(t, e, "r2"); err != nil {
		t.Fatal(err)
	}
	if n := len(fillProbes(f)); n != 2 {
		t.Errorf("fill probes = %d, want the new image's volume filled", n)
	}
	if got := runHost(t, f, "r2").Mounts; !slices.ContainsFunc(got, func(m mount.Mount) bool {
		return m.Source == ToolCacheVolume(base, newImage) && m.Target == ToolCachePath
	}) {
		t.Fatalf("second run mounts %v", got)
	}
	wantRemoved := []string{ToolCacheVolume(base, oldImage), base}
	if !slices.Equal(f.removedVolumes, wantRemoved) {
		t.Errorf("removed %v, want %v: the tagged image's volume and another runner's stay", f.removedVolumes, wantRemoved)
	}
}

// A fill that fails fails the run as E_CACHE, before a run container exists,
// and takes the run's volume with it.
func TestPrepare_ToolCacheFillFails(t *testing.T) {
	f := &fakeAPI{envFileMissing: true, waitStatus: 1, probeLogs: []string{"cp: write error: No space left on device\n"}}
	e, _ := newExecutor(t, f, nil)
	_, err := prepareToolCache(t, e, "r1")
	if err == nil {
		t.Fatal("Prepare should have failed")
	}
	if CodeOf(err) != protocol.ErrCache || !strings.Contains(err.Error(), "No space left") {
		t.Errorf("err = %v (%s)", err, CodeOf(err))
	}
	if slices.Contains(f.createdNames, ContainerNamePrefix+"r1") {
		t.Error("a run container was made on an unfilled tool cache")
	}
	if !slices.Contains(f.removedVolumes, RunVolumePrefix+"r1") {
		t.Errorf("the run volume leaked: removed %v", f.removedVolumes)
	}
}

// Reap removes every runner's tool cache volumes whose image is gone or
// untagged, leaves a run's own volumes to their own rule, and passes over a
// volume in use without calling it a failure.
func TestReap_StaleToolCaches(t *testing.T) {
	f := &fakeAPI{
		images: map[string]image.InspectResponse{
			newImage: {ID: newImage, RepoTags: []string{"latem-ubuntu:local"}},
			oldImage: {ID: oldImage},
		},
		listedVolumes: []*volume.Volume{
			{Name: "latem-toolcache-a-111111111111", Labels: map[string]string{
				LabelToolCache: "latem-toolcache-a", LabelToolCacheImage: oldImage}},
			{Name: "latem-toolcache-a-222222222222", Labels: map[string]string{
				LabelToolCache: "latem-toolcache-a", LabelToolCacheImage: newImage}},
			{Name: "latem-toolcache-b-999999999999", Labels: map[string]string{
				LabelToolCache: "latem-toolcache-b", LabelToolCacheImage: "sha256:9999"}},
			{Name: "latem-toolcache-c-111111111111", Labels: map[string]string{
				LabelToolCache: "latem-toolcache-c", LabelToolCacheImage: oldImage}},
			{Name: RunVolumePrefix + "live", Labels: map[string]string{LabelRunID: "live", LabelRunVolume: "true"}},
		},
		volumeRemoveErrs: map[string]error{
			"latem-toolcache-c-111111111111": errdefs.Conflict(errors.New("volume is in use")),
		},
	}
	e, _ := newExecutor(t, f, nil)
	reaped, err := e.Reap(context.Background(), func(id string) bool { return id == "live" })
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	want := []string{"latem-toolcache-a-111111111111", "latem-toolcache-b-999999999999"}
	if !slices.Equal(f.removedVolumes, want) {
		t.Errorf("removed %v, want %v", f.removedVolumes, want)
	}
	var kinds []string
	for _, r := range reaped {
		kinds = append(kinds, r.Kind+":"+r.Ref)
	}
	if !slices.Contains(kinds, "toolcache:latem-toolcache-a-111111111111") {
		t.Errorf("reaped = %v", kinds)
	}
}

// doctor's view: the volume the next run mounts, whether its fill finished,
// and what is stale. It creates nothing.
func TestCheckToolCache(t *testing.T) {
	base := "latem-toolcache-linux"
	current := ToolCacheVolume(base, fakeImageID)
	volumes := func() []*volume.Volume {
		return []*volume.Volume{
			{Name: current, Labels: map[string]string{LabelToolCache: base, LabelToolCacheImage: fakeImageID}},
			{Name: base, Labels: map[string]string{LabelManaged: "true"}},
		}
	}

	t.Run("filled", func(t *testing.T) {
		f := &fakeAPI{listedVolumes: volumes(), probeLogs: []string{fakeImageID + "\n"}}
		e, _ := newExecutor(t, f, nil)
		st, err := e.CheckToolCache(context.Background(), base, "test:latest")
		if err != nil {
			t.Fatal(err)
		}
		if !st.Exists || !st.Filled || st.Volume != current || !slices.Equal(st.Stale, []string{base}) {
			t.Errorf("state = %+v", st)
		}
		if len(f.volumes) != 0 {
			t.Errorf("doctor created volumes: %v", f.volumes)
		}
		if host := f.createdHosts[0]; !host.Mounts[0].ReadOnly {
			t.Errorf("the read probe should mount the volume read-only: %+v", host.Mounts)
		}
	})
	t.Run("cut short", func(t *testing.T) {
		f := &fakeAPI{listedVolumes: volumes(), probeLogs: []string{""}}
		e, _ := newExecutor(t, f, nil)
		st, err := e.CheckToolCache(context.Background(), base, "test:latest")
		if err != nil {
			t.Fatal(err)
		}
		if !st.Exists || st.Filled {
			t.Errorf("state = %+v, want exists and not filled", st)
		}
	})
	t.Run("not yet made", func(t *testing.T) {
		f := &fakeAPI{}
		e, _ := newExecutor(t, f, nil)
		st, err := e.CheckToolCache(context.Background(), base, "test:latest")
		if err != nil {
			t.Fatal(err)
		}
		if st.Exists || len(f.createdConfigs) != 0 {
			t.Errorf("state = %+v, probes %d: a missing volume needs no probe", st, len(f.createdConfigs))
		}
	})
}
