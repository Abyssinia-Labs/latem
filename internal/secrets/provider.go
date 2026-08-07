// Package secrets resolves named secrets from a provider chain.
//
// This file is owned by WS-00 and frozen. The chain implementation is WS-03.
package secrets

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound means a provider does not hold this name. It is not a failure —
// the chain tries the next provider. A name unresolved by every provider is
// protocol.ErrMissingSecret, raised at admission rather than forty minutes
// into a build (RUNNER_SPEC §15).
var ErrNotFound = errors.New("secrets: not found")

// Value is a secret's bytes.
//
// It is []byte and not string for one reason: RUNNER_SPEC §15 requires values
// be zeroed on run completion, and Go strings cannot be zeroed — they are
// immutable and live until the collector decides otherwise. Draft 01 of the
// spec promised zeroing "where the language allows", which was a guarantee the
// implementation could not have honoured (finding F10).
//
// Conversion to string happens exactly once, at the last moment, when building
// the executor's environment map. That map is dropped with the run.
type Value []byte

// Zero overwrites the value in place. Call it when the run reaches a terminal
// state.
func (v Value) Zero() {
	for i := range v {
		v[i] = 0
	}
}

// String returns a redaction marker, never the value.
//
// This is a deliberate safety net rather than a convenience. A stray %v, a
// log line written in haste, or an error wrapping a Value should not be able
// to leak a signing key. Callers that genuinely need the bytes convert
// explicitly, which is also the point at which a reviewer can see them doing
// it.
func (v Value) String() string { return "***" }

// GoString covers %#v as well.
func (v Value) GoString() string { return "secrets.Value(***)" }

// Provider resolves one secret name.
type Provider interface {
	// Resolve returns the value for name, or [ErrNotFound] if this provider
	// does not hold it. Only names declared in a job's secrets: block are ever
	// passed here.
	Resolve(ctx context.Context, name string) (Value, error)

	// Name identifies the provider in diagnostics: "env", "file", "keychain",
	// "pass", "command". `latem doctor --secrets` reports which provider
	// resolved which name — names only, never values.
	Name() string
}

// NotImplemented satisfies [Provider] by refusing every call.
type NotImplemented struct{}

var _ Provider = NotImplemented{}

func (NotImplemented) Resolve(context.Context, string) (Value, error) {
	return nil, fmt.Errorf("secrets: Resolve: %w", errors.ErrUnsupported)
}

func (NotImplemented) Name() string { return "not-implemented" }
