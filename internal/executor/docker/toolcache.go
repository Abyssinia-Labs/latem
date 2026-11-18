package docker

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The runner's tool cache, filled by latem rather than by the engine (A109).
//
// A51 mounted one named volume per runner at /opt/hostedtoolcache and left
// the engine to fill it: Docker copies an image's directory into an empty
// named volume the first time a container mounts it, and never again. Two
// things then go wrong, and both hide the image's own tool cache behind the
// volume. On 2026-11-16 the MacBook's first fill was cut short (Kiri, with the
// exec race of LAT-23) and left 184 KB of one Python; and an image rebuilt
// with a new Node keeps the old volume. latem-ubuntu's /usr/local/node is a
// link into /opt/hostedtoolcache, so either way it dangled and every
// JavaScript action failed at its first step with exit 127.
//
// So the volume is one per image, named for the image's id, and latem fills
// it itself before the run's container starts: a probe from the run's image,
// as root, copies the image's directory into the volume mounted elsewhere and
// writes a marker naming the image last. A volume without that marker was
// never finished and is emptied and filled again. The run's mounts say
// NoCopy, so the engine never starts a fill of its own.
const (
	// ToolCacheVolumePrefix and the runner's name are the runner's name for
	// its tool cache ([executor.Spec.ToolCache]), which A51 used as the
	// volume's and A109 begins each volume's with. Not the cache volumes'
	// prefix, so §14.4's pruning, which is about caches, leaves them alone.
	ToolCacheVolumePrefix = "latem-toolcache-"

	// LabelToolCache names the runner's tool cache a volume belongs to (the
	// name [executor.Spec.ToolCache] gives), and LabelToolCacheImage the id of
	// the image it is filled from. Both are set on every tool cache volume
	// created since A109; a volume with neither is A51's.
	LabelToolCache      = "latem.toolcache"
	LabelToolCacheImage = "latem.toolcache.image"

	// ToolCacheMarker is the file, at the top of the tool cache, whose content
	// is the id of the image the volume was filled from. It is written after
	// everything else, so its presence means the fill finished.
	ToolCacheMarker = ".latem-toolcache"

	// toolCacheFillPath is where the fill probe mounts the volume, so that
	// the image's own /opt/hostedtoolcache stays visible to copy from.
	toolCacheFillPath = "/latem-toolcache"
)

// toolCacheFillScript fills the volume at $LATEM_TOOLCACHE_DST from the
// directory at $LATEM_TOOLCACHE_SRC, unless the marker already names
// $LATEM_TOOLCACHE_IMAGE. A compile-time constant handed to the shell as one
// argv element, its inputs in the environment (INV-3). POSIX sh, cp -a and
// rm: busybox, coreutils and a Mac (which runs it in the tests) alike.
//
// Whatever the volume holds without the right marker is removed first: a fill
// cut short, or one from another image, leaves files that copying over would
// not take away. The two syncs put the copy on disk before the marker, and
// the marker before the answer.
const toolCacheFillScript = `set -u
src=$LATEM_TOOLCACHE_SRC dst=$LATEM_TOOLCACHE_DST want=$LATEM_TOOLCACHE_IMAGE
mark="$dst/` + ToolCacheMarker + `"
if [ "$(cat "$mark" 2>/dev/null)" = "$want" ]; then
	echo toolcache=ready
	exit 0
fi
rm -rf "$dst"/* "$dst"/.[!.]* "$dst"/..?* || exit 1
if [ -d "$src" ]; then
	cp -a "$src/." "$dst/" || exit 1
fi
sync
printf '%s\n' "$want" > "$mark.tmp" && mv -f "$mark.tmp" "$mark" || exit 1
sync
echo toolcache=filled
`

// toolCacheReadScript says what the marker names, for doctor.
const toolCacheReadScript = `cat "$LATEM_TOOLCACHE_DST/` + ToolCacheMarker + `" 2>/dev/null || true`

