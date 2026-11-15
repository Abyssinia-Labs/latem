package secrets

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// typedVault answers list and get from entries keyed collection + "/" + entry,
// each with its kind, and collections with the collections they name.
type typedEntry struct {
	kind  byte
	value string
}

func typedVault(entries map[string]typedEntry) func(r ovRequest) []byte {
	collections := func() []string {
		var out []string
		for k := range entries {
			c, _, _ := cutFirst(k)
			out = appendUnique(out, c)
		}
		return out
	}
	return func(r ovRequest) []byte {
		switch r.typ {
		case ovCollections:
			return ovNameList(collections()...)
		case ovList:
			c := r.fields[0]
			listed := map[string]byte{}
			for k, e := range entries {
				if col, name, _ := cutFirst(k); col == c {
					listed[name] = e.kind
				}
			}
			if len(listed) == 0 {
				return ovRefusal("no collection " + c)
			}
			return ovListingOf(listed)
		case ovGet:
			if e, ok := entries[r.fields[0]+"/"+r.fields[1]]; ok {
				return ovFound(e.kind, e.value)
			}
			return ovNotFound()
		}
		return ovRefusal("unexpected request")
	}
}

func cutFirst(k string) (string, string, bool) { return strings.Cut(k, "/") }

func appendUnique(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

// A96: a repository's variables are the variable entries of its collection,
// at the top for every job and one folder deep for an environment's.
func TestOpenVaultVariables_AreTheCollectionsVariableEntriesByEnvironment(t *testing.T) {
	v := newFakeVault(t, typedVault(map[string]typedEntry{
		"tsedal/LATEM":             {ovVar, "true"},
		"tsedal/staging/BASE_URL":  {ovVar, "https://staging.example.invalid"},
		"tsedal/production/REGION": {ovVar, "eu"},
	}))
	got, err := NewOpenVaultProvider(v.socket, 0).Variables(context.Background(), "tsedal", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Variables{
		"":           {"LATEM": "true"},
		"staging":    {"BASE_URL": "https://staging.example.invalid"},
		"production": {"REGION": "eu"},
	}
	if !variablesEqual(got, want) {
		t.Errorf("Variables = %v, want %v", got, want)
	}
}

// A secret is masked in a log and a variable is not: no secret, nor a file,
// ever answers vars, whatever it is called.
func TestOpenVaultVariables_ASecretOrAFileNeverAnswers(t *testing.T) {
	v := newFakeVault(t, typedVault(map[string]typedEntry{
		"app/DEPLOY_KEY":            {ovSecret, "not-a-variable-0123"},
		"app/production/DEPLOY_KEY": {ovSecret, "not-a-variable-4567"},
		"app/google-services.json":  {ovFile, "{}"},
		"app/REGION":                {ovVar, "eu"},
	}))
	p := NewOpenVaultProvider(v.socket, 0)
	got, err := p.Variables(context.Background(), "app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Variables{"": {"REGION": "eu"}}); !variablesEqual(got, want) {
		t.Errorf("Variables = %v, want %v", got, want)
	}
	for _, r := range v.got() {
		if r.typ == ovGet && r.fields[1] != "REGION" {
			t.Errorf("read %s/%s, which is no variable: its value has no business here", r.fields[0], r.fields[1])
		}
	}
}

// The listing and the read are two requests. An entry that became a secret
// between them is left out rather than read into vars.
func TestOpenVaultVariables_AnEntryThatIsNoLongerAVariableIsLeftOut(t *testing.T) {
	v := newFakeVault(t, func(r ovRequest) []byte {
		switch r.typ {
		case ovList:
			return ovListingOf(map[string]byte{"TOKEN": ovVar, "GONE": ovVar})
		case ovGet:
			if r.fields[1] == "TOKEN" {
				return ovFound(ovSecret, "now-a-secret-0123")
			}
			return ovNotFound()
		}
		return ovRefusal("unexpected request")
	})
	got, err := NewOpenVaultProvider(v.socket, 0).Variables(context.Background(), "app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Variables = %v, want none", got)
	}
}

// envs narrows the reads: the heartbeat reports only a repository's own
// variables, and reads no environment's.
func TestOpenVaultVariables_OnlyTheEnvironmentsAskedForAreRead(t *testing.T) {
	v := newFakeVault(t, typedVault(map[string]typedEntry{
		"app/LATEM":            {ovVar, "true"},
		"app/production/LATEM": {ovVar, "false"},
	}))
	got, err := NewOpenVaultProvider(v.socket, 0).Variables(context.Background(), "app",
		func(env string) bool { return env == "" })
	if err != nil {
		t.Fatal(err)
	}
	if want := (Variables{"": {"LATEM": "true"}}); !variablesEqual(got, want) {
		t.Errorf("Variables = %v, want %v", got, want)
	}
	gets := 0
	for _, r := range v.got() {
		if r.typ == ovGet {
			gets++
		}
	}
	if gets != 1 {
		t.Errorf("%d reads, want 1: production's was not asked for", gets)
	}
}

// A name no workflow could read, deeper than one folder, or GitHub's own, is
// no variable here.
func TestOpenVaultVariables_ANameNoWorkflowCouldReadIsLeftOut(t *testing.T) {
	v := newFakeVault(t, typedVault(map[string]typedEntry{
		"app/a/b/DEEP":     {ovVar, "x"},
		"app/GITHUB_TOKEN": {ovVar, "x"},
		"app/has-hyphen":   {ovVar, "x"},
		"app/OK":           {ovVar, "y"},
	}))
	got, err := NewOpenVaultProvider(v.socket, 0).Variables(context.Background(), "app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Variables{"": {"OK": "y"}}); !variablesEqual(got, want) {
		t.Errorf("Variables = %v, want %v", got, want)
	}
}

// A machine never scoped to the collection holds none of its variables: a
// miss, so config.toml answers.
func TestOpenVaultVariables_ACollectionThisMachineDoesNotHoldIsNoCollection(t *testing.T) {
	v := newFakeVault(t, typedVault(map[string]typedEntry{"other/X": {ovVar, "1"}}))
	_, err := NewOpenVaultProvider(v.socket, 0).Variables(context.Background(), "app", nil)
	if !errors.Is(err, ErrNoCollection) || !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNoCollection", err)
	}
}

// A service that is not running is a backend down, never "no variables".
func TestOpenVaultVariables_AServiceThatIsNotRunningIsABackendFailure(t *testing.T) {
	socket := filepath.Join(filepath.Dir(ovSocketPath(t)), "absent.sock")
	_, err := NewOpenVaultProvider(socket, 0).Variables(context.Background(), "app", nil)
	var oe *OpenVaultError
	if !errors.As(err, &oe) || !oe.NotRunning {
		t.Fatalf("err = %v, want a not-running OpenVaultError", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a stopped service read as no variables: a job would run with them empty")
	}
}

func variablesEqual(a, b Variables) bool {
	return maps.EqualFunc(a, b, maps.Equal[map[string]string])
}
