//go:build unix

package agent

import "syscall"

// sendReload delivers SIGHUP, which `latem agent` turns into [Agent.Reload].
func sendReload(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }
