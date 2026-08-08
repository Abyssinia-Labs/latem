package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// vmPrefix is the name every Tart VM the agent creates carries, from
// RUNNER_SPEC §13.2's `tart clone <base> latem-run-<run-id>`.
const vmPrefix = "latem-run-"

// Prune reclaims space per RUNNER_SPEC §14.4.
//
// Every sweep runs even if an earlier one failed, and the errors are joined at
// the end. Stopping at the first failure is the wrong shape for a disk
// reclaimer: the sweep most likely to fail is the one that shells out, and
// abandoning the four that would have freed 60 GB because `tart` is not
// installed leaves the machine exactly as full as it was.
//
// The [Report] is the deliverable as much as the free space is. A prune that
// silently drops a warm cache is indistinguishable, from the next build's
// perspective, from a bug, so callers should log what came back.
func (l *Local) Prune(ctx context.Context, opts PruneOptions) (Report, error) {
	if opts.MaxVolumeBytes <= 0 {
		opts.MaxVolumeBytes = DefaultMaxVolumeBytes
	}
	if opts.WorkspaceMaxAge <= 0 {
		opts.WorkspaceMaxAge = DefaultWorkspaceMaxAge
	}
	var (
		report Report
		errs   []error
	)
	for _, sweep := range []func(context.Context, PruneOptions, *Report) error{
		l.pruneBuildCache,
		l.pruneCaches,
		l.pruneWorkspaces,
		l.collectMirrors,
		l.reapVMs,
	} {
		if err := sweep(ctx, opts, &report); err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}
	if len(errs) > 0 {
		return report, cacheErr(errors.Join(errs...), "pruning")
	}
	return report, nil
}

// pruneBuildCache is §14.4's `docker buildx prune --filter until=168h`.
func (l *Local) pruneBuildCache(ctx context.Context, _ PruneOptions, report *Report) error {
	if l.opts.Docker == nil {
		return nil
	}
	freed, err := l.opts.Docker.BuildCachePrune(ctx, buildCacheKeep)
	report.BytesReclaimed += freed
	return err
}

// pruneCaches enforces §14.4's per-cache ceiling.
//
// The two backends can be shrunk to different depths, and pretending otherwise
// would be worse than saying so:
//
// A directory cache is evicted from the inside. Its top-level entries are
// deleted by access time, oldest first, until the tree is under 80% of the cap
// — for a package store that means the dependencies nothing has read in months
// go and this week's stay, which is the whole point of a cache.
//
// A volume cache cannot be opened from the host at all: on Docker Desktop for
// macOS it lives inside the daemon's VM. Enumerating it would mean starting a
// container to do the deleting, which turns a prune into a build dependency —
// so an oversized volume is removed whole and the next run repopulates it. The
// cost is one cold build; the alternative is a volume that grows forever.
func (l *Local) pruneCaches(ctx context.Context, opts PruneOptions, report *Report) error {
	target := int64(float64(opts.MaxVolumeBytes) * evictTarget)

	infos, listErr := l.List(ctx)
	var errs []error
	if listErr != nil {
		errs = append(errs, listErr)
	}

	for _, info := range infos {
		if info.SizeBytes < 0 {
			continue // the daemon could not size it; "unknown" is not "over"
		}
		overCap := info.SizeBytes > opts.MaxVolumeBytes
		// Under Aggressive every cache is taken down to the target, not only
		// the ones over the ceiling: the machine is below a runner's floor and
		// the next run is being refused with E_DISK.
		overTarget := opts.Aggressive && info.SizeBytes > target
		if !overCap && !overTarget {
			continue
		}

		volumeBacked := l.volumeBacked(info.Runner)
		if l.locks.busy(l.paths.lockPath(info.Runner, info.Name, volumeBacked)) {
			// A run holds this cache. Evicting under it produces exactly the
			// intermittent corruption §14.3 exists to prevent, and it would
			// look like a flaky build rather than a bug in the pruner.
			continue
		}

		if !volumeBacked {
			freed, err := evictDir(info.Source, target)
			if err != nil {
				errs = append(errs, err)
			}
			if freed > 0 {
				report.BytesReclaimed += freed
				report.VolumesEvicted = append(report.VolumesEvicted, info.Source)
			}
			continue
		}

		if err := l.opts.Docker.VolumeRemove(ctx, info.Source); err != nil {
			errs = append(errs, err)
			continue
		}
		report.BytesReclaimed += info.SizeBytes
		report.VolumesEvicted = append(report.VolumesEvicted, info.Source)
	}
	return errors.Join(errs...)
}

