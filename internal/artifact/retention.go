package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PruneOptions bounds the daily artifact sweep (RUNNER_SPEC §17).
type PruneOptions struct {
	// Now is the sweep's clock. The zero value uses time.Now.
	Now time.Time
	// BudgetBytes is the total artifact budget across all runs. Zero uses
	// [DefaultBudgetBytes].
	BudgetBytes int64
	// ExpiresAt reports a stored artifact's expiry, normally from the
	// artifacts table (§22), which holds one expires_at per (run_id, path)
	// because two declarations in one job may retain for different periods.
	// Nil means nothing expires by declaration and only the budget applies.
	ExpiresAt func(runID, relPath string) (time.Time, bool)
	// Live reports whether a run is still going. A run's artifacts are written
	// during `collecting`, so a sweep landing at the wrong moment could
	// otherwise delete a file the same run is still copying.
	Live func(runID string) bool
}

// PruneReport records what the sweep removed.
//
// It exists for the same reason logstream's does: retention that silently
// removes the build someone was about to download is indistinguishable from a
// bug, and TotalBytes is what `latem doctor` reports so an operator can see
// the budget filling before it starts evicting.
type PruneReport struct {
	// FilesExpired are "<run-id>/<path>" entries removed by declaration.
	FilesExpired []string
	// RunsEvicted are run ids removed whole to stay inside the budget.
	RunsEvicted []string
	BytesFreed  int64
	// TotalBytes is the footprint after the sweep.
	TotalBytes int64
}

// Prune enforces retention and the total artifact budget.
//
// Two passes, in order. Declared retention first, because it is what the job
// author asked for. The budget second, evicting whole runs oldest-first: a run
// is the unit an operator thinks in, and half a release's artifacts are worth
// less than none of them — you cannot install two thirds of an .ipa, but you
// can be misled by finding one.
func Prune(root string, opts PruneOptions) (PruneReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.BudgetBytes <= 0 {
		opts.BudgetBytes = DefaultBudgetBytes
	}
	if opts.Live == nil {
		opts.Live = func(string) bool { return false }
	}

	var report PruneReport
	dir := filepath.Join(root, "artifacts")

	runs, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, artifactErr(err, "reading %s", dir)
	}

	type runDir struct {
		id     string
		size   int64
		newest time.Time
		// protected marks a live run: it counts against the budget but is
		// never evicted, because it is being written to right now.
		protected bool
	}
	var (
		all   []*runDir
		total int64
		errs  []error
	)

	for _, e := range runs {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		path := filepath.Join(dir, id)
		if opts.Live(id) {
			size, newest, _ := measure(path)
			total += size
			all = append(all, &runDir{id: id, size: size, newest: newest, protected: true})
			continue // counted against the budget, never evicted
		}

		if opts.ExpiresAt != nil {
			freed, names, err := expire(path, id, opts)
			if err != nil {
				errs = append(errs, err)
			}
			report.BytesFreed += freed
			report.FilesExpired = append(report.FilesExpired, names...)
		}

		size, newest, files := measure(path)
		if files == 0 {
			// Every file expired, or none was ever written. Leaving the
			// directory tree behind would have `latem doctor` reporting runs
			// whose artifacts are all gone.
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, fmt.Errorf("artifact: removing %s: %w", path, err))
			}
			continue
		}
		total += size
		all = append(all, &runDir{id: id, size: size, newest: newest})
	}

	// Oldest first, by the newest file each run holds: a run's artifacts are
	// written at the end of it, so that timestamp is when the run finished.
	sort.Slice(all, func(i, j int) bool { return all[i].newest.Before(all[j].newest) })

	for _, r := range all {
		if total <= opts.BudgetBytes {
			break
		}
		if r.protected {
			continue
		}
		path := filepath.Join(dir, r.id)
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("artifact: removing %s: %w", path, err))
			continue
		}
		total -= r.size
		report.BytesFreed += r.size
		report.RunsEvicted = append(report.RunsEvicted, r.id)
	}

	report.TotalBytes = total
	if len(errs) > 0 {
		return report, artifactErr(errors.Join(errs...), "pruning artifacts")
	}
	return report, nil
}

// expire removes the files of one run whose declared retention has passed.
//
// The walk collects, and a second pass deletes through an [os.Root] scoped to
// the run's directory. Deleting from inside a WalkDir callback resolves the
// path a second time, which is a window in which a symlink can be swapped for
// one pointing elsewhere; os.Root refuses to traverse a symlink out of its
// root at all. Nothing should ever put a symlink in the artifact store — the
// collector writes regular files — but a retention sweep runs unattended,
// daily, as the user who can delete everything, and that is not a place to
// rely on "should".
func expire(dir, runID string, opts PruneOptions) (int64, []string, error) {
	var (
		freed   int64
		names   []string
		errs    []error
		expired []string // slash-separated, relative to dir
		sizes   []int64
	)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // a vanished entry is not a reason to stop
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil // same
		}
		rel = filepath.ToSlash(rel)

		expiresAt, ok := opts.ExpiresAt(runID, rel)
		if !ok || !opts.Now.After(expiresAt) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // same
		}
		expired = append(expired, rel)
		sizes = append(sizes, info.Size())
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	if len(expired) == 0 {
		return 0, nil, errors.Join(errs...)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		errs = append(errs, fmt.Errorf("artifact: opening %s: %w", dir, err))
		return 0, nil, errors.Join(errs...)
	}
	defer func() { _ = root.Close() }()

	for i, rel := range expired {
		if err := root.Remove(filepath.FromSlash(rel)); err != nil {
			errs = append(errs, fmt.Errorf("artifact: removing %s/%s: %w", runID, rel, err))
			continue
		}
		freed += sizes[i]
		names = append(names, runID+"/"+rel)
	}
	return freed, names, errors.Join(errs...)
}

// Usage reports the artifact footprint, for `latem doctor`.
func Usage(root string) int64 {
	size, _, _ := measure(filepath.Join(root, "artifacts"))
	return size
}

// measure returns a tree's apparent size, its newest modification time, and
// how many regular files it holds.
//
// The file count is not derivable from the size: a run whose artifacts all
// expired and a run holding one zero-byte file both measure zero bytes, and
// only the first should have its directory removed.
func measure(path string) (int64, time.Time, int) {
	var (
		total  int64
		newest time.Time
		files  int
	)
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // best effort; a vanished file is normal
		}
		info, err := d.Info()
		if err != nil {
			return nil // same
		}
		total += info.Size()
		files++
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return total, newest, files
}
