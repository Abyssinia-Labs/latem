//go:build unix

package native

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the step's shell into a process group of its own, so
// that the whole tree it spawns can be signalled with one call.
//
// This is not a refinement, it is the difference between a step timeout that
// works and one that does not. `xcodebuild` alone leaves behind a swift
// frontend, a `clang` or six, `XCBBuildService`, and a simulator device
// process; `gradle` leaves a daemon that is explicitly designed to outlive the
// invocation. Killing only the pid of the shell reparents every one of those to
// launchd, still holding DerivedData locks and still burning cores, and the
// next run inherits the mess. With Setpgid the child's pid *is* the new group
// id, so signalling -pid reaches everything the step started that has not
// deliberately left the group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateGroup asks the step's whole process group to stop.
//
// SIGTERM first because a build tool given the chance will flush its logs,
// remove its lock files and shut down its daemon; a build system killed
// outright leaves state the next run has to recover from.
func terminateGroup(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGTERM) }

// killGroup ends the step's whole process group unconditionally. It is what
// happens after [TerminateGrace] has passed and SIGTERM was not enough.
func killGroup(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }

// signalGroup sends sig to the negative pid, which is the kernel's spelling of
// "every process in that group".
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := cmd.Process.Pid
	// A pgid of 0 or 1 would mean "this process's own group" or "everything on
	// the machine". Neither is ever right here, and getting it wrong once is
	// unrecoverable, so it is checked rather than assumed.
	if pid <= 1 {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// The group is already gone. That is the goal, not a failure.
			return nil
		}
		return err
	}
	return nil
}

// stepExitCode reports the exit status a job author expects to see.
//
// os.ProcessState.ExitCode returns -1 for a process killed by a signal, which
// would report "the build was OOM-killed" and "the executor could not read the
// status" as the same thing. Shells have had a convention for this since v7
// UNIX — 128 plus the signal number — and since every step already goes through
// bash, using it here means a step killed by SIGKILL reports 137 whether the
// kill landed on the shell or on something the shell was waiting for.
func stepExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if status, ok := ps.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return ps.ExitCode()
}
