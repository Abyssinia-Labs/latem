package registry

import (
	"reflect"
	"strings"
	"testing"
)

// A53: a checkout reaches the run's commit, or a commit whose history a
// policy ref holds. Anything else — however it is named — is refused.
func TestAdmitCheckout(t *testing.T) {
	const pinned, other = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	cases := []struct {
		name       string
		sha        string
		containing []string
		allowed    []string
		wantVouch  string
		wantOK     bool
	}{
		{"the run's own commit needs no other reason", pinned, nil, []string{"main"}, "", true},
		{"main's tip", other, []string{"refs/heads/main"}, []string{"main"}, "refs/heads/main", true},
		{"a commit on main reached through a tag", other, []string{"refs/heads/main", "refs/tags/v1"}, []string{"main"}, "refs/heads/main", true},
		{"a commit only on a branch the policy does not name", other, []string{"refs/heads/feature/x"}, []string{"main"}, "", false},
		{"a tag the policy does not name, on no admitted branch", other, []string{"refs/tags/v1"}, []string{"main"}, "", false},
		{"a tag the policy names", other, []string{"refs/tags/v1"}, []string{"main", "refs/tags/v*"}, "refs/tags/v1", true},
		{"a pull request's merge ref, when admitted", other, []string{"refs/pull/7/merge"}, []string{"refs/pull/*/merge"}, "refs/pull/7/merge", true},
		{"nothing holds it", other, nil, []string{"main"}, "", false},
		{"a glob branch", other, []string{"refs/heads/release/1.2"}, []string{"release/*"}, "refs/heads/release/1.2", true},
		{"a glob does not cross a slash", other, []string{"refs/heads/release/1.2/hotfix"}, []string{"release/*"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vouch, ok := AdmitCheckout(c.sha, pinned, c.containing, c.allowed)
			if ok != c.wantOK || vouch != c.wantVouch {
				t.Fatalf("AdmitCheckout = %q, %v; want %q, %v", vouch, ok, c.wantVouch, c.wantOK)
			}
		})
	}
}

// The mirror is asked only about refs a policy could admit, and each prefix
// is one `git for-each-ref` matches: whole, or up to a slash.
func TestPolicyPrefixes(t *testing.T) {
	got := PolicyPrefixes([]string{"main", "release/*", "refs/tags/v*", "refs/pull/*/merge", "rel*", "main"})
	want := []string{"refs/heads/", "refs/heads/main", "refs/heads/release/", "refs/pull/", "refs/tags/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PolicyPrefixes = %q, want %q", got, want)
	}
}

func TestCheckoutStepValidate(t *testing.T) {
	cases := []struct {
		name string
		step CheckoutStep
		want string
	}{
		{"a branch", CheckoutStep{Ref: "main"}, ""},
		{"a commit", CheckoutStep{Ref: "0123456789abcdef0123456789abcdef01234567"}, ""},
		{"a hostile ref", CheckoutStep{Ref: "main;rm -rf /"}, "checkout: ref:"},
		{"a negative depth", CheckoutStep{FetchDepth: -1}, "negative"},
		{"sparse patterns", CheckoutStep{Sparse: []string{"packages/cli/package.json", "!*.md"}}, ""},
		{"a sparse pattern of two lines", CheckoutStep{Sparse: []string{"src\ndocs"}}, "not one line"},
		{"an empty sparse pattern", CheckoutStep{Sparse: []string{""}}, "not one line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			problems := c.step.validate()
			if c.want == "" {
				if len(problems) != 0 {
					t.Fatalf("validate = %v, want none", problems)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(problems[0].Error(), c.want) {
				t.Fatalf("validate = %v, want one containing %q", problems, c.want)
			}
		})
	}
}

// A step is one kind of thing.
func TestStepIsOneKind(t *testing.T) {
	s := Step{Name: "both", Run: "true", Checkout: &CheckoutStep{}}
	problems := s.validate(nil)
	if len(problems) == 0 || !strings.Contains(problems[0].Error(), "only one") {
		t.Fatalf("validate = %v", problems)
	}
}
