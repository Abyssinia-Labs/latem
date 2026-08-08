package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The hostile-input table from RUNNER_SPEC §5. Every entry is something a
// compromised or buggy control plane could put in RunRequest.Ref.
//
// The agent never passes these to a shell and always uses argv slices with
// "--" before user-influenced arguments, so this validation is defence in
// depth rather than the only thing standing between a broker and the machine.
// It is still the cheapest place to stop them, and the one that produces a
// diagnosable error rather than a confusing git failure.
func TestValidateRef_HostileInputs(t *testing.T) {
	hostile := map[string]string{
		"git option injection":     "--upload-pack=/bin/sh",
		"short option":             "-x",
		"exec flag":                "--exec=/bin/sh",
		"shell command separator":  "; rm -rf /",
		"command substitution":     "$(id)",
		"backtick substitution":    "`id`",
		"variable expansion":       "${HOME}",
		"pipe":                     "main | cat /etc/passwd",
		"ampersand":                "main && id",
		"path traversal":           "../../etc/passwd",
		"traversal mid-ref":        "refs/heads/../../../etc/passwd",
		"embedded newline":         "refs/heads/x\nrefs/heads/y",
		"embedded carriage return": "refs/heads/x\rrefs/heads/y",
		"null byte":                "refs/heads/x\x00y",
		"reflog syntax":            "HEAD@{1}",
		"reflog on branch":         "main@{yesterday}",
		"leading slash":            "/refs/heads/main",
		"trailing slash":           "refs/heads/main/",
		"empty component":          "refs/heads//main",
		"trailing dot":             "refs/heads/main.",
		"dot component":            "refs/heads/.hidden",
		"lock suffix":              "refs/heads/main.lock",
		"space":                    "refs/heads/my branch",
		"tab":                      "refs/heads/my\tbranch",
		"glob star":                "refs/heads/*",
		"question mark":            "refs/heads/mai?",
		"tilde":                    "main~1",
		"caret":                    "main^",
		"colon":                    "main:refs/heads/evil",
		"cyrillic homoglyph":       "m\u0430in", // U+0430 renders as "a"
		"greek homoglyph":          "m\u03bfin", // U+03BF renders as "o"
		// Escaped rather than written literally: an invisible character in
		// source is exactly the Trojan Source problem this row is testing for,
		// and a reviewer should be able to see what the test contains.
		"zero width space":       "ma\u200bin",
		"right-to-left override": "ma\u202ein",
		"fullwidth latin":        "\uff4dain",
		"empty":                  "",
	}

	for name, ref := range hostile {
		t.Run(name, func(t *testing.T) {
			err := ValidateRef(ref)
			if err == nil {
				t.Fatalf("accepted hostile ref %q", ref)
			}
			if got := CodeOf(err); got != protocol.ErrInvalidRef {
				t.Errorf("code = %s, want %s", got, protocol.ErrInvalidRef)
			}
		})
	}
}

func TestValidateRef_LengthBound(t *testing.T) {
	// A 10 KB ref, from the spec's table.
	if err := ValidateRef(strings.Repeat("a", 10*1024)); err == nil {
		t.Error("accepted a 10 KB ref")
	}
	if err := ValidateRef(strings.Repeat("a", 256)); err == nil {
		t.Error("accepted a 256-character ref")
	}
	if err := ValidateRef(strings.Repeat("a", 255)); err != nil {
		t.Errorf("rejected a 255-character ref: %v", err)
	}
}

func TestValidateRef_AcceptsRealRefs(t *testing.T) {
	for _, ref := range []string{
		"main",
		"develop",
		"release/1.2",
		"release/1.2.3",
		"feature/JIRA-123-do-the-thing",
		"v1.0.0",
		"refs/heads/main",
		"refs/tags/v1.0.0",
		"refs/heads/release/2024.01",
		"a",
		"user.name/branch",
	} {
		if err := ValidateRef(ref); err != nil {
			t.Errorf("rejected legitimate ref %q: %v", ref, err)
		}
	}
}

