// Package artifact collects a run's declared outputs and reports them by
// reference.
//
// Two rules shape everything here, both from RUNNER_SPEC §17.
//
// Globs are relative to the workspace root and MUST NOT escape it. That makes
// this package a security boundary, not a file copier: the glob comes from a
// repository, the workspace contains whatever the build wrote, and a symlink
// is a perfectly ordinary thing for a build to produce. Resolution therefore
// ends in a containment check, and an escape is a hard failure rather than a
// skipped file — a build that quietly declines to collect the artifact
// somebody is exfiltrating through teaches nobody anything.
//
// Only references cross the boundary. A [protocol.ArtifactRef] carries a path,
// a size, and a digest; the .ipa itself is 50–300 MB and belongs on the machine
// that signed it (§5).
//
// # What the boundary does not stop
//
// A hard link inside the workspace is indistinguishable from a regular file,
// and one pointing at a file elsewhere on the same filesystem would be
// collected. Refusing files with a link count above one is not available as a
// defence: §12's workspaces are `git clone --local` copies whose entire object
// store is hard linked to the mirror, so that check would reject every real
// artifact. The mitigation is elsewhere — creating such a link requires read
// access to the target as the agent's own user, which is the same access the
// job already has.
package artifact

import (
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// DefaultBudgetBytes is RUNNER_SPEC §17's total artifact budget: 50 GB,
// evicted oldest-first. Mobile builds make this a real constraint rather than
// a theoretical one — .ipa and .aab files are 50–300 MB each, so a fortnight
// of nightly releases is most of the budget.
const DefaultBudgetBytes int64 = 50 << 30

// Error carries protocol.ErrArtifact alongside a human explanation. Every
// failure in this package is E_ARTIFACT: the glob escaped the workspace, or
// collection failed (§21).
type Error struct {
	Msg string
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", protocol.ErrArtifact, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", protocol.ErrArtifact, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Code reports the §21 code for an error from this package, or the empty code
// for anything else.
func Code(err error) protocol.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return protocol.ErrArtifact
	}
	return ""
}

func artifactErr(err error, format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Err: err}
}

// Declaration is one artifacts[] entry from the job registry: a glob relative
// to the workspace root, plus how long to keep what it matches.
//
// It mirrors registry.ArtifactDecl without importing it. The registry's type
// is a parsed YAML shape carrying protocol.Duration; this one is what the
// collector needs. Keeping them separate means neither package has to move
// when the other's file format changes.
type Declaration struct {
	Path string
	// Retain is how long the artifact is kept. Zero means "until the budget
	// evicts it" rather than "delete immediately" — a job that declares an
	// artifact and no retention wants the artifact.
	Retain time.Duration
}
