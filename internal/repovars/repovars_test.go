package repovars

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

type fakeVault struct {
	vars  map[string]secrets.Variables // collection -> what it holds
	err   error
	asked []string
}

func (f *fakeVault) Variables(_ context.Context, collection string, envs func(string) bool) (secrets.Variables, error) {
	f.asked = append(f.asked, collection)
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vars[collection]
	if !ok {
		return nil, secrets.ErrNoCollection
	}
	out := secrets.Variables{}
	for env, m := range v {
		if envs == nil || envs(env) {
			out[env] = m
		}
	}
	return out, nil
}

func tsedal() *config.Repo {
	return &config.Repo{
		Vars: map[string]string{"LATEM": "false", "REGION": "us"},
		Environments: map[string]config.RepoEnvironment{
			"staging": {Vars: map[string]string{"BASE_URL": "https://old.example.invalid", "ONLY_HERE": "x"}},
		},
	}
}

// A96: OpenVault comes before config.toml within each scope, and a more
// specific scope before a wider one, as for secrets (§15).
func TestFor_TheVaultComesFirstInEachScopeAndTheEnvironmentOverTheRepository(t *testing.T) {
	v := &fakeVault{vars: map[string]secrets.Variables{"tsedal": {
		"":        {"LATEM": "true"},
		"staging": {"BASE_URL": "https://staging.example.invalid", "REGION": "eu"},
	}}}
	s, err := Read(context.Background(), v, "tsedal", tsedal())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.For(""), map[string]string{"LATEM": "true", "REGION": "us"}; !maps.Equal(got, want) {
		t.Errorf(`For("") = %v, want %v`, got, want)
	}
	want := map[string]string{
		"LATEM":     "true",                            // the vault's, over config.toml's
		"REGION":    "eu",                              // the environment's in the vault, over the repository's
		"BASE_URL":  "https://staging.example.invalid", // the vault's environment entry, over config.toml's
		"ONLY_HERE": "x",
	}
	if got := s.For("staging"); !maps.Equal(got, want) {
		t.Errorf(`For("staging") = %v, want %v`, got, want)
	}
	if got := s.Source("BASE_URL", "staging"); got != FromOpenVault {
		t.Errorf("BASE_URL's source = %q", got)
	}
	if got := s.Source("ONLY_HERE", "staging"); got != FromConfig {
		t.Errorf("ONLY_HERE's source = %q", got)
	}
}

// Without OpenVault, a repository's variables are config.toml's, as A50 had
// them.
func TestFor_WithoutAVaultIsConfigs(t *testing.T) {
	s, err := Read(context.Background(), nil, "tsedal", tsedal())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.For("staging"), tsedal().VarsFor("staging"); !maps.Equal(got, want) {
		t.Errorf("For = %v, want config.toml's %v", got, want)
	}
}

// A collection this machine does not hold is the vault having none of the
// repository's variables, not a failure.
func TestRead_ACollectionNotHeldHereIsConfigsAlone(t *testing.T) {
	v := &fakeVault{vars: map[string]secrets.Variables{}}
	s, err := Read(context.Background(), v, "tsedal", tsedal())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.For(""); got["LATEM"] != "false" {
		t.Errorf("For = %v, want config.toml's", got)
	}
}

// A service that does not answer is an error, never an empty set.
func TestRead_AServiceThatDoesNotAnswerIsAnError(t *testing.T) {
	down := &secrets.OpenVaultError{Op: "list", NotRunning: true}
	_, err := Read(context.Background(), &fakeVault{err: down}, "tsedal", tsedal())
	var oe *secrets.OpenVaultError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want the service's failure", err)
	}
}

// latem vars list says which store holds each name and which answers.
func TestList_NamesEachScopesStoresTheVaultFirst(t *testing.T) {
	v := &fakeVault{vars: map[string]secrets.Variables{"tsedal": {
		"":           {"LATEM": "true"},
		"production": {"REGION": "eu"},
	}}}
	s, err := Read(context.Background(), v, "tsedal", tsedal())
	if err != nil {
		t.Fatal(err)
	}
	got := s.List()
	want := []Var{
		{Env: "", Name: "LATEM", Value: "true", Sources: []string{FromOpenVault, FromConfig}},
		{Env: "", Name: "REGION", Value: "us", Sources: []string{FromConfig}},
		{Env: "production", Name: "REGION", Value: "eu", Sources: []string{FromOpenVault}},
		{Env: "staging", Name: "BASE_URL", Value: "https://old.example.invalid", Sources: []string{FromConfig}},
		{Env: "staging", Name: "ONLY_HERE", Value: "x", Sources: []string{FromConfig}},
	}
	if !slices.EqualFunc(got, want, func(a, b Var) bool {
		return a.Env == b.Env && a.Name == b.Name && a.Value == b.Value && slices.Equal(a.Sources, b.Sources)
	}) {
		t.Errorf("List =\n%+v\nwant\n%+v", got, want)
	}
}
