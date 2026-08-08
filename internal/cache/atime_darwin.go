package cache

import (
	"io/fs"
	"syscall"
	"time"
)

// accessTime reports when a file was last read, for RUNNER_SPEC §14.4's
// "evict by atime, oldest first".
//
// Access time is the right clock for a package store: entries are read on
// every build and written once, so mtime would evict a dependency that fifty
// builds have used since it was downloaded. It is also the less reliable
// clock — relatime updates it at most daily, noatime not at all — so a zero
// value falls back to mtime rather than reading as "1970, evict first".
// Daily granularity is fine here; the eviction order only needs to separate
// last week from this morning.
func accessTime(info fs.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	at := time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
	if at.IsZero() || at.Before(info.ModTime()) {
		return info.ModTime()
	}
	return at
}