// ToolCacheVolume names the volume of a runner's tool cache for one image:
// the runner's name for it, then the first twelve hex digits of the image id.
func ToolCacheVolume(base, imageID string) string {
	hex := strings.TrimPrefix(imageID, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return base + "-" + hex
}

// toolCacheEnv is the fill and read scripts' environment.
func toolCacheEnv(imageID string) []string {
	return []string{
		"LATEM_TOOLCACHE_SRC=" + ToolCachePath,
		"LATEM_TOOLCACHE_DST=" + toolCacheFillPath,
		"LATEM_TOOLCACHE_IMAGE=" + imageID,
	}
}

// ensureToolCache returns the volume to mount at [ToolCachePath] for a run of
// img, created and filled.
//
// The fill is checked once per volume per agent: what was seen filled is
// remembered by the volume's creation time, which the engine reports with
// every create. A volume removed and made again has another, and is checked
// again; an engine that reports none is checked every run. So a warm runner
// pays one image inspect and one volume create a run, as before, and a probe
// container only after a restart or a new image.
func (e *Executor) ensureToolCache(ctx context.Context, base, img string) (string, error) {
	inspected, err := e.api.ImageInspect(ctx, img)
	if err != nil {
		return "", wrapErr(protocol.ErrCache, err, "tool cache: inspect image %q", img)
	}
	if inspected.ID == "" {
		return "", codeErr(protocol.ErrCache, "tool cache: the engine gave image %q no id", img)
	}
	name := ToolCacheVolume(base, inspected.ID)

	e.toolCacheMu.Lock()
	defer e.toolCacheMu.Unlock()

	vol, err := e.api.VolumeCreate(ctx, volume.CreateOptions{
		Name: name,
		Labels: map[string]string{
			LabelManaged: "true", LabelToolCache: base, LabelToolCacheImage: inspected.ID,
		},
	})
	if err != nil {
		return "", wrapErr(protocol.ErrCache, err, "tool cache volume %q", name)
	}
	if vol.CreatedAt != "" && e.toolCacheSeen[name] == vol.CreatedAt {
		return name, nil
	}

	res, err := e.runProbe(ctx, probeSpec{
		image: img,
		// As root, which can read all of the image's directory and write the
		// volume whoever owns it.
		user:       "",
		entrypoint: []string{"/bin/sh", "-c", toolCacheFillScript},
		env:        toolCacheEnv(inspected.ID),
		mutate: func(hc *container.HostConfig) {
			hc.Mounts = append(hc.Mounts, mount.Mount{
				Type: mount.TypeVolume, Source: name, Target: toolCacheFillPath,
				VolumeOptions: &mount.VolumeOptions{NoCopy: true},
			})
		},
	})
	if err != nil {
		return "", wrapErr(protocol.ErrCache, err, "filling tool cache volume %q from %q", name, img)
	}
	if res.exitCode != 0 {
		return "", codeErr(protocol.ErrCache, "filling tool cache volume %q from %q (exit %d): %s",
			name, img, res.exitCode, firstLine(res.output))
	}
	if vol.CreatedAt != "" {
		e.toolCacheSeen[name] = vol.CreatedAt
	}
	if strings.Contains(res.output, "toolcache=filled") {
		// A new fill is when an older one is most likely to have been left
		// behind: by a rebuilt image, or by A51's single volume. A volume
		// that will not go is no reason to fail this run; the reap tries
		// again and its caller logs what it could not remove.
		_, _ = e.pruneToolCaches(ctx, base)
	}
	return name, nil
}

// pruneToolCaches removes the tool cache volumes no run will mount again: A51's
// volume named base itself, and every volume filled from an image the engine
// no longer has or has left with neither tag nor digest, as a rebuild leaves
// the image it replaced. Base empty means every runner's. A volume a running
// container holds is left for the next reap. It returns what it removed and
// the first failure that was not the volume being in use.
func (e *Executor) pruneToolCaches(ctx context.Context, base string) ([]executor.Reaped, error) {
	label := LabelToolCache
	if base != "" {
		label += "=" + base
	}
	vols, err := e.api.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", label)),
	})
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "list tool cache volumes")
	}
	var doomed []string
	for _, v := range vols.Volumes {
		if v != nil && e.toolCacheImageGone(ctx, v.Labels[LabelToolCacheImage]) {
			doomed = append(doomed, v.Name)
		}
	}
	if base != "" {
		if legacy, ok := e.legacyToolCache(ctx, base); ok {
			doomed = append(doomed, legacy)
		}
	}

	var (
		removed  []executor.Reaped
		firstErr error
	)
	for _, name := range doomed {
		// Not forced: a volume in use is a run still on it.
		err := e.api.VolumeRemove(ctx, name, false)
		switch {
		case err == nil, isNotFound(err):
			removed = append(removed, executor.Reaped{Kind: "toolcache", Ref: name})
		case isConflict(err):
		default:
			if firstErr == nil {
				firstErr = wrapErr(protocol.ErrExecutor, err, "remove tool cache volume %s", name)
			}
		}
	}
	return removed, firstErr
}

