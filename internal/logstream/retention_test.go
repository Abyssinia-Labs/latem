package logstream

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, dir, name, body string, age time.Duration, now time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := now.Add(-age)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrune_CompressesAndDeletes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	body := strings.Repeat("> Task :app:compileKotlin\n", 500)

	writeLog(t, dir, "fresh.log", body, time.Hour, now)
	writeLog(t, dir, "stale.log", body, 48*time.Hour, now)
	writeLog(t, dir, "ancient.log", body, 30*24*time.Hour, now)

	report, err := Prune(dir, PruneOptions{}, now, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "fresh.log")); err != nil {
		t.Error("a one-hour-old log was touched")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.log.gz")); err != nil {
		t.Error("a two-day-old log was not compressed")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.log")); !os.IsNotExist(err) {
		t.Error("the original was left behind after compression")
	}
	if _, err := os.Stat(filepath.Join(dir, "ancient.log")); !os.IsNotExist(err) {
		t.Error("a 30-day-old log was not deleted")
	}

	if len(report.Compressed) != 1 || len(report.Deleted) != 1 {
		t.Errorf("report = %+v", report)
	}
	// Build logs are highly compressible; this is most of the disk saving.
	if report.BytesFreed <= 0 {
		t.Errorf("BytesFreed = %d", report.BytesFreed)
	}
}

func TestPrune_CompressedContentRoundTrips(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	body := "line one\nline two\nline three\n"
	writeLog(t, dir, "run_1.log", body, 48*time.Hour, now)

	if _, err := Prune(dir, PruneOptions{}, now, nil); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(filepath.Join(dir, "run_1.log.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("round trip lost content: %q", got)
	}
}

// Otherwise compressing a log would silently extend its life by however long
// it happened to sit there uncompressed.
func TestPrune_CompressionPreservesTheRetentionClock(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "run_1.log", "content here\n", 48*time.Hour, now)

	if _, err := Prune(dir, PruneOptions{}, now, nil); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "run_1.log.gz"))
	if err != nil {
		t.Fatal(err)
	}
	age := now.Sub(info.ModTime())
	if age < 47*time.Hour {
		t.Errorf("compression reset the mtime: the file now reads as %v old", age)
	}
}

// A compressed log still ages out.
func TestPrune_DeletesCompressedLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "old.log.gz", "compressed", 30*24*time.Hour, now)

	if _, err := Prune(dir, PruneOptions{}, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.log.gz")); !os.IsNotExist(err) {
		t.Error("an expired .gz was kept")
	}
}

// An in-flight build's log must never be compressed out from under it or
// deleted mid-write.
func TestPrune_SkipsLiveRuns(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "running.log", "in progress", 30*24*time.Hour, now)
	writeLog(t, dir, "finished.log", "done", 30*24*time.Hour, now)

	live := func(runID string) bool { return runID == "running" }
	report, err := Prune(dir, PruneOptions{}, now, live)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "running.log")); err != nil {
		t.Error("a live run's log was removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "finished.log")); !os.IsNotExist(err) {
		t.Error("a finished run's expired log was kept")
	}
	if len(report.Deleted) != 1 {
		t.Errorf("report = %+v", report)
	}
}

func TestPrune_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "notes.txt", "not a log", 90*24*time.Hour, now)
	writeLog(t, dir, "state.db", "not a log", 90*24*time.Hour, now)

	if _, err := Prune(dir, PruneOptions{}, now, nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes.txt", "state.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed", name)
		}
	}
}

func TestPrune_MissingDirectoryIsNotAnError(t *testing.T) {
	report, err := Prune(filepath.Join(t.TempDir(), "nope"), PruneOptions{}, time.Now(), nil)
	if err != nil {
		t.Fatalf("a missing log directory should be a no-op: %v", err)
	}
	if len(report.Deleted) != 0 || len(report.Compressed) != 0 {
		t.Errorf("report = %+v", report)
	}
}

func TestPrune_HonoursCustomWindows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLog(t, dir, "run_1.log", "x", 2*time.Hour, now)

	if _, err := Prune(dir, PruneOptions{Retention: time.Hour}, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run_1.log")); !os.IsNotExist(err) {
		t.Error("a custom retention window was ignored")
	}
}

// A run's result is swept with its log: kept as long, never compressed, and
// never removed under a run still going (A92).
func TestPrune_ResultsGoWithTheirLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	writeLog(t, dir, "fresh.result.json", `{"runId":"fresh"}`, time.Hour, now)
	writeLog(t, dir, "stale.result.json", `{"runId":"stale"}`, 48*time.Hour, now)
	writeLog(t, dir, "ancient.result.json", `{"runId":"ancient"}`, 30*24*time.Hour, now)
	writeLog(t, dir, "live.result.json", `{"runId":"live"}`, 30*24*time.Hour, now)

	report, err := Prune(dir, PruneOptions{}, now, func(id string) bool { return id == "live" })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fresh.result.json", "stale.result.json", "live.result.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed or renamed: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ancient.result.json")); !os.IsNotExist(err) {
		t.Error("a result older than the retention was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.result.json.gz")); !os.IsNotExist(err) {
		t.Error("a result was compressed: a caller reads it by name")
	}
	if len(report.Compressed) != 0 || report.BytesFreed <= 0 {
		t.Errorf("report = %+v", report)
	}
}
