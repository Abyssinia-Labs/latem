//go:build darwin || linux

package artifact

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
