package logstream

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Retention defaults from RUNNER_SPEC §16.
const (
	DefaultRetention = 14 * 24 * time.Hour
	DefaultGzipAfter = 24 * time.Hour
)

// ResultSuffix is what a run's result file adds to its id, beside its log
// (internal/result writes it; this package only sweeps it).
const ResultSuffix = ".result.json"

// PruneOptions controls the daily log sweep.
type PruneOptions struct {
	// Retention deletes logs older than this. Zero uses [DefaultRetention].
	Retention time.Duration
	// GzipAfter compresses logs older than this. Zero uses [DefaultGzipAfter].
	// Build logs are highly compressible — verbose Gradle output routinely
	// shrinks by an order of magnitude — so this is most of the disk saving.
	GzipAfter time.Duration
}

// PruneReport records what a sweep did. Callers should log it: a sweep that
// silently removes the log someone was about to read is indistinguishable from
// a bug, and RUNNER_SPEC's "no silent caps" principle applies to retention as
// much as anywhere.
type PruneReport struct {
	Compressed []string
	Deleted    []string
	BytesFreed int64
}

// Prune compresses and removes old logs.
//
// live reports whether a run is still going, so an in-flight build's log is
// never compressed out from under it or deleted mid-write. A nil live treats
// everything as finished.
func Prune(dir string, opts PruneOptions, now time.Time, live func(runID string) bool) (PruneReport, error) {
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.GzipAfter <= 0 {
		opts.GzipAfter = DefaultGzipAfter
	}
	if live == nil {
		live = func(string) bool { return false }
	}

	var report PruneReport

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("logstream: reading %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ResultSuffix) {
			// A run's result (A92) is kept as long as its log is and no
			// longer: it is the log, read once. It is small, and is never
			// compressed, since a caller reads it by name.
			runID := strings.TrimSuffix(name, ResultSuffix)
			info, err := entry.Info()
			if err != nil || live(runID) || now.Sub(info.ModTime()) <= opts.Retention {
				continue
			}
			path := filepath.Join(dir, name)
			if err := os.Remove(path); err != nil {
				return report, fmt.Errorf("logstream: removing %s: %w", path, err)
			}
			report.BytesFreed += info.Size()
			continue
		}
		if !strings.HasSuffix(name, ".log") && !strings.HasSuffix(name, ".log.gz") {
			continue
		}

		runID := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".log")
		if live(runID) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue // vanished between ReadDir and here; nothing to do
		}
		age := now.Sub(info.ModTime())
		path := filepath.Join(dir, name)

		if age > opts.Retention {
			size := info.Size()
			if err := os.Remove(path); err != nil {
				return report, fmt.Errorf("logstream: removing %s: %w", path, err)
			}
			report.Deleted = append(report.Deleted, name)
			report.BytesFreed += size
			continue
		}

		if age > opts.GzipAfter && strings.HasSuffix(name, ".log") {
			saved, err := compress(path)
			if err != nil {
				return report, err
			}
			report.Compressed = append(report.Compressed, name)
			report.BytesFreed += saved
		}
	}

	return report, nil
}

// compress gzips path in place, returning the bytes saved.
//
// It writes to a temporary file and renames, so an interrupted sweep leaves
// either the original or the complete compressed version — never a truncated
// log that reads as a build which stopped early.
func compress(path string) (int64, error) {
	// #nosec G304 -- path is filepath.Join(dir, name) where name came from
	// os.ReadDir of dir itself, so it names a file already in the log
	// directory. Nothing here originates outside the machine.
	src, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("logstream: opening %s: %w", path, err)
	}
	defer func() { _ = src.Close() }() // read-only; a close error changes nothing

	info, err := src.Stat()
	if err != nil {
		return 0, err
	}

	tmp := path + ".gz.tmp"
	// #nosec G304 -- derived from the same in-directory path as src above.
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("logstream: creating %s: %w", tmp, err)
	}

	zw := gzip.NewWriter(dst)
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		_ = dst.Close()
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logstream: compressing %s: %w", path, err)
	}
	if err := zw.Close(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}

	final := path + ".gz"
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logstream: renaming %s: %w", tmp, err)
	}
	// Preserve the modification time so the retention clock keeps running from
	// when the build happened, not from when the sweep touched it. Otherwise
	// compressing a log would silently extend its life by however long it sat
	// there uncompressed.
	_ = os.Chtimes(final, info.ModTime(), info.ModTime())

	if err := os.Remove(path); err != nil {
		return 0, fmt.Errorf("logstream: removing %s after compression: %w", path, err)
	}

	gzInfo, err := os.Stat(final)
	if err != nil {
		return 0, err
	}
	return info.Size() - gzInfo.Size(), nil
}
