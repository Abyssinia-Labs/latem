package cache

import (
	"io/fs"
	"syscall"
	"time"
)

// accessTime reports when a file was last read. See the darwin build of this
// file for why atime is the right clock and why it falls back to mtime.
func accessTime(info fs.FileInfo) time.Time {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.ModTime()
	}
	at := time.Unix(st.Atim.Sec, st.Atim.Nsec)
	if at.IsZero() || at.Before(info.ModTime()) {
		return info.ModTime()
	}
	return at
}
