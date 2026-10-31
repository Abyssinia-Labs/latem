//go:build !darwin && !linux

package system

import "io/fs"

// fileOwner has no answer off the two supported platforms; see diskUsage.
func fileOwner(fs.FileInfo) (uid, gid uint32, ok bool) { return 0, 0, false }
