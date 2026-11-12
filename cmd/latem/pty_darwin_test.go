//go:build darwin

package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a pseudo-terminal: what a person's terminal window is to the
// shell in it. The slave is the terminal a command reads and writes; the
// master is the person, typing and reading the screen.
//
// grantpt, unlockpt and ptsname, as the three ioctls libc makes them of on
// darwin. Through SyscallConn rather than Fd, which would put the files in
// blocking mode, where a Close no longer ends a Read waiting on them.
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
		name  [128]byte
		errno syscall.Errno
	)
	if err := rc.Control(func(fd uintptr) {
		for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
			if _, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); errno != 0 {
				return
			}
		}
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("setting up the pseudo-terminal: %v", errno)
	}
	n := bytes.IndexByte(name[:], 0)
	if n <= 0 {
		t.Fatal("the pseudo-terminal has no name")
	}
	slave, err = os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", name[:n], err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}
