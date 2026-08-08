package docker

import (
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Error carries a stable RUNNER_SPEC §21 error code alongside a human
// explanation.
//
// The shape is deliberately the same as registry.Error: a control plane maps
// the code to copy and to a retry button, an operator reads the message, and
// neither has to infer one from the other. Executor failures land on three
// codes — E_EXECUTOR for "the container would not start or would not run",
// E_CACHE for a mount the run needs and cannot have, and E_ARTIFACT for a glob
// that tried to leave the workspace.
type Error struct {
	Code protocol.ErrorCode
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

// codeErr builds an [Error] with no wrapped cause.
func codeErr(code protocol.ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// wrapErr builds an [Error] carrying the underlying failure, so that
// errors.Is against a Docker SDK sentinel still works through it.
func wrapErr(code protocol.ErrorCode, err error, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...), Err: err}
}

// CodeOf extracts the error code from an error produced by this package,
// returning the empty code for anything else.
func CodeOf(err error) protocol.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
