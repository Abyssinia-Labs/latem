package registry

import (
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Error carries a stable error code alongside a human explanation.
//
// The code is what a control plane maps to copy and to a retry button
// (RUNNER_SPEC §21); the message is what an operator reads. Keeping both on
// one type means a caller never has to infer one from the other, which is how
// error taxonomies rot.
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

// CodeOf extracts the error code from an error produced by this package,
// returning the empty code for anything else.
func CodeOf(err error) protocol.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Message returns the human-readable part of an error, without the code prefix
// that [Error.Error] adds.
//
// It exists so a [Decision] can carry the code in its own field and the
// explanation in another, rather than rendering "E_INVALID_REF: E_INVALID_REF:
// ref is empty" when the two are combined.
func Message(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	switch {
	case e.Msg != "" && e.Err != nil:
		return e.Msg + ": " + e.Err.Error()
	case e.Msg != "":
		return e.Msg
	case e.Err != nil:
		return e.Err.Error()
	}
	return string(e.Code)
}
