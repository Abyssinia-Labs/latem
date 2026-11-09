package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CheckoutStep is `uses: actions/checkout`, performed by the run loop rather
// than an executor (A53).
//
// It moves the job's working tree — never its definition — to a commit of
// the same repository, and makes the job's token the workspace's credential.
// The compiler is its only writer; the field holding it is excluded from
// decoding, like [ArtifactStep]'s.
type CheckoutStep struct {
	// Ref is a ref or commit known at admission; RefLate is one only known
	// as the step runs (`steps.*`). Both empty means the run's own commit.
	Ref     string
	RefLate LateValue
	// Token is `token:`, which must be an expression. Nil means the job's
	// own token.
	Token LateValue
	// PersistCredentials leaves the credential in place for the rest of the
	// job, as GitHub's checkout does by default.
	PersistCredentials bool
	// Clean removes untracked files before the switch.
	Clean bool
	// FetchDepth and FetchTags are accepted and recorded; the workspace
	// already holds the mirror's whole history and every tag (A53).
	FetchDepth int
	FetchTags  bool
	// Sparse is `sparse-checkout:`, one pattern each, and SparseCone its
	// cone mode. The step leaves only what they match in the working tree,
	// as `git sparse-checkout set` does; none restores the whole tree.
	Sparse     []string
	SparseCone bool
}

// commitID is a full commit id: SHA-1, or SHA-256 for a repository using it.
var commitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// IsCommitID reports whether s names a commit rather than a ref.
func IsCommitID(s string) bool { return commitID.MatchString(s) }

func (c *CheckoutStep) validate() []error {
	var problems []error
	add := func(format string, args ...any) { problems = append(problems, fmt.Errorf(format, args...)) }

	if c.Ref != "" && c.RefLate != nil {
		add("checkout: ref is either known at admission or late, not both")
	}
	if c.Ref != "" && !IsCommitID(c.Ref) {
		if err := ValidateRef(c.Ref); err != nil {
			add("checkout: ref: %v", err)
		}
	}
	if c.FetchDepth < 0 {
		add("checkout: fetch-depth %d is negative", c.FetchDepth)
	}
	for _, p := range c.Sparse {
		if p == "" || strings.ContainsAny(p, "\n\x00") {
			add("checkout: sparse-checkout pattern %q is not one line of a path", p)
		}
	}
	return problems
}

// AdmitCheckout decides whether a checkout may move a job's working tree to
// sha (A53, INV-5): the run's own pinned commit, or a commit reachable from
// the tip of a ref the repository's policy admits.
//
// containing is every ref whose history holds sha, as the freshly fetched
// mirror answers — asked only about [PolicyPrefixes], so it is short. Vouch is
// the policy ref that admitted it, for the log; it is empty for the pinned
// commit, which needs no other reason.
//
// Reachability rather than a name match is the decision. A deploy job's
// `needs.bump.outputs.sha` is a commit on main, and a rollback tag is a tag
// whose commit is on main; both are code the policy already trusts. A commit
// only on a branch the policy does not name is not, whatever it is called.
func AdmitCheckout(sha, pinned string, containing []string, allowedRefs []string) (vouch string, ok bool) {
	if sha == pinned {
		return "", true
	}
	expanded := ExpandPolicy(allowedRefs)
	refs := append([]string(nil), containing...)
	sort.Strings(refs)
	for _, r := range refs {
		if MatchPolicy(r, expanded) {
			return r, true
		}
	}
	return "", false
}

// PolicyPrefixes are where a policy's refs can live, for asking the mirror
// only about refs that could match: each expanded pattern up to its last
// slash before the first glob character, or whole when it has none. `git
// for-each-ref` matches a literal pattern completely or up to a slash, so a
// prefix that ended mid-name would match nothing.
func PolicyPrefixes(allowedRefs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ExpandPolicy(allowedRefs) {
		prefix := p
		if i := strings.IndexAny(p, "*?["); i >= 0 {
			prefix = p[:strings.LastIndex(p[:i], "/")+1]
		}
		if prefix == "" || seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	sort.Strings(out)
	return out
}