type fakeLister map[string]bool

func (f fakeLister) HasRef(_ context.Context, name string) (bool, error) {
	return f[name], nil
}

// Normalization is the first third of finding F5: draft 01 said the policy
// glob was "matched after resolution", which cannot work because resolution
// produces a SHA.
func TestNormalizeRef(t *testing.T) {
	refs := fakeLister{
		"refs/heads/main":        true,
		"refs/heads/release/1.2": true,
		"refs/tags/v1.0.0":       true,
		"refs/heads/both":        true,
		"refs/tags/both":         true,
		"refs/pull/42/head":      true,
	}

	tests := map[string]struct {
		in   string
		want string
		code protocol.ErrorCode
	}{
		"short branch":            {"main", "refs/heads/main", ""},
		"nested branch":           {"release/1.2", "refs/heads/release/1.2", ""},
		"short tag":               {"v1.0.0", "refs/tags/v1.0.0", ""},
		"qualified branch":        {"refs/heads/main", "refs/heads/main", ""},
		"qualified tag":           {"refs/tags/v1.0.0", "refs/tags/v1.0.0", ""},
		"qualified pull ref":      {"refs/pull/42/head", "refs/pull/42/head", ""},
		"nonexistent":             {"nope", "", protocol.ErrInvalidRef},
		"nonexistent qualified":   {"refs/heads/nope", "", protocol.ErrInvalidRef},
		"branch and tag conflict": {"both", "", protocol.ErrAmbiguousRef},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeRef(context.Background(), refs, tc.in)
			if tc.code != "" {
				if err == nil {
					t.Fatalf("normalized %q to %q, want error %s", tc.in, got, tc.code)
				}
				if c := CodeOf(err); c != tc.code {
					t.Errorf("code = %s, want %s", c, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A branch and a tag of the same name is the case where guessing is worst: the
// two point at different commits and the operator has no way to tell which one
// built.
func TestNormalizeRef_AmbiguityIsNeverGuessed(t *testing.T) {
	refs := fakeLister{"refs/heads/release": true, "refs/tags/release": true}
	if _, err := NormalizeRef(context.Background(), refs, "release"); CodeOf(err) != protocol.ErrAmbiguousRef {
		t.Fatalf("want E_AMBIGUOUS_REF, got %v", err)
	}
	// Qualifying resolves it.
	for _, q := range []string{"refs/heads/release", "refs/tags/release"} {
		got, err := NormalizeRef(context.Background(), refs, q)
		if err != nil || got != q {
			t.Errorf("qualified %q: got %q, %v", q, got, err)
		}
	}
}

func TestExpandPolicy(t *testing.T) {
	got := ExpandPolicy([]string{"main", "release/*", "refs/tags/v*", "refs/heads/x"})
	want := []string{"refs/heads/main", "refs/heads/release/*", "refs/tags/v*", "refs/heads/x"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The second third of F5. A short-form allowlist must not be bypassable by
// spelling the same ref out in full, and must not silently cover tags.
func TestMatchPolicy(t *testing.T) {
	policy := ExpandPolicy([]string{"main", "develop", "release/*"})

	allowed := []string{
		"refs/heads/main",
		"refs/heads/develop",
		"refs/heads/release/1.2",
		"refs/heads/release/hotfix",
	}
	denied := []string{
		// A tag named main is a different object from the branch, and a policy
		// reading "main" almost never means "and also a tag someone named main".
		"refs/tags/main",
		"refs/heads/mainline",
		"refs/heads/feature/x",
		// * does not cross a separator.
		"refs/heads/release/1.2/hotfix",
		// Server-synthesised namespaces fail by construction, so an untrusted
		// pull request cannot execute without a deliberate config entry.
		"refs/pull/42/head",
		"refs/merge-requests/7/head",
	}

	for _, r := range allowed {
		if !MatchPolicy(r, policy) {
			t.Errorf("%q should be allowed", r)
		}
	}
	for _, r := range denied {
		if MatchPolicy(r, policy) {
			t.Errorf("%q should be denied", r)
		}
	}
}

// Writing out the fully-qualified form gives the same answer as the short one.
// If it did not, an allowlist would be one spelling away from useless.
func TestMatchPolicy_QualifiedSpellingIsEquivalent(t *testing.T) {
	policy := ExpandPolicy([]string{"main"})
	if !MatchPolicy("refs/heads/main", policy) {
		t.Error("refs/heads/main must match a policy of \"main\"")
	}

	qualified := ExpandPolicy([]string{"refs/heads/main"})
	if !MatchPolicy("refs/heads/main", qualified) {
		t.Error("an explicitly qualified policy must match too")
	}
}

func TestMatchPolicy_DoubleStarCrossesSegments(t *testing.T) {
	policy := ExpandPolicy([]string{"release/**"})
	for _, r := range []string{
		"refs/heads/release/1.2",
		"refs/heads/release/1.2/hotfix",
		"refs/heads/release/a/b/c",
	} {
		if !MatchPolicy(r, policy) {
			t.Errorf("%q should match release/**", r)
		}
	}
	if MatchPolicy("refs/heads/other/1.2", policy) {
		t.Error("release/** must not match another prefix")
	}
}

func TestMatchPolicy_TagsRequireExplicitEntry(t *testing.T) {
	policy := ExpandPolicy([]string{"refs/tags/v*"})
	if !MatchPolicy("refs/tags/v1.0.0", policy) {
		t.Error("explicit tag policy should match")
	}
	if MatchPolicy("refs/heads/v1.0.0", policy) {
		t.Error("a tag policy must not match a branch")
	}
}

// Gate 4's pre-check must never admit something gate 8 would reject. It may be
// optimistic in the other direction — that only costs a fetch.
func TestCouldMatchPolicy_IsSound(t *testing.T) {
	policy := ExpandPolicy([]string{"main", "release/*", "refs/tags/v*"})

	couldMatch := []string{"main", "release/1.2", "v1.0.0", "refs/tags/v2.0.0"}
	for _, r := range couldMatch {
		if !CouldMatchPolicy(r, policy) {
			t.Errorf("%q should survive the pre-check", r)
		}
	}

	// Nothing these could normalize to is allowed, so no fetch can change the
	// answer and they park immediately.
	cannotMatch := []string{"feature/x", "mainline", "refs/pull/42/head"}
	for _, r := range cannotMatch {
		if CouldMatchPolicy(r, policy) {
			t.Errorf("%q should fail the pre-check", r)
		}
	}
}

// The soundness property stated as a property: for every ref that a real
// normalization could produce, if gate 8 would allow it then gate 4 must have.
func TestCouldMatchPolicy_NeverRejectsWhatGate8Allows(t *testing.T) {
	policies := [][]string{
		{"main"},
		{"main", "release/*"},
		{"refs/tags/v*"},
		{"release/**"},
		{"main", "develop", "release/*", "refs/tags/v*"},
	}
	raw := []string{"main", "develop", "release/1.2", "v1.0.0", "feature/x", "mainline"}

	for _, p := range policies {
		expanded := ExpandPolicy(p)
		for _, r := range raw {
			for _, normalized := range []string{headsPrefix + r, tagsPrefix + r} {
				if MatchPolicy(normalized, expanded) && !CouldMatchPolicy(r, expanded) {
					t.Errorf("policy %v: gate 8 allows %q but gate 4 rejected %q",
						p, normalized, r)
				}
			}
		}
	}
}

func TestValidatePolicy(t *testing.T) {
	if err := ValidatePolicy([]string{"main", "release/*", "refs/tags/v*", "**"}); err != nil {
		t.Errorf("rejected a valid policy: %v", err)
	}
	for _, bad := range [][]string{{""}, {"release/[unclosed"}} {
		if err := ValidatePolicy(bad); err == nil {
			t.Errorf("accepted invalid policy %v", bad)
		}
	}
}