// evictDir deletes top-level entries by access time until the tree is under
// target, returning the bytes freed.
//
// Top-level only, and deliberately. The entries of a package store are self-
// contained units — one version of one package — and deleting individual files
// from inside one leaves a corrupt entry that the tool will neither use nor
// re-download. Coarse eviction is correct eviction here.
func evictDir(dir string, target int64) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("cache: reading %s: %w", dir, err)
	}

	type candidate struct {
		path string
		size int64
		used time.Time
	}
	var (
		cands []candidate
		total int64
	)
	for _, e := range entries {
		if e.Name() == lockFileName {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue // vanished between ReadDir and here
		}
		size := info.Size()
		if e.IsDir() {
			size = treeSize(path)
		}
		total += size
		cands = append(cands, candidate{path: path, size: size, used: accessTime(info)})
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].used.Before(cands[j].used) })

	var freed int64
	for _, c := range cands {
		if total <= target {
			break
		}
		if err := os.RemoveAll(c.path); err != nil {
			return freed, fmt.Errorf("cache: evicting %s: %w", c.path, err)
		}
		total -= c.size
		freed += c.size
	}
	return freed, nil
}

// pruneWorkspaces deletes §14.4's stale clones under work/.
//
// Nothing else needs cleaning: §12's workspaces are `git clone --local` copies
// that leave no administrative state behind in the mirror, so deleting the
// directory is the whole operation.
func (l *Local) pruneWorkspaces(_ context.Context, opts PruneOptions, report *Report) error {
	entries, err := os.ReadDir(l.paths.work())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cache: reading %s: %w", l.paths.work(), err)
	}

	now := l.opts.Now()
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || isDotName(e.Name()) {
			continue
		}
		runID := e.Name()
		if l.opts.Live(runID) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Under Aggressive the age filter is dropped entirely: free disk has
		// fallen below a runner's floor, the machine is already refusing runs
		// with E_DISK, and a workspace nothing is reading is worth more as
		// free space than as a debugging aid.
		if !opts.Aggressive && now.Sub(info.ModTime()) < opts.WorkspaceMaxAge {
			continue
		}

		path := filepath.Join(l.paths.work(), runID)
		size := treeSize(path)
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("cache: removing workspace %s: %w", path, err))
			continue
		}
		report.BytesReclaimed += size
		report.WorkspacesGone = append(report.WorkspacesGone, runID)
	}
	return errors.Join(errs...)
}

// collectMirrors is §14.4's `git gc --prune=30.days.ago` per mirror.
//
// Mirrors run with gc.auto disabled (§12, finding F11) so that automatic gc
// cannot collect the commit a build is checking out after a force-push. That
// makes explicit gc this function's job — and makes skipping a mirror a live
// run is reading not an optimisation but the same correctness rule.
func (l *Local) collectMirrors(ctx context.Context, _ PruneOptions, report *Report) error {
	entries, err := os.ReadDir(l.paths.mirrors())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cache: reading %s: %w", l.paths.mirrors(), err)
	}

	var errs []error
	for _, e := range entries {
		if !e.IsDir() || isDotName(e.Name()) {
			continue
		}
		mirror := filepath.Join(l.paths.mirrors(), e.Name())
		if l.opts.MirrorBusy(mirror) {
			continue
		}

		before := treeSize(mirror)
		out, err := l.opts.Exec(ctx, l.opts.Git, "-C", mirror, "gc", mirrorGCPrune)
		if err != nil {
			errs = append(errs, fmt.Errorf("cache: git gc in %s: %w: %s", mirror, err, strings.TrimSpace(string(out))))
			continue
		}
		if freed := before - treeSize(mirror); freed > 0 {
			report.BytesReclaimed += freed
		}
		report.MirrorsCollected = append(report.MirrorsCollected, e.Name())
	}
	return errors.Join(errs...)
}

