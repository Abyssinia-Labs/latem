// Package logstream splits, redacts, sequences, and fans out run output.
//
// This file is owned by WS-00 and frozen. The pipeline implementation is WS-02.
package logstream

import "strings"

// Registrar accepts values that must never appear in output.
//
// It exists as a narrow interface so internal/secrets can register what it
// resolves without importing the redactor implementation, and so the ordering
// obligation in INV-4 — register before the first step starts — is expressible
// as a dependency rather than as a comment.
type Registrar interface {
	// Register adds a value to the redaction set.
	//
	// Implementations MUST expand the value per RUNNER_SPEC §16, which is
	// where finding F3 lives and is not optional:
	//
	//  1. the whole value, verbatim;
	//  2. if it contains a newline, EVERY non-empty line of it, individually;
	//  3. the value with all whitespace stripped;
	//  4. the base64 and URL-encoded forms of 1 and 3.
	//
	// Rule 2 is the one draft 01 of the spec omitted. Because the pipeline
	// splits into lines before redacting, registering only the whole value of
	// a multi-line secret redacts nothing at all — the whole value never
	// appears in any single line. APP_STORE_CONNECT_KEY_P8 is a PEM block, so
	// the omission meant an echoed App Store Connect key reached both the log
	// file and the control plane while INV-4's test passed.
	//
	// Values shorter than MinRedactableLength are ignored; matching them turns
	// every occurrence of "true" or "dev" into a redaction marker and makes
	// logs unreadable.
	Register(value []byte)
}

// MinRedactableLength is the shortest value worth redacting.
const MinRedactableLength = 6

// TooShort reports whether a value is below the length worth redacting.
//
// Callers resolving secrets should warn on this — by name, never by value —
// because the operator has a secret that will appear in logs verbatim and
// nothing else will tell them. Matching values this short turns every
// occurrence of "true" or "dev" into a marker and makes logs unreadable, which
// is why the floor exists rather than being zero.
//
// This lives beside the interface rather than beside the implementation
// because internal/secrets consumes it, and a consumer reaching past the seam
// into an implementation file is the coupling the seam exists to prevent
// (amendment A13).
func TooShort(value []byte) bool {
	return len(strings.TrimSpace(string(value))) < MinRedactableLength
}

// Redactor removes registered values and known credential patterns from a line.
type Redactor interface {
	Registrar

	// Redact returns line with every registered value and matched pattern
	// replaced by "***" plus a stable six-character hash of what was matched,
	// so an operator can correlate occurrences without learning the secret.
	//
	// This is called once per line at the point of emission, before frames are
	// sequenced and before fan-out. A sink therefore cannot be added that
	// bypasses it (INV-4).
	Redact(line string) string
}
