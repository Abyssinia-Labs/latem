//go:build darwin || linux

package system

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// diskUsage sums the blocks actually allocated under dir.
//
// Allocated blocks, not apparent sizes, and the distinction is the whole point
// of the function. A Tart VM's disk image is a sparse file whose apparent size
// is the guest's full disk — 90 GB or more — while a freshly cloned image
// shares nearly every block with its parent through APFS copy-on-write and
// occupies almost nothing. Comparing an apparent size against `min_free_gb`
// (RUNNER_SPEC §13.2, §14.4) would report a machine as unable to hold an image
// it is already holding.
//
// Hardlinks are counted once. A directory tree can contain the same inode
// several times, and counting it twice inflates the answer in exactly the case
// — a cloned image — where the answer matters.
func diskUsage(dir string) (int64, error) {
	seen := make(map[uint64]struct{})
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is normal on a live machine and
			// must not fail a diagnostic. Anything else propagates.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		if st.Nlink > 1 {
			if _, dup := seen[st.Ino]; dup {
				return nil
			}
			seen[st.Ino] = struct{}{}
		}
		// st_blocks is in 512-byte units by POSIX definition, regardless of
		// the filesystem's own block size.
		total += st.Blocks * 512
		return nil
	})
	return total, err
}