// toolCacheImageGone reports whether no run will use the image a tool cache
// volume was filled from again. An unreadable label counts as gone: the
// volume is no run's.
func (e *Executor) toolCacheImageGone(ctx context.Context, id string) bool {
	if id == "" {
		return true
	}
	img, err := e.api.ImageInspect(ctx, id)
	if err != nil {
		return isNotFound(err)
	}
	return len(img.RepoTags) == 0 && len(img.RepoDigests) == 0
}

// legacyToolCache finds A51's volume for base: named base exactly, made by
// latem, with no image label.
func (e *Executor) legacyToolCache(ctx context.Context, base string) (string, bool) {
	vols, err := e.api.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", base), filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return "", false
	}
	for _, v := range vols.Volumes {
		if v != nil && v.Name == base && v.Labels[LabelToolCacheImage] == "" {
			return v.Name, true
		}
	}
	return "", false
}

// ToolCacheState is what [Executor.CheckToolCache] found for one runner.
type ToolCacheState struct {
	// Image is the image's id, and Volume the volume its next run mounts.
	Image  string
	Volume string
	// Exists says the volume is there; Filled that its marker names Image.
	// A volume that exists and is not filled had its fill cut short, and the
	// next run empties and fills it.
	Exists bool
	Filled bool
	// Stale names the runner's tool cache volumes no run will mount again:
	// A51's, and those of images since rebuilt or removed. The agent removes
	// them at its next fill or reap.
	Stale []string
}

// CheckToolCache says what a run of img on the runner whose tool cache is base
// will mount, and whether it is filled, without creating or changing
// anything. It is doctor's (A109).
func (e *Executor) CheckToolCache(ctx context.Context, base, img string) (ToolCacheState, error) {
	inspected, err := e.api.ImageInspect(ctx, img)
	if err != nil {
		return ToolCacheState{}, wrapErr(protocol.ErrCache, err, "inspect image %q", img)
	}
	st := ToolCacheState{Image: inspected.ID, Volume: ToolCacheVolume(base, inspected.ID)}

	vols, err := e.api.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelToolCache+"="+base)),
	})
	if err != nil {
		return st, wrapErr(protocol.ErrExecutor, err, "list tool cache volumes")
	}
	for _, v := range vols.Volumes {
		switch {
		case v == nil:
		case v.Name == st.Volume:
			st.Exists = true
		case e.toolCacheImageGone(ctx, v.Labels[LabelToolCacheImage]):
			st.Stale = append(st.Stale, v.Name)
		}
	}
	if legacy, ok := e.legacyToolCache(ctx, base); ok {
		st.Stale = append(st.Stale, legacy)
	}
	if !st.Exists {
		return st, nil
	}

	res, err := e.runProbe(ctx, probeSpec{
		image:      img,
		user:       "",
		entrypoint: []string{"/bin/sh", "-c", toolCacheReadScript},
		env:        toolCacheEnv(inspected.ID),
		mutate: func(hc *container.HostConfig) {
			hc.Mounts = append(hc.Mounts, mount.Mount{
				Type: mount.TypeVolume, Source: st.Volume, Target: toolCacheFillPath,
				ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true},
			})
		},
	})
	if err != nil {
		return st, wrapErr(protocol.ErrCache, err, "reading tool cache volume %q", st.Volume)
	}
	st.Filled = strings.TrimSpace(res.output) == inspected.ID
	return st, nil
}
