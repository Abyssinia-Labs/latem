//go:build darwin || linux

package source

import (
	"errors"
	"os"
	"syscall"
)

// tryFlock takes an exclusive, non-blocking flock on f, and reports whether
// it got it. A warm workspace's lock (A90) is the one use: it is what keeps
// `latem cache prune`, a second process, from removing a workspace a run in
// the agent is building in.
func tryFlock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}
