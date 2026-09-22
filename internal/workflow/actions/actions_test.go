package actions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pin = "0123456789abcdef0123456789abcdef01234567"

func TestParseRef(t *testing.T) {
	cases := map[string]Ref{
		"actions/checkout@v5":     {Host: DefaultHost, Owner: "actions", Repo: "checkout", Ref: "v5"},
		"actions/checkout@" + pin: {Host: DefaultHost, Owner: "actions", Repo: "checkout", Ref: pin},
		"owner/repo/sub/dir@main": {Host: DefaultHost, Owner: "owner", Repo: "repo", Path: "sub/dir", Ref: "main"},
		"git.example.com/o/r@v1":  {Host: "git.example.com", Owner: "o", Repo: "r", Ref: "v1"},
		"o/r@release/2026":        {Host: DefaultHost, Owner: "o", Repo: "r", Ref: "release/2026"},
	}
	for uses, want := range cases {
		got, err := ParseRef(uses)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", uses, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", uses, got, want)
		}
	}
}

// A `uses:` is repository content: a contributor who can open a pull request
// can write one. Every one of these would otherwise reach an argv or a path.
func TestParseRefRefusesHostileInput(t *testing.T) {
	for _, uses := range []string{
		"actions/checkout",                    // no version at all
		"-flag/repo@v1",                       // an owner that is an option
		"actions/-repo@v1",                    // a repo that is an option
		"actions/checkout@-flag",              // a ref that is an option
		"../../etc/passwd@v1",                 // traversal in the slug
		"owner/repo/../../../etc@v1",          // traversal in the path
		"owner/repo/./x@v1",                   // a segment that needs cleaning
		"owner/repo@v1;rm -rf /",              // shell punctuation in the ref
		"owner/repo@v1 --upload-pack=/bin/sh", // the INV-3 classic
		"owner@v1",                            // no repository
		"owner/repo@",                         // empty ref
		"o/r@" + strings.Repeat("v", 300),     // an unbounded ref
		strings.Repeat("a", 600) + "/r@v1",    // an unbounded uses
		"./local/action",                      // not remote
		"docker://alpine:3.19",                // not remote
	} {
		if got, err := ParseRef(uses); err == nil {
			t.Errorf("ParseRef(%q) accepted it as %+v", uses, got)
		}
	}
}

func TestPinned(t *testing.T) {
	sha, _ := ParseRef("o/r@" + pin)
	if !sha.Pinned() {
		t.Error("a full commit id is pinned")
	}
	// An abbreviation is a prefix, and a prefix can become ambiguous later.
	short, _ := ParseRef("o/r@0123456")
	if short.Pinned() {
		t.Error("an abbreviated sha must not count as pinned")
	}
	tag, _ := ParseRef("o/r@v5")
	if tag.Pinned() {
		t.Error("a tag is a moving reference")
	}
}

// The zero value refuses everything: opting into third-party code on a machine
// holding your credentials is a decision, not a default.
func TestPolicyDefaultRefusesEverything(t *testing.T) {
	r, _ := ParseRef("actions/checkout@" + pin)
	if err := (Policy{}).Permits(r); err == nil {
		t.Fatal("the zero policy admitted an action")
	}
	if err := DefaultPolicy().Permits(r); err == nil {
		t.Fatal("DefaultPolicy admitted an action")
	}
}

func TestPolicyAllowlist(t *testing.T) {
	p := Policy{Allowed: []string{"actions/*", "docker/build-push-action"}}
	allowed := []string{"actions/checkout@v5", "actions/SETUP-go@v6", "docker/build-push-action@v6"}
	for _, uses := range allowed {
		r, err := ParseRef(uses)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Permits(r); err != nil {
			t.Errorf("%s should be allowed: %v", uses, err)
		}
	}
	for _, uses := range []string{"evil/checkout@v5", "actions/checkout/nested/deep@v5", "docker/other@v1"} {
		r, err := ParseRef(uses)
		if err != nil {
			t.Fatal(err)
		}
		if uses == "actions/checkout/nested/deep@v5" {
			// A subdirectory does not change whose repository it is.
			if err := p.Permits(r); err != nil {
				t.Errorf("a path inside an allowed repo should be allowed: %v", err)
			}
			continue
		}
		if err := p.Permits(r); err == nil {
			t.Errorf("%s should be refused", uses)
		}
	}

	// `*` covers one segment, so admitting everything has to be written out.
	if (Policy{Allowed: []string{"*"}}).allows("actions/checkout") {
		t.Error("a bare * must not match a two-segment slug")
	}
	if !(Policy{Allowed: []string{"*/*"}}).allows("anyone/anything") {
		t.Error("*/* is how an operator says every action")
	}
}

// The two refusals have different fixes — one is a config line, the other is a
// workflow edit — so they must not collapse into one message.
func TestPolicyPinningIsASeparateRefusal(t *testing.T) {
	p := Policy{Allowed: []string{"actions/*"}, RequirePinned: true}
	tag, _ := ParseRef("actions/checkout@v5")
	err := p.Permits(tag)
	if err == nil {
		t.Fatal("a moving ref was admitted under require_pinned")
	}
	if !strings.Contains(err.Error(), "commit id") || strings.Contains(err.Error(), "allowed") {
		t.Errorf("the pinning refusal should be about pinning: %v", err)
	}

	p.RequirePinned = false
	if err := p.Permits(tag); err != nil {
		t.Errorf("with pinning off a tag is allowed: %v", err)
	}
}

