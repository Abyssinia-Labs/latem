//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a pseudo-terminal: what a person's terminal window is to the
// shell in it. The slave is the terminal a command reads and writes; the
// master is the person, typing and reading the screen.
//
// unlockpt and ptsname, as the two ioctls libc makes them of on Linux, where
// grantpt has nothing to do. Through SyscallConn rather than Fd, which would
// put the files in blocking mode, where a Close no longer ends a Read waiting
// on them.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening a pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })

	rc, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var (
		unlock int32
		n      uint32
		errno  syscall.Errno
	)
	if err := rc.Control(func(fd uintptr) {
		if _, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
			return
		}
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("setting up the pseudo-terminal: %v", errno)
	}
	name := fmt.Sprintf("/dev/pts/%d", n)
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}
