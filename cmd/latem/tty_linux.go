//go:build linux && (amd64 || arm64)

package main

import "syscall"

// ioctlGetTermios is tcgetattr(3)'s request on Linux.
const ioctlGetTermios = syscall.TCGETS

// tcflsh is TCFLSH in <asm-generic/ioctls.h>, which amd64 and arm64, the two
// Linux targets a release is built for, both use. The syscall package has it
// for arm64 and not for amd64. Other architectures number their ioctls
// differently, which is why this file is built for these two alone.
const tcflsh = 0x540B

// flushInput is tcflush(fd, TCIFLUSH).
func flushInput(fd uintptr) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tcflsh, syscall.TCIFLUSH)
	return errno
}
