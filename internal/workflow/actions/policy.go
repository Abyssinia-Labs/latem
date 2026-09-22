package actions

import (
	"fmt"
	"sort"
	"strings"
)

// Policy is what an operator has decided about third-party actions on this
// machine. Its zero value refuses everything, which is the intended default:
// a marketplace action is code the operator did not write, running on a
// machine that holds their credentials, and opting into that is a decision
// rather than a setting to discover later.
type Policy struct {
	// Allowed is a list of `owner/repo` patterns. `*` matches within one path
	// segment, so `actions/*` admits every action that org publishes and
	// nothing else. An empty list admits nothing.
	Allowed []string
	// RequirePinned refuses a reference that is not a full commit id. It
	// ships on: a tag on somebody else's repository is a mutable reference,
	// and INV-5 yields reproducibility only for a commit.
	RequirePinned bool
	// AllowUnisolated permits a job with a remote action to run on the native
	// executor, where it executes as the agent's user with everything that
	// user can reach. Docker isolates; native does not, on any OS (A47). Off.
	AllowUnisolated bool
}

// DefaultPolicy is the refuse-everything zero value, named so a caller that
// means it says so.
func DefaultPolicy() Policy { return Policy{RequirePinned: true} }

// Permits reports why a reference may not be used, or nil.
//
// The two refusals are separate because the fixes are: one is a line in
// config.toml an operator adds deliberately, the other is an edit to the
// workflow. Collapsing them into "not allowed" would send people to the wrong
// file.
func (p Policy) Permits(r Ref) error {
	if !p.allows(r.Slug()) {
		return fmt.Errorf(
			"%s is not in [actions] allowed on this machine. A remote action is code you did not "+
				"write, running where your credentials are; add %q to allow it",
			r.Slug(), r.Slug())
	}
	if p.RequirePinned && !r.Pinned() {
		return fmt.Errorf(
			"%s names %q, which is a tag or branch and can be moved by whoever owns that "+
				"repository. Pin it to a full commit id, or clear require_pinned on this machine "+
				"to accept a moving reference",
			r.Slug(), r.Ref)
	}
	return nil
}

// allows matches a slug against the patterns.
func (p Policy) allows(slug string) bool {
	for _, pat := range p.Allowed {
		if matchSlug(strings.TrimSpace(pat), slug) {
			return true
		}
	}
	return false
}

// matchSlug matches `owner/repo` against a pattern whose `*` covers one
// segment.
//
// One segment, not any: `*/*` is a way to write "every action on the
// marketplace", and an operator who means that should have to write it rather
// than reach it by writing `*` and being surprised. A bare `*` matches
// nothing here, because a slug always has two segments.
func matchSlug(pattern, slug string) bool {
	pp, sp := strings.Split(pattern, "/"), strings.Split(slug, "/")
	if len(pp) != len(sp) {
		return false
	}
	for i := range pp {
		if pp[i] == "*" {
			continue
		}
		if !strings.EqualFold(pp[i], sp[i]) {
			return false
		}
	}
	return true
}

// Describe renders the policy for `latem doctor`, sorted so two machines with
// the same policy print the same thing.
func (p Policy) Describe() string {
	if len(p.Allowed) == 0 {
		return "remote actions: refused (nothing in [actions] allowed)"
	}
	pats := append([]string(nil), p.Allowed...)
	sort.Strings(pats)
	out := "remote actions: " + strings.Join(pats, ", ")
	if p.RequirePinned {
		out += "; pinned to a commit"
	} else {
		out += "; moving refs accepted"
	}
	if p.AllowUnisolated {
		out += "; permitted on native runners"
	}
	return out
}
