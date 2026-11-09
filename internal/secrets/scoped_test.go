package secrets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestScopedName(t *testing.T) {
	cases := map[string]string{
		ScopedName("TOKEN"):                          "TOKEN",
		ScopedName("TOKEN", "laemat"):                "laemat__TOKEN",
		ScopedName("TOKEN", "laemat", "production"):  "laemat__production__TOKEN",
		ScopedName("TOKEN", "ios-native", "prod.eu"): "ios_native__prod_eu__TOKEN",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// A50: most specific scope first, the value kept under the bare name, an
// unset name reported rather than fatal, and every value registered.
func TestResolveScoped(t *testing.T) {
	reg := &fakeRegistrar{}
	p := &registeringProvider{reg: reg, t: t, values: map[string]string{
		"app__production__DEPLOY_KEY": longEnough("prod-key"),
		"app__DEPLOY_KEY":             longEnough("repo-key"),
		"DEPLOY_KEY":                  longEnough("machine-key"),
		"app__ACCOUNT":                longEnough("repo-account"),
		"SHARED":                      longEnough("machine-shared"),
		"app__EMPTY":                  "",
	}}
	scopes := [][]string{{"app", "production"}, {"app"}, nil}

	set, missing, err := ResolveScoped(context.Background(), p, reg,
		[]string{"DEPLOY_KEY", "ACCOUNT", "SHARED", "UNSET", "EMPTY", "DEPLOY_KEY"}, scopes)
	if err != nil {
		t.Fatalf("ResolveScoped: %v", err)
	}
	want := map[string]string{
		"DEPLOY_KEY": longEnough("prod-key"),
		"ACCOUNT":    longEnough("repo-account"),
		"SHARED":     longEnough("machine-shared"),
	}
	for name, v := range want {
		got, ok := set.Value(name)
		if !ok || string(got) != v {
			t.Errorf("%s = %q, want %q", name, got, v)
		}
	}
	if !slices.Equal(missing, []string{"UNSET", "EMPTY"}) {
		t.Errorf("missing = %v, want UNSET and EMPTY", missing)
	}
	if got := set.Source("DEPLOY_KEY"); !strings.Contains(got, "as app__production__DEPLOY_KEY") {
		t.Errorf("source = %q, should name the scoped name it was found as", got)
	}
	if got := len(reg.registered()); got != 3 {
		t.Errorf("%d values registered, want 3", got)
	}

	// Without an environment the repository scope is the most specific. A
	// fresh provider: the one above handed out an empty value, which is never
	// registered, and its INV-4 bookkeeping counts every value handed out.
	reg2 := &fakeRegistrar{}
	p2 := &registeringProvider{reg: reg2, t: t, values: p.values}
	set, _, err = ResolveScoped(context.Background(), p2, reg2, []string{"DEPLOY_KEY"}, [][]string{{"app"}, nil})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := set.Value("DEPLOY_KEY"); string(got) != longEnough("repo-key") {
		t.Errorf("DEPLOY_KEY = %q, want the repository's", got)
	}
}

// A called workflow's job reads a secret as the name its caller passed it as,
// kept under its own name; one with no candidates is missing (A82).
func TestResolveAs(t *testing.T) {
	reg := &fakeRegistrar{}
	p := &registeringProvider{reg: reg, t: t, values: map[string]string{
		"app__NPM_TOKEN": longEnough("repo-npm"),
		"NPM":            longEnough("machine-npm"),
	}}
	lookup := func(name string) []string {
		if name == "NPM" {
			return []string{"app__production__NPM", "app__NPM_TOKEN", "NPM_TOKEN"}
		}
		return nil
	}
	set, missing, err := ResolveAs(context.Background(), p, reg, []string{"NPM", "OTHER"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := set.Value("NPM"); string(got) != longEnough("repo-npm") {
		t.Errorf("NPM = %q, want the repository's NPM_TOKEN", got)
	}
	if !slices.Equal(missing, []string{"OTHER"}) {
		t.Errorf("missing = %v", missing)
	}
}

type brokenProvider struct{}

func (brokenProvider) Resolve(context.Context, string) (Value, error) {
	return nil, errors.New("keychain locked")
}
func (brokenProvider) Name() string { return "broken" }

// A backend that cannot answer is not an unset secret.
func TestResolveScoped_BackendFailureIsAnError(t *testing.T) {
	_, _, err := ResolveScoped(context.Background(), brokenProvider{}, &fakeRegistrar{},
		[]string{"TOKEN"}, [][]string{{"app"}, nil})
	if err == nil || !strings.Contains(err.Error(), "app__TOKEN") {
		t.Errorf("want an error naming the lookup, got %v", err)
	}
}
