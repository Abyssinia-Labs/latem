//go:build darwin || (linux && (amd64 || arm64))

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Asking the terminal driver itself, for `latem approve` (A89). isTerminal
// asks the file system, which is enough to decide whether to prompt; this is
// what decides whether a person can be there at all.
//
// Through syscall rather than golang.org/x/term, for the reason useColor and
// readHidden give: two ioctls are not worth a dependency in a module whose
// dependency list is a reviewed part of the spec (§3).

// isTTY reports whether f is a terminal: whether the terminal driver answers
// for it with its settings, which is the question isatty(3) asks.
//
// Stricter than [isTerminal], which takes any character device but /dev/null.
// /dev/zero and /dev/random are character devices too, and nobody is behind
// either of them.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var errno syscall.Errno
	err = rc.Control(func(fd uintptr) {
		var t syscall.Termios
		//nolint:gosec // G103: the ioctl writes a termios into t and nothing else; the pointer is taken in the call, as unsafe's rule (3) asks.
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	})
	return err == nil && errno == 0
}

// discardTypeAhead throws away what was typed at f's terminal and not yet
// read, so that the line read next was typed after the question was on the
// screen. An answer that arrived before the question is not an answer to it.
func discardTypeAhead(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) { errno = flushInput(fd) }); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
