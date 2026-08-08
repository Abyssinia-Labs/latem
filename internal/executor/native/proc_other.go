//go:build !unix

package native

import (
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
