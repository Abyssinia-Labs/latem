//go:build unix

package agent

import (
	"os"
	"syscall"
)

// sendReload delivers SIGHUP, which `latem agent` turns into [Agent.Reload].
func sendReload(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }

// sendSwitch delivers SIGUSR2, which `latem agent` turns into [Agent.Switch].
func sendSwitch(pid int) error { return syscall.Kill(pid, syscall.SIGUSR2) }

// SwitchSignal is the signal `latem agent` treats as [Agent.Switch].
func SwitchSignal() (os.Signal, bool) { return syscall.SIGUSR2, true }
