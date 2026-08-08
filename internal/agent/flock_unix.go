//go:build unix

package agent

import (
	"errors"
	"os"
	"syscall"
)

// errWouldBlock is the platform's "somebody else holds it".
var errWouldBlock = syscall.EWOULDBLOCK

// lockExclusive takes a non-blocking exclusive flock.
//
// Non-blocking is the whole point. A second `latem agent` must fail
// immediately with a message naming the holder, not queue behind it and start
// hours later when the first one is stopped — by which time nobody is watching
// the terminal it was launched from.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EAGAIN) {
		// EAGAIN and EWOULDBLOCK are the same value on Linux and Darwin, but
		// that is a platform fact rather than a language one, so both are
		// mapped rather than assumed equal.
		return errWouldBlock
	}
	return err
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
