package cache

import (
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Error carries a stable RUNNER_SPEC §21 code alongside a human explanation.
//
// The code is what a control plane maps to copy and to a retry button; the
// message is what an operator reads. Keeping both on one type means a caller
// never has to infer one from the other, which is how error taxonomies rot.
//
// This package returns exactly two codes. E_CACHE is "this run cannot have the
// cache it asked for"; E_DISK is "this machine cannot hold the build at all".
// They are different operational problems with different fixes, and collapsing
// them would tell an operator to clear caches when the answer is to buy a disk.
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

// ErrBelowFloor means free disk is under a runner's min_free_gb. It maps to
// protocol.ErrDisk, and refusing the run here is the whole point: failing at
// admission costs seconds, failing at 80% of an iOS build costs an hour
// (RUNNER_SPEC §14.4).
var ErrBelowFloor = errors.New("cache: free disk is below the runner floor")

func cacheErr(err error, format string, args ...any) *Error {
	return &Error{Code: protocol.ErrCache, Msg: fmt.Sprintf(format, args...), Err: err}
}

func diskErr(format string, args ...any) *Error {
	return &Error{Code: protocol.ErrDisk, Msg: fmt.Sprintf(format, args...), Err: ErrBelowFloor}
}

// CodeOf reports which §21 code an error from this package carries.
//
// A caller that has to switch on the error type to decide between E_CACHE and
// E_DISK will eventually get it wrong; this is the one place that mapping
// lives. Anything not produced here reports the empty code.
func CodeOf(err error) protocol.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	// ErrLockTimeout is declared on the frozen interface without a code
	// attached, and callers may compare against it directly, so map it here as
	// well rather than depending on it always arriving wrapped.
	if errors.Is(err, ErrLockTimeout) {
		return protocol.ErrCache
	}
	return ""
}
