package source

import (
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Error carries a stable error code alongside a human explanation.
//
// The shape mirrors internal/registry deliberately. A caller driving the
// admission gates in RUNNER_SPEC §10 reads one code from every gate and maps
// it to a run outcome; having gate 7 hand back a code-bearing error and gate 9
// hand back a bare fmt.Errorf would push that mapping into the caller, which
// is where error taxonomies go to rot.
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

func codeErr(code protocol.ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

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
