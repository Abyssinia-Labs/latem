package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stored writes a file into the artifact store as the collector would, with a
// modification time, so the eviction order can be exercised without waiting.
func stored(t *testing.T, root, runID, rel string, size int, age time.Duration, now time.Time) {
	t.Helper()
	path := filepath.Join(root, "artifacts", runID, filepath.FromSlash(rel))
	write(t, path, strings.Repeat("x", size))
	mtime := now.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestPrune_DeletesExpiredFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	stored(t, root, "run_1", "build/app.ipa", 100, time.Hour, now)
	stored(t, root, "run_1", "build/keep.aab", 100, time.Hour, now)

	report, err := Prune(root, PruneOptions{
		Now: now,
		ExpiresAt: func(runID, rel string) (time.Time, bool) {
			if runID == "run_1" && rel == "build/app.ipa" {
				return now.Add(-time.Minute), true // expired a minute ago
			}
			return time.Time{}, false
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, "artifacts", "run_1", "build", "app.ipa")); !os.IsNotExist(err) {
		t.Error("an expired artifact was kept")
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "run_1", "build", "keep.aab")); err != nil {
		t.Error("an artifact with no declared expiry was deleted")
	}
	if len(report.FilesExpired) != 1 || report.FilesExpired[0] != "run_1/build/app.ipa" {
		t.Errorf("FilesExpired = %v", report.FilesExpired)
	}
	if report.BytesFreed != 100 {
		t.Errorf("BytesFreed = %d, want 100", report.BytesFreed)
	}
	if report.TotalBytes != 100 {
		t.Errorf("TotalBytes = %d, want the remaining artifact", report.TotalBytes)
	}
}

// A run whose artifacts have all expired should not linger as an empty
// directory: `latem doctor` would report runs whose artifacts are gone.
func TestPrune_RemovesEmptiedRunDirectories(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stored(t, root, "run_1", "build/app.ipa", 10, time.Hour, now)

	if _, err := Prune(root, PruneOptions{
		Now:       now,
		ExpiresAt: func(string, string) (time.Time, bool) { return now.Add(-time.Hour), true },
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "run_1")); !os.IsNotExist(err) {
		t.Error("an emptied run directory was left behind")
	}
}

// §17's total budget, evicted oldest-first. .ipa and .aab files are 50–300 MB
// each, so this is what stops a fortnight of nightly releases filling a disk.
func TestPrune_EvictsOldestRunsToStayInBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	stored(t, root, "oldest", "app.ipa", 400, 72*time.Hour, now)
	stored(t, root, "middle", "app.ipa", 400, 48*time.Hour, now)
	stored(t, root, "newest", "app.ipa", 400, time.Hour, now)

	report, err := Prune(root, PruneOptions{Now: now, BudgetBytes: 900})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.RunsEvicted) != 1 || report.RunsEvicted[0] != "oldest" {
		t.Fatalf("RunsEvicted = %v, want [oldest]", report.RunsEvicted)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "newest")); err != nil {
		t.Error("the newest run was evicted")
	}
	if report.TotalBytes != 800 {
		t.Errorf("TotalBytes = %d, want 800", report.TotalBytes)
	}
	if report.BytesFreed != 400 {
		t.Errorf("BytesFreed = %d, want 400", report.BytesFreed)
	}
}

// Artifacts are written during `collecting`. A sweep landing at that moment
// must not delete the files the run is still copying.
func TestPrune_NeverEvictsALiveRun(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stored(t, root, "running", "app.ipa", 400, 72*time.Hour, now)
	stored(t, root, "finished", "app.ipa", 400, time.Hour, now)

	report, err := Prune(root, PruneOptions{
		Now:         now,
		BudgetBytes: 100,
		Live:        func(runID string) bool { return runID == "running" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "running", "app.ipa")); err != nil {
		t.Error("a live run's artifacts were evicted")
	}
	if len(report.RunsEvicted) != 1 || report.RunsEvicted[0] != "finished" {
		t.Errorf("RunsEvicted = %v", report.RunsEvicted)
	}
}

// A live run still counts against the budget. Reporting a footprint that
// excludes it would let the store overshoot by however much is in flight.
func TestPrune_CountsLiveRunsAgainstTheBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stored(t, root, "running", "app.ipa", 500, time.Hour, now)

	report, err := Prune(root, PruneOptions{
		Now:         now,
		BudgetBytes: 100,
		Live:        func(string) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalBytes != 500 {
		t.Errorf("TotalBytes = %d, want 500", report.TotalBytes)
	}
}

func TestPrune_MissingStoreIsNotAnError(t *testing.T) {
	report, err := Prune(t.TempDir(), PruneOptions{})
	if err != nil {
		t.Fatalf("an agent that has never collected an artifact is not a failure: %v", err)
	}
	if report.TotalBytes != 0 {
		t.Errorf("report = %+v", report)
	}
}

func TestPrune_DefaultBudgetKeepsEverythingSmall(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stored(t, root, "run_1", "app.ipa", 1024, 400*24*time.Hour, now)

	report, err := Prune(root, PruneOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RunsEvicted) != 0 {
		t.Errorf("a kilobyte was evicted under a 50 GB budget: %v", report.RunsEvicted)
	}
}

func TestUsage(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stored(t, root, "run_1", "a.ipa", 300, time.Hour, now)
	stored(t, root, "run_2", "b.aab", 200, time.Hour, now)

	if got := Usage(root); got != 500 {
		t.Errorf("Usage = %d, want 500", got)
	}
}
