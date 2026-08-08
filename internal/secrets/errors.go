package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// MaxNameLength bounds a declared secret name. Names become environment
// variable keys and argv elements; a name longer than this is a mistake
// somewhere upstream, and catching it here is cheaper than catching it in
// `security`'s argument parser.
const MaxNameLength = 128

// namePattern is the shape of a secret name.
//
// internal/registry already applies this pattern when it parses a job's
// `secrets:` block, and this package applies it again on the way in. That is
// deliberate, and it is the same argument RUNNER_SPEC §5 makes about run ids:
// one validation point that everything downstream trusts is the shape of the
// next incident.
//
// Here the second check earns its keep concretely. A name reaches
// `security find-generic-password -s latem -a <NAME> -w` as an argv element,
// so a name beginning with `-` stops being data and becomes an option — and
// this package can be called by `latem doctor` with names from somewhere the
// registry never saw.
var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validName(name string) error {
	if name == "" {
		return errors.New("secret name is empty")
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("secret name is %d bytes, limit %d", len(name), MaxNameLength)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("secret name %q must match %s", name, namePattern)
	}
	return nil
}

// Error is a failure to produce the secrets a job declared.
//
// It carries names only. There is deliberately no field on this type capable
// of holding a value: this error becomes Update.ErrorMsg, and Update.ErrorMsg
// crosses the trust boundary (INV-4, RUNNER_SPEC §15). An error type that
// *could* carry a value is one careless refactor away from carrying one.
type Error struct {
	// Names are the declared secrets this failure concerns.
	Names []string
	// Msg explains what went wrong without quoting any value.
	Msg string
	// Err is the underlying cause, when there is one.
	Err error
}

func (e *Error) Error() string {
	// The prefix follows Code(), so a locked keychain reads E_SECRET_BACKEND
	// in the message as well as in the field a control plane switches on.
	code := e.Code()
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", code, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", code, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", code, e.Err)
	}
	return string(code)
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the RUNNER_SPEC §21 code for this failure.
//
// The distinction it draws is the one a retry button depends on. A secret
// nobody configured is E_MISSING_SECRET: rejected, non-retryable, and no
// amount of waiting fixes it. A secret store that could not be reached is
// E_SECRET_BACKEND: failed, retryable, because the most common instance — a
// Mac mini whose login keychain is locked after a reboot — is a machine
// condition that resolves without anyone editing a commit.
//
// This package originally returned E_MISSING_SECRET for both and said so in a
// comment, because §21 defined no code for a broken backend. Amendment A16
// added one, and collapsing the two would now tell an operator to go and fix
// a commit when the answer was to unlock a keychain.
//
// A program that is simply not installed stays E_MISSING_SECRET: `op` absent
// from PATH is a configuration fact, not a transient one.
func (e *Error) Code() protocol.ErrorCode {
	var ce *CommandError
	if errors.As(e.Err, &ce) && !ce.NotInstalled() {
		return protocol.ErrSecretBackend
	}
	if errors.Is(e.Err, fs.ErrPermission) {
		// A world-readable secrets.env, or a file this user cannot read: the
		// value exists, the store is unusable. Fixable in place.
		return protocol.ErrSecretBackend
	}
	return protocol.ErrMissingSecret
}

// CodeOf returns the §21 code for a secrets failure, or the empty code for
// anything else.
//
// A cancelled context deliberately falls into "anything else": a run torn down
// mid-resolution was interrupted, not misconfigured, and reporting
// E_MISSING_SECRET for it would send an operator looking for a secret that is
// perfectly fine.
func CodeOf(err error) protocol.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return ""
}