// fakeGit records argv and answers from a script.
type fakeGit struct {
	calls [][]string
	out   map[string]string
	err   error
}

func (f *fakeGit) Run(_ context.Context, dir string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{dir}, args...))
	if f.err != nil {
		return "", f.err
	}
	return f.out[args[0]], nil
}

func newCache(t *testing.T, g Runner) *Cache {
	t.Helper()
	return &Cache{
		Root:   t.TempDir(),
		Policy: Policy{Allowed: []string{"*/*"}},
		Git:    g,
	}
}

// A tag is dereferenced to the commit it names, and the cache is keyed on that
// commit — so a tag that moves cannot be served from what it used to point at.
func TestFetchResolvesATagAndKeysOnTheCommit(t *testing.T) {
	other := strings.Repeat("b", 40)
	g := &fakeGit{out: map[string]string{
		// An annotated tag: the tag object first, its commit second.
		"ls-remote": other + "\trefs/tags/v5\n" + pin + "\trefs/tags/v5^{}",
	}}
	c := newCache(t, g)
	r, _ := ParseRef("actions/checkout@v5")

	got, err := c.Fetch(context.Background(), r)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.SHA != pin {
		t.Errorf("sha = %q, want the dereferenced commit %q", got.SHA, pin)
	}
	if !strings.HasSuffix(got.Dir, filepath.Join("github.com", "actions", "checkout", pin)) {
		t.Errorf("dir = %q, want it keyed on the commit", got.Dir)
	}

	// Every workflow-supplied value sits behind a terminator.
	for _, call := range g.calls {
		for i, a := range call {
			if a == "--end-of-options" {
				continue
			}
			if strings.HasPrefix(a, "https://") || a == "v5" || a == pin {
				if !containsBefore(call, "--end-of-options", i) {
					t.Errorf("%q is not behind a terminator in %v", a, call)
				}
			}
		}
	}
}

func containsBefore(argv []string, want string, before int) bool {
	for i := 0; i < before && i < len(argv); i++ {
		if argv[i] == want {
			return true
		}
	}
	return false
}

// A pinned reference needs no network at all before the fetch.
func TestFetchSkipsResolutionForAPin(t *testing.T) {
	g := &fakeGit{}
	c := newCache(t, g)
	r, _ := ParseRef("actions/checkout@" + pin)
	if _, err := c.Fetch(context.Background(), r); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, call := range g.calls {
		if len(call) > 1 && call[1] == "ls-remote" {
			t.Error("a pinned ref was resolved over the network")
		}
	}
}

// A second run finds the tree already there and fetches nothing.
func TestFetchIsCachedOnTheSecondCall(t *testing.T) {
	g := &fakeGit{}
	c := newCache(t, g)
	r, _ := ParseRef("actions/checkout@" + pin)
	if _, err := c.Fetch(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	before := len(g.calls)
	got, err := c.Fetch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cached {
		t.Error("the second fetch should report a cache hit")
	}
	if len(g.calls) != before {
		t.Errorf("the second fetch ran %d more git commands", len(g.calls)-before)
	}
}

// Policy is checked before the network, so a refusal costs a string compare.
func TestFetchChecksPolicyBeforeTouchingTheNetwork(t *testing.T) {
	g := &fakeGit{}
	c := newCache(t, g)
	c.Policy = DefaultPolicy()
	r, _ := ParseRef("actions/checkout@" + pin)
	if _, err := c.Fetch(context.Background(), r); err == nil {
		t.Fatal("a refused action was fetched")
	}
	if len(g.calls) != 0 {
		t.Errorf("a refused action ran git: %v", g.calls)
	}
}

// The action's own .git is dropped: it is the bulk of the fetch, and a second
// repository inside a tree a build will later see is a way for a `git` command
// in a step to find the wrong one.
func TestFetchDropsTheActionsOwnGitDir(t *testing.T) {
	c := newCache(t, &gitMaterialiser{})
	r, _ := ParseRef("actions/checkout@" + pin)
	got, err := c.Fetch(context.Background(), r)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git survived in %s", got.Dir)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "action.yml")); err != nil {
		t.Errorf("the checkout is missing its content: %v", err)
	}
}

// gitMaterialiser writes what a real checkout would leave behind.
type gitMaterialiser struct{}

func (gitMaterialiser) Run(_ context.Context, dir string, args ...string) (string, error) {
	if args[0] == "checkout" {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(dir, "action.yml"), []byte("runs:\n  using: composite\n"), 0o600)
	}
	return "", nil
}

// A subdirectory that is not there should say so, rather than handing back a
// path nothing reads.
func TestFetchRequiresThePathToExist(t *testing.T) {
	c := newCache(t, &gitMaterialiser{})
	r, _ := ParseRef("actions/checkout/missing@" + pin)
	if _, err := c.Fetch(context.Background(), r); err == nil {
		t.Fatal("a missing subdirectory was accepted")
	}
}

func TestDescribe(t *testing.T) {
	if got := DefaultPolicy().Describe(); !strings.Contains(got, "refused") {
		t.Errorf("Describe = %q", got)
	}
	p := Policy{Allowed: []string{"docker/*", "actions/*"}, RequirePinned: true, AllowUnisolated: true}
	got := p.Describe()
	for _, want := range []string{"actions/*, docker/*", "pinned", "native"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, missing %q", got, want)
		}
	}
}