// tartVM is one entry of `tart list --format json`.
type tartVM struct {
	Name string `json:"Name"`
	// Size is the VM's on-disk size in gigabytes; Disk is what it may grow to.
	// Tart reports both as whole GB, which is precise enough for a reclaim
	// figure measured in tens of them.
	Size int `json:"Size"`
	Disk int `json:"Disk"`
}

// reapVMs deletes leaked Tart VMs, §14.4's most valuable sweep by volume.
//
// A leaked VM carrying an Xcode image costs ~90 GB, which is the fastest way
// to fill a Mac mini. They leak whenever the agent dies between `tart clone`
// and `tart delete` — a crash, a kill -9, a power cut — so this is not an edge
// case, it is what happens the first time someone restarts the agent during a
// build.
func (l *Local) reapVMs(ctx context.Context, _ PruneOptions, report *Report) error {
	if !l.hasTartRunner() {
		return nil
	}

	out, err := l.opts.Exec(ctx, l.opts.Tart, "list", "--format", "json")
	if err != nil {
		return fmt.Errorf("cache: tart list: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var vms []tartVM
	if err := json.Unmarshal(out, &vms); err != nil {
		return fmt.Errorf("cache: parsing tart list output: %w", err)
	}

	var errs []error
	for _, vm := range vms {
		runID, ok := strings.CutPrefix(vm.Name, vmPrefix)
		if !ok || runID == "" {
			continue // not ours; leave it alone
		}
		if l.opts.Live(runID) {
			continue
		}
		if out, err := l.opts.Exec(ctx, l.opts.Tart, "delete", vm.Name); err != nil {
			errs = append(errs, fmt.Errorf("cache: tart delete %s: %w: %s", vm.Name, err, strings.TrimSpace(string(out))))
			continue
		}
		size := vm.Size
		if size == 0 {
			size = vm.Disk
		}
		report.BytesReclaimed += int64(size) << 30
		report.VMsReaped = append(report.VMsReaped, vm.Name)
	}
	return errors.Join(errs...)
}

func (l *Local) hasTartRunner() bool {
	for _, cfg := range l.opts.Runners {
		if cfg.Executor == ExecutorTart {
			return true
		}
	}
	return false
}

// StaleMirrors reports mirrors untouched for longer than age.
//
// It reports and never deletes, per §14.4: a mirror may be the only local copy
// of a repository whose origin has been renamed, made private, or deleted, and
// an automatic sweep that reclaims 3 GB by destroying the last copy of
// someone's code is not a trade the agent gets to make on its own. `latem
// doctor` surfaces this so a human can.
func (l *Local) StaleMirrors(age time.Duration) ([]string, error) {
	if age <= 0 {
		age = StaleMirrorAge
	}
	entries, err := os.ReadDir(l.paths.mirrors())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: reading %s: %w", l.paths.mirrors(), err)
	}

	now := l.opts.Now()
	var stale []string
	for _, e := range entries {
		if !e.IsDir() || isDotName(e.Name()) {
			continue
		}
		if now.Sub(l.mirrorTouched(filepath.Join(l.paths.mirrors(), e.Name()))) > age {
			stale = append(stale, e.Name())
		}
	}
	return stale, nil
}

// mirrorTouched is when a mirror last saw a fetch.
//
// FETCH_HEAD is rewritten by every `git fetch`, which is the only thing the
// agent does to a mirror, so its mtime answers the question directly. The
// directory's own mtime does not: it changes when a lock file is created and
// removed, so a mirror nothing has fetched in a year can look fresh.
func (l *Local) mirrorTouched(mirror string) time.Time {
	if info, err := os.Stat(filepath.Join(mirror, "FETCH_HEAD")); err == nil {
		return info.ModTime()
	}
	if info, err := os.Stat(mirror); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// treeSize sums the apparent size of a tree, best effort.
//
// Apparent rather than allocated: it is what `du --apparent-size` reports and
// what a cache cap is naturally expressed in. Errors are swallowed because a
// file that vanishes mid-walk — which is normal, a build is probably running —
// must not fail a sweep whose entire purpose is to free space.
func treeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // a vanished entry is not a reason to stop
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
