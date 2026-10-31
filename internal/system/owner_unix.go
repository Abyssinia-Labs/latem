//go:build darwin || linux

package system

import (
	"io/fs"
	"syscall"
)

// fileOwner is the uid and gid a file belongs to.
func fileOwner(info fs.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}
