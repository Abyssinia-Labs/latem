package main

import (
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Exit codes. These are contract: CI scripts, the install script, and a
// service manager's restart policy all branch on them, so a code's meaning is
// as much an API as a flag name.
//
// RUNNER_SPEC §7.3 fixes the interesting one. [ExitAgentDown] is not a run
// outcome — no run reached a terminal state — it means `latem run --wait` or
// `--follow` had nobody to wait for. A caller distinguishing "the build
// failed" from "the machine is not running an agent" needs those to be
// different numbers.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitFailure is any ordinary failure: a check failed, a run failed, a
	// file could not be written.
	ExitFailure = 1
	// ExitUsage is a malformed command line: an unknown flag, a missing
	// argument, an unknown subcommand. Usage is printed for these and for
	// nothing else, because a wall of usage text under a real error buries it.
	ExitUsage = 2
	// ExitAgentDown is protocol.ErrAgentDown: no agent is listening on the
	// local socket (§7.3, §21).
	ExitAgentDown = 3
)

// ExitError carries the exit code a failure should produce.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// usageErrorf builds an [ExitError] that exits [ExitUsage] and prints usage.
func usageErrorf(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

// errAgentDown is what a command returns when the local socket has nobody
// behind it.
func errAgentDown(socket string) error {
	return &ExitError{
		Code: ExitAgentDown,
		Err: fmt.Errorf("%s: no agent is listening on %s\n\nstart one with:\n  latem agent\nor install it as a service:\n  latem service install",
			protocol.ErrAgentDown, socket),
	}
}

// exitCode maps an error to the process exit status.
func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitFailure
}

// isUsage reports whether usage should be printed alongside the error.
func isUsage(err error) bool {
	return exitCode(err) == ExitUsage
}
