//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// ioctlGetTermios is tcgetattr(3)'s request on darwin.
const ioctlGetTermios = syscall.TIOCGETA

// fread is FREAD in <sys/fcntl.h>: TIOCFLUSH with it flushes the input queue
// and leaves the output alone. The syscall package does not carry it.
const fread = 0x1

// flushInput is tcflush(fd, TCIFLUSH).
func flushInput(fd uintptr) syscall.Errno {
	which := int32(fread)
	//nolint:gosec // G103: TIOCFLUSH reads one int from which; the pointer is taken in the call, as unsafe's rule (3) asks.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCFLUSH, uintptr(unsafe.Pointer(&which)))
	return errno
}
