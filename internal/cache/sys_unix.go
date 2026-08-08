//go:build darwin || linux

package cache

import (
	"errors"
	"os"
	"syscall"
)

// tryFlock takes an exclusive, non-blocking flock on f.
//
// Non-blocking is what makes the ten-minute timeout in RUNNER_SPEC §14.3
// implementable: syscall.Flock without LOCK_NB blocks in the kernel, where no
// context cancellation and no deadline can reach it, so a contended cache
// would hang the run for as long as the other run takes. Polling costs a
// syscall every 50 ms and buys an upper bound.
//
// flock associates the lock with the open file description, not the process,
// so two os.OpenFile calls in one agent contend exactly as two agents would.
// That is what lets the concurrency test be a real test.
func tryFlock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	// EWOULDBLOCK is "someone else holds it", which is an answer, not a
	// failure. Anything else is a broken lock file and worth reporting.
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

func unflock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// freeBytes reports space available to an unprivileged user on the filesystem
// holding path.
//
// Bavail rather than Bfree: the difference is the reserved superuser blocks,
// which the agent cannot use, and reporting them as free is how a disk gate
// passes onto a machine that then fails mid-build.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize is uint32 on darwin and int64 on linux; the conversion makes one
	// expression compile on both.
	//
	// #nosec G115 -- a filesystem block size reported by statfs is a small
	// positive power of two. The signed-to-unsigned conversion only matters on
	// linux, where a negative Bsize would mean the kernel returned a nonsense
	// filesystem, and the disk gate would then read as "no space" rather than
	// as an overflow to a huge free figure.
	return st.Bavail * uint64(st.Bsize), nil
}
