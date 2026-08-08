package secrets

import (
	"errors"
	"fmt"
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
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", protocol.ErrMissingSecret, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", protocol.ErrMissingSecret, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", protocol.ErrMissingSecret, e.Err)
	}
	return string(protocol.ErrMissingSecret)
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the RUNNER_SPEC §21 code for this failure, which is always
// E_MISSING_SECRET.
//
// Every way this package can fail is, from the run's point of view, the same
// event: a declared secret did not arrive. Whether that was because no
// provider held it, because `secrets.env` was world-readable, or because `op`
// was not signed in is a distinction for the operator's terminal — `latem
// doctor` has the detail — and not for a control plane's retry button. §21
// defines no code for a broken secret backend, and inventing one here would
// produce a code the web app cannot render.
func (e *Error) Code() protocol.ErrorCode { return protocol.ErrMissingSecret }

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
