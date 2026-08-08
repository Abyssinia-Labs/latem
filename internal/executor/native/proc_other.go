//go:build !unix

package native

import (
	"fmt"
	"os"
	"os/exec"
)

// The native executor targets macOS (§13.3) and works on any unix. This file
// exists so the package still compiles elsewhere — `go build ./...` on a
// developer's Windows machine should not fail on a package that platform will
// never run — and it is deliberately worse than the unix version rather than
// pretending to be equivalent: there is no process group, so a step timeout
// reaches the shell and not the tree beneath it.

func setProcessGroup(*exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}

func killGroup(cmd *exec.Cmd) error { return terminateGroup(cmd) }

func stepExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	return ps.ExitCode()
}

// The process group record ([stepGroup]) degrades the same way, and for the same
// reason: there is no group to record, and [exec.Cmd.ExtraFiles] is not
// supported on Windows, so nothing is attached and no descriptor is inherited.
// The record file is still created and still removed, which keeps one code path
// in [runEnv.Exec] rather than two; [processGroupOf] reports 0, so
// [stepGroup.note] writes nothing and a reaper finds an empty record and signals
// nothing. A crashed agent leaks the step's processes here, which is what "no
// process group" already meant for step timeouts above.

func processGroupOf(*exec.Cmd) int { return 0 }

func attachGroupRecord(*exec.Cmd, *os.File) error { return nil }

func groupRecordHeld(string) (bool, error) { return false, nil }

func terminatePgid(pgid int) error {
	return fmt.Errorf("%w: %d cannot be signalled as a group on this platform", errRefusedPgid, pgid)
}

func killPgid(pgid int) error { return terminatePgid(pgid) }

func pgidGone(int) bool { return true }
