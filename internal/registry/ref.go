package registry

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// refPattern is the INV-3 character class. It excludes every character git
// treats specially in an argument position, and every character a shell would
// treat specially if this value ever reached one — which it must not.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// Ref namespaces used during normalization.
const (
	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
	refsPrefix  = "refs/"
)

// ValidateRef applies INV-3 to a broker-supplied ref.
//
// This runs at gate 2, before any network or filesystem activity, so a hostile
// request costs microseconds. The value is never interpolated into a shell and
// never concatenated into a command string; git invocations pass argv as a
// slice with "--" before user-influenced arguments. This function exists as
// defence in depth on top of that, not instead of it.
//
// The leading-dash rule is the one carrying the most weight: "--upload-pack=/bin/sh"
// is a valid-looking ref string that turns `git fetch` into arbitrary code
// execution on any implementation that forgets the "--".
func ValidateRef(ref string) error {
	switch {
	case ref == "":
		return codeErr(protocol.ErrInvalidRef, "ref is empty")

	case len(ref) > 255:
		return codeErr(protocol.ErrInvalidRef, "ref is %d bytes, limit 255", len(ref))

	case !refPattern.MatchString(ref):
		return codeErr(protocol.ErrInvalidRef,
			"ref %q contains characters outside [A-Za-z0-9._/-]", ref)

	case strings.HasPrefix(ref, "-"):
		return codeErr(protocol.ErrInvalidRef,
			"ref %q begins with a dash and would be read as a git option", ref)

	case strings.Contains(ref, ".."):
		return codeErr(protocol.ErrInvalidRef,
			"ref %q contains \"..\"", ref)

	case strings.Contains(ref, "@{"):
		return codeErr(protocol.ErrInvalidRef,
			"ref %q uses reflog syntax, which is not reproducible", ref)
	}

	// The remaining rules are git's own (git check-ref-format). They are here
	// so an invalid ref fails at gate 2 with a clear message rather than at
	// gate 6 as an opaque git error forty seconds into a fetch.
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return codeErr(protocol.ErrInvalidRef, "ref %q has a leading or trailing slash", ref)
	}
	if strings.Contains(ref, "//") {
		return codeErr(protocol.ErrInvalidRef, "ref %q contains an empty path component", ref)
	}
	if strings.HasSuffix(ref, ".") {
		return codeErr(protocol.ErrInvalidRef, "ref %q ends with a dot", ref)
	}
	if strings.HasSuffix(ref, ".lock") {
		return codeErr(protocol.ErrInvalidRef, "ref %q ends with .lock", ref)
	}
	for _, seg := range strings.Split(ref, "/") {
		if strings.HasPrefix(seg, ".") {
			return codeErr(protocol.ErrInvalidRef,
				"ref %q has a component beginning with a dot", ref)
		}
		if strings.HasSuffix(seg, ".lock") {
			return codeErr(protocol.ErrInvalidRef,
				"ref %q has a component ending in .lock", ref)
		}
	}

	return nil
}

// RefLister reports whether a fully-qualified ref exists. Implemented by
// internal/source (WS-06) against the repo's mirror; an interface here so
// normalization stays pure and testable without a git repository.
type RefLister interface {
	HasRef(ctx context.Context, fullyQualified string) (bool, error)
}

// NormalizeRef resolves a supplied ref to exactly one fully-qualified name.
//
// Draft 01 of the spec said the policy glob was "matched after resolution",
// which is not implementable: resolution produces a SHA, and globbing
// release/* against a SHA matches nothing. The real order is normalize, match
// policy, then resolve (finding F5). This is the first of those three.
//
// Candidates, in order:
//
//   - a ref already beginning with "refs/" is taken as written;
//   - otherwise both refs/heads/<ref> and refs/tags/<ref> are considered.
//
// Exactly one must exist. Zero is E_INVALID_REF. More than one is
// E_AMBIGUOUS_REF and never a guess: when a repository has both a branch and a
// tag called "release", picking either silently is how you ship the wrong
// commit and cannot explain it afterwards.
func NormalizeRef(ctx context.Context, l RefLister, ref string) (string, error) {
	if err := ValidateRef(ref); err != nil {
		return "", err
	}

	if strings.HasPrefix(ref, refsPrefix) {
		ok, err := l.HasRef(ctx, ref)
		if err != nil {
			return "", &Error{Code: protocol.ErrSource, Msg: "listing refs", Err: err}
		}
		if !ok {
			return "", codeErr(protocol.ErrInvalidRef, "ref %q does not exist", ref)
		}
		return ref, nil
	}

	var found []string
	for _, candidate := range []string{headsPrefix + ref, tagsPrefix + ref} {
		ok, err := l.HasRef(ctx, candidate)
		if err != nil {
			return "", &Error{Code: protocol.ErrSource, Msg: "listing refs", Err: err}
		}
		if ok {
			found = append(found, candidate)
		}
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", codeErr(protocol.ErrInvalidRef,
			"ref %q matches neither %s%s nor %s%s", ref, headsPrefix, ref, tagsPrefix, ref)
	default:
		return "", codeErr(protocol.ErrAmbiguousRef,
			"ref %q is both a branch and a tag; qualify it as %s or %s",
			ref, found[0], found[1])
	}
}

// ExpandPolicy rewrites allowed_refs entries into fully-qualified patterns.
//
// An entry without a "refs/" prefix means a branch: "main" becomes
// refs/heads/main and does NOT match refs/tags/main. Allowing tags is written
// explicitly as "refs/tags/v*".
//
// This asymmetry is the point. Branch names are controlled by whoever can
// push; tags are frequently signed and mean something different. A policy
// reading "main" almost never intends "and also a tag someone named main".
func ExpandPolicy(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if strings.HasPrefix(p, refsPrefix) {
			out = append(out, p)
			continue
		}
		out = append(out, headsPrefix+p)
	}
	return out
}

// ValidatePolicy reports the first malformed glob in a policy list.
func ValidatePolicy(patterns []string) error {
	for _, p := range patterns {
		if p == "" {
			return fmt.Errorf("allowed_refs contains an empty pattern")
		}
		if !validGlob(p) {
			return fmt.Errorf("allowed_refs pattern %q is not a valid glob", p)
		}
	}
	return nil
}

// MatchPolicy reports whether a normalized ref is allowed to run without
// approval. Patterns must already have been through [ExpandPolicy].
func MatchPolicy(normalized string, expanded []string) bool {
	for _, p := range expanded {
		if matchPath(p, normalized) {
			return true
		}
	}
	return false
}

// CouldMatchPolicy is the conservative pre-check at gate 4 (RUNNER_SPEC §10).
//
// At that point the ref has not been normalized, because normalizing needs a
// fetched mirror and gates 1-5 run before any network activity. So expand the
// raw ref every way it could normalize and ask whether ANY of them could
// match. If none can, no fetch will change that answer and the run goes
// straight to awaiting_approval having cost nothing.
//
// The check is sound in the direction that matters: it never admits something
// the authoritative check at gate 8 would reject. It can be optimistic — a ref
// that could have been an allowed branch but turns out to be a tag proceeds to
// gate 8 and is caught there.
func CouldMatchPolicy(rawRef string, expanded []string) bool {
	candidates := []string{headsPrefix + rawRef, tagsPrefix + rawRef}
	if strings.HasPrefix(rawRef, refsPrefix) {
		candidates = []string{rawRef}
	}
	for _, c := range candidates {
		if MatchPolicy(c, expanded) {
			return true
		}
	}
	return false
}
