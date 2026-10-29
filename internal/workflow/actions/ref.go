// Package actions resolves and fetches the third-party actions a workflow
// names — A42 phase 5, the marketplace.
//
// This package does not run anything. It answers three questions and nothing
// else: what does `uses: owner/repo@ref` refer to, is this operator willing to
// run it, and where on disk is it. Execution is the compiler's and the
// executors' business, and keeping the fetch separate is what lets the
// dangerous half be reviewed on its own.
//
// Two properties shape everything here.
//
// A `uses:` string is repository content. It arrives from a workflow at the
// resolved commit, which puts it inside INV-5's boundary and nowhere near
// trusted: a contributor who can open a pull request can write one. So every
// part of it is validated to a shape before it reaches an argv, and the argv
// discipline is internal/source's — one function builds every git command, no
// shell is anywhere in the path.
//
// And a ref is not a commit. `actions/checkout@v5` names a tag on somebody
// else's repository, and a tag moves. INV-5 yields reproducibility only for a
// digest, so this package resolves to a SHA, keys its cache on the SHA, and
// records what it resolved — and [Policy.RequirePinned], which ships on,
// refuses anything that is not already one.
package actions

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultHost is where an unqualified `owner/repo` comes from.
const DefaultHost = "github.com"

const (
	maxUsesBytes = 512
	maxPathDepth = 16
)

var (
	// ownerPattern and repoPattern are GitHub's own limits, narrowed: no
	// leading dash, because an argv element that begins with one is an option
	// wherever a terminator is forgotten, and this package would rather the
	// name be unrepresentable than rely on remembering.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`)
	// refPattern covers a tag, a branch or a SHA as written in a workflow. It
	// is narrower than a git refname: `uses:` in the wild is a version tag, a
	// branch name or a hex SHA, and the characters git additionally allows
	// buy nothing here and cost a validation surface.
	refPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,199})$`)
	// shaPattern is a full commit id. Nothing shorter counts as pinned: an
	// abbreviated SHA is a prefix, and a prefix is a thing that can become
	// ambiguous later.
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
)

// Ref is a parsed remote `uses:`.
type Ref struct {
	Host  string
	Owner string
	Repo  string
	// Path is the subdirectory inside the repository holding action.yml, for
	// the `owner/repo/sub/dir@ref` form. Empty means the repository root.
	Path string
	// Ref is the tag, branch or SHA as the workflow wrote it.
	Ref string
}

// Slug is `owner/repo`, which is what an allowlist matches and what a human
// recognises. It deliberately carries neither the subdirectory nor the ref:
// permission to run an action is permission to run that repository.
func (r Ref) Slug() string { return r.Owner + "/" + r.Repo }

// String round-trips the reference as a workflow would write it.
func (r Ref) String() string {
	s := r.Slug()
	if r.Path != "" {
		s += "/" + r.Path
	}
	return s + "@" + r.Ref
}

// Pinned reports whether the reference names a commit rather than a moving
// name.
func (r Ref) Pinned() bool { return shaPattern.MatchString(r.Ref) }

// CloneURL is where the repository is fetched from.
func (r Ref) CloneURL() string {
	return "https://" + r.Host + "/" + r.Owner + "/" + r.Repo + ".git"
}

// IsRemote reports whether a `uses:` names another repository, as opposed to
// the local and docker forms phase 4 already handles.
func IsRemote(uses string) bool {
	return uses != "" && !strings.HasPrefix(uses, "./") && !strings.HasPrefix(uses, "../") &&
		!strings.HasPrefix(uses, "docker://")
}

// ParseRef parses `owner/repo@ref`, `owner/repo/path@ref`, and either with a
// leading host.
//
// Every refusal names the part that was wrong. An operator reading a build log
// should not have to guess which of five fields a workflow got wrong, and a
// contributor probing the validator should learn nothing from the answer that
// the rules in this file do not already say.
func ParseRef(uses string) (Ref, error) {
	if len(uses) > maxUsesBytes {
		return Ref{}, fmt.Errorf("uses: is %d bytes, limit %d", len(uses), maxUsesBytes)
	}
	if !IsRemote(uses) {
		return Ref{}, fmt.Errorf("uses: %q is not a remote action", uses)
	}

	body, ref, ok := strings.Cut(uses, "@")
	if !ok {
		return Ref{}, fmt.Errorf(
			"uses: %q names no version. A remote action is `owner/repo@ref`, and the ref is what "+
				"decides which code runs", uses)
	}
	if !refPattern.MatchString(ref) {
		return Ref{}, fmt.Errorf("uses: %q is not a usable tag, branch or commit", ref)
	}

	parts := strings.Split(body, "/")
	out := Ref{Host: DefaultHost}
	// A leading host is recognised by the dot no owner may contain.
	if len(parts) > 0 && strings.Contains(parts[0], ".") {
		out.Host = parts[0]
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return Ref{}, fmt.Errorf("uses: %q names no repository; expected owner/repo@ref", uses)
	}
	out.Owner, out.Repo, parts = parts[0], parts[1], parts[2:]

	if !ownerPattern.MatchString(out.Owner) {
		return Ref{}, fmt.Errorf("uses: %q is not a usable owner name", out.Owner)
	}
	if !repoPattern.MatchString(out.Repo) {
		return Ref{}, fmt.Errorf("uses: %q is not a usable repository name", out.Repo)
	}
	if !hostPattern.MatchString(out.Host) {
		return Ref{}, fmt.Errorf("uses: %q is not a usable host", out.Host)
	}

	if len(parts) > maxPathDepth {
		return Ref{}, fmt.Errorf("uses: the path in %q is %d segments deep, limit %d", uses, len(parts), maxPathDepth)
	}
	for _, seg := range parts {
		// "." and ".." are refused rather than cleaned. A path that has to be
		// cleaned to be safe is one where a later reader has to know it was
		// cleaned, and the whole value is somebody else's string.
		if seg == "" || seg == "." || seg == ".." || !segmentPattern.MatchString(seg) {
			return Ref{}, fmt.Errorf("uses: %q has a path segment that is not a plain directory name", uses)
		}
	}
	out.Path = strings.Join(parts, "/")
	out.Ref = ref
	return out, nil
}

var (
	hostPattern    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,252})[A-Za-z0-9]$`)
	segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)
