//go:build unix

package native

import (
	"errors"
	"fmt"
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

// signalGroup sends sig to the group led by a step this process started.
//
// A command with no process is reported as already done rather than refused,
// because that is what it is: [stepProc.stop] and [runEnv.Close] both call this
// on commands that may have finished, and neither should have to tell "never
// started" apart from "already exited".
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := cmd.Process.Pid
	// A pgid of 0 or 1 would mean "this process's own group" or "everything on
	// the machine". Neither is ever right here, and getting it wrong once is
	// unrecoverable, so it is checked rather than assumed. [signalPgid] checks
	// it again; see [readGroupRecord] for why the duplication is deliberate.
	if pid <= 1 {
		return os.ErrProcessDone
	}
	return signalPgid(pid, sig)
}

// signalPgid sends sig to the negative pgid, which is the kernel's spelling of
// "every process in that group".
//
// This is the one call in the package that can end processes it was never
// handed, so the two numbers that would be catastrophic are refused before it is
// reached. 0 means the caller's own process group: for the agent, that is the
// agent, and on a runner installed as a LaunchAgent it may be every latem
// process on the machine. 1 is launchd's group. Anything ≤ 1 is therefore not a
// mistake to recover from but one to refuse, loudly, with the number in the
// message.
//
// The agent's own group is refused explicitly as well, because the number that
// reaches here during a reap comes off a disk rather than from a live
// [exec.Cmd]: a stale record naming a pgid the kernel has since handed back to
// this very agent would otherwise have the reconciliation pass at startup kill
// the agent that was running it.
func signalPgid(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("%w: %d means this process's own group or every process on the machine",
			errRefusedPgid, pgid)
	}
	if own := syscall.Getpgrp(); pgid == own {
		return fmt.Errorf("%w: %d is the agent's own process group", errRefusedPgid, pgid)
	}
	// Every number that reaches this line has been through the two guards above,
	// and there are exactly two ways to get here: a step's own [exec.Cmd], and a
	// record read from this executor's own EnvFileDir whose group an inherited
	// flock has just proved to be the one that was recorded.
	if err := syscall.Kill(-pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// The group is already gone. That is the goal, not a failure.
			return nil
		}
		return err
	}
	return nil
}

// terminatePgid and killPgid are [terminateGroup] and [killGroup] for a group
// that no [exec.Cmd] refers to any more — an orphan named by a record left
// behind when the agent died. See [stopGroup].
func terminatePgid(pgid int) error { return signalPgid(pgid, syscall.SIGTERM) }

func killPgid(pgid int) error { return signalPgid(pgid, syscall.SIGKILL) }

// pgidGone reports whether the group is empty. Signal 0 delivers nothing and
// only asks the question; ESRCH is the kernel saying there is no such group.
// Any other answer — EPERM, which a group holding an unreaped zombie can
// produce — is read as "still there", so the caller waits rather than declaring
// victory it cannot see.
func pgidGone(pgid int) bool {
	if pgid <= 1 {
		return true
	}
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// processGroupOf reports the group [setProcessGroup] gave a started command. It
// is the child's own pid: Setpgid with no Pgid makes the child a group leader,
// and a leader's pid is its group id.
func processGroupOf(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// attachGroupRecord locks a step's process group record and puts it in the
// child's file descriptor table, before the child exists.
//
// The flock is what makes the record trustworthy after a crash, and it works
// because of one specific property: a BSD flock belongs to the open file
// description, not to the process that took it. The descriptor is duplicated
// into the child by [exec.Cmd.ExtraFiles] and inherited by everything the child
// forks, so the lock is held by the step's whole tree and outlives both the
// agent that took it and the shell that led the group. [stepGroup] documents
// what a later reaper concludes from being able, or unable, to take it.
//
// LOCK_NB because there is nothing to wait for: the file was created by
// [os.CreateTemp] a moment ago and nobody else can have it. A blocking flock
// here would hang the step start on a filesystem oddity instead of failing it.
func attachGroupRecord(cmd *exec.Cmd, f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	return nil
}

// groupRecordHeld reports whether any process that inherited this record's
// descriptor is still alive — which is [stepGroup]'s answer to pid reuse.
//
// Taking the lock proves the step's tree is gone, so the lock is dropped again
// immediately and the record is treated as stale. Failing to take it proves a
// holder survives, and a recycled pid cannot produce that: the lock is on this
// inode, created by this agent for this step.
func groupRecordHeld(path string) (bool, error) {
	// #nosec G304 -- see readGroupRecord: the path is built entirely from this
	// executor's own EnvFileDir and a directory entry inside it.
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	fd := int(f.Fd())
	switch err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		// Closing the file would release it anyway; unlocking says so.
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	default:
		return false, err
	}
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
