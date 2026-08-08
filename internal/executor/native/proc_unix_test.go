//go:build unix

package native

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// The guards in [signalGroup] are the ones that must never be wrong. A pgid of
// 0 means "my own process group", which for the agent is the agent; a pgid of 1
// is launchd. Either would be signalled by `kill(-pid)` if the pid were ever
// unset, and there is no recovering from having done it once.
func TestSignalGroup_RefusesToSignalItselfOrTheWorld(t *testing.T) {
	tests := map[string]*exec.Cmd{
		"nil command":   nil,
		"never started": {},
		"pid zero":      {Process: &os.Process{Pid: 0}},
		"pid one":       {Process: &os.Process{Pid: 1}},
	}
	for name, cmd := range tests {
		t.Run(name, func(t *testing.T) {
			if err := signalGroup(cmd, syscall.SIGKILL); !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("signalGroup = %v, want %v — it must refuse rather than guess", err, os.ErrProcessDone)
			}
		})
	}
}

// A group that has already gone is the goal, not a failure: a step that timed
// out and a Close arriving at the same moment both signal it.
func TestSignalGroup_AlreadyGoneIsSuccess(t *testing.T) {
	requireShell(t)
	cmd := exec.Command(DefaultShell, "-c", "exit 0")
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := signalGroup(cmd, syscall.SIGTERM); err != nil {
		t.Errorf("signalling a group that has already exited: %v", err)
	}
	if err := terminateGroup(cmd); err != nil {
		t.Errorf("terminateGroup on a finished command: %v", err)
	}
	if err := killGroup(cmd); err != nil {
		t.Errorf("killGroup on a finished command: %v", err)
	}
}

// A step whose process never produced a state has no exit code to report, and
// -1 is the only honest answer. It must not be mistaken for 0.
func TestStepExitCode_NoProcessState(t *testing.T) {
	if got := stepExitCode(nil); got != -1 {
		t.Errorf("stepExitCode(nil) = %d, want -1", got)
	}
}
