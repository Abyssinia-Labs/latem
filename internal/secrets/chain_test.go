package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// stubProvider is a Provider with no machinery behind it, for testing the
// chain's ordering rules in isolation.
type stubProvider struct {
	name   string
	values map[string]string
	err    error
	calls  int
}

func (s *stubProvider) Resolve(_ context.Context, name string) (Value, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[name]
	if !ok {
		return nil, ErrNotFound
	}
	return Value(v), nil
}

func (s *stubProvider) Name() string { return s.name }

func TestChain_FirstHitWins(t *testing.T) {
	first := &stubProvider{name: "env", values: map[string]string{"A": "from-env"}}
	second := &stubProvider{name: "file", values: map[string]string{"A": "from-file", "B": "from-file"}}
	c := NewChain(first, second)

	v, source, err := c.ResolveFrom(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "from-env" || source != "env" {
		t.Errorf("A = %q from %q, want from-env from env", v, source)
	}
	if second.calls != 0 {
		t.Error("a later provider was consulted after an earlier one answered")
	}

	v, source, err = c.ResolveFrom(t.Context(), "B")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "from-file" || source != "file" {
		t.Errorf("B = %q from %q", v, source)
	}
}

// "secrets.env is world-readable" and "the keychain is locked" must not
// degrade into "secret not found", which is the message an operator gets for a
// typo and sends them to the wrong half of the problem.
func TestChain_AHardErrorStopsTheChain(t *testing.T) {
	broken := &stubProvider{name: "file", err: errors.New("mode 0644")}
	later := &stubProvider{name: "keychain", values: map[string]string{"A": "from-keychain"}}
	c := NewChain(broken, later)

	_, _, err := c.ResolveFrom(t.Context(), "A")
	if err == nil {
		t.Fatal("a broken provider was skipped and the secret resolved anyway")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a hard error was reported as a miss")
	}
	if later.calls != 0 {
		t.Error("the chain continued past a hard error")
	}
}

// Exporting LATEM_SECRET_X= to override a stale keychain entry has to mean the
// override took effect.
func TestChain_AnEmptyValueIsAnAnswer(t *testing.T) {
	first := &stubProvider{name: "env", values: map[string]string{"A": ""}}
	second := &stubProvider{name: "keychain", values: map[string]string{"A": "stale"}}
	c := NewChain(first, second)

	v, source, err := c.ResolveFrom(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("value = %q, want the empty override", v)
	}
	if source != "env" {
		t.Errorf("source = %q, want env", source)
	}
	if second.calls != 0 {
		t.Error("an empty override fell through to a stale entry")
	}
}

// "keychain: no item; pass: gpg exited 2" is the difference between a typo and
// a broken GPG agent, and neither is visible from "not found" alone.
func TestChain_UnresolvedNameKeepsEveryNearMiss(t *testing.T) {
	c := NewChain(
		&stubProvider{name: "keychain", err: fmt.Errorf("keychain: no item: %w", ErrNotFound)},
		&stubProvider{name: "pass", err: fmt.Errorf("pass: gpg exited 2: %w", ErrNotFound)},
	)
	_, _, err := c.ResolveFrom(t.Context(), "A")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	for _, want := range []string{"A", "keychain", "gpg exited 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

func TestChain_EmptyChainIsAMiss(t *testing.T) {
	if _, err := NewChain().Resolve(t.Context(), "A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestChain_DropsNilProviders(t *testing.T) {
	c := NewChain(nil, &stubProvider{name: "env"}, nil)
	if got := c.Providers(); !slices.Equal(got, []string{"env"}) {
		t.Errorf("Providers() = %q", got)
	}
	if c.Name() != "chain" {
		t.Errorf("Name() = %q", c.Name())
	}
}

func TestChain_RejectsABadName(t *testing.T) {
	c := NewChain(&stubProvider{name: "env", values: map[string]string{"-w": "x"}})
	if _, err := c.Resolve(t.Context(), "-w"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
}

func TestChain_HonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p := &stubProvider{name: "env", values: map[string]string{"A": "x"}}
	if _, _, err := NewChain(p).ResolveFrom(ctx, "A"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if p.calls != 0 {
		t.Error("a provider was consulted after the run was cancelled")
	}
}

func TestNew_BuildsTheSpecChainInOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("A=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := New(Options{
		Environ:   []string{"LATEM_SECRET_A=x", "OPENVAULT_HOME=" + dir},
		FilePath:  path,
		Keychain:  On,
		Pass:      PassOptions{Binary: "pass"},
		Commands:  map[string]string{"A": "op read op://x/y"},
		OpenVault: true,
		Runner:    answering("x"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// OpenVault straight after the override: it is the copy every machine
	// shares, and the stores after it are the copies that go stale (A86).
	want := []string{"env", "openvault", "file", "keychain", "pass", "command"}
	if got := c.Providers(); !slices.Equal(got, want) {
		t.Errorf("Providers() = %q, want %q", got, want)
	}
}

// A provider that is not configured should not be in the chain producing a
// failed exec per secret per run.
func TestNew_OmitsUnconfiguredProviders(t *testing.T) {
	c, err := New(Options{Environ: []string{}, Keychain: Off, Runner: answering("")})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Providers(); !slices.Equal(got, []string{"env"}) {
		t.Errorf("Providers() = %q, want only env", got)
	}
}

// Options{} must not silently mean "no Keychain" on the one platform the whole
// mobile pipeline runs on.
func TestNew_KeychainFollowsThePlatformByDefault(t *testing.T) {
	c, err := New(Options{Environ: []string{}, Runner: answering("")})
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Contains(c.Providers(), "keychain")
	want := runtime.GOOS == "darwin"
	if got != want {
		t.Errorf("keychain present = %v on %s, want %v", got, runtime.GOOS, want)
	}
}

// A machine whose [secrets.commands] entry does not parse has a config bug,
// and starting anyway means it surfaces days later in an unrelated build.
func TestNew_RejectsBadConfiguration(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"unparseable command", Options{Commands: map[string]string{"A": "op read $VAULT"}}},
		{"bad keychain service", Options{Keychain: On, KeychainService: "-w"}},
		{"bad pass prefix", Options{Pass: PassOptions{Binary: "pass", Prefix: "../etc"}}},
		{"unknown pass binary", Options{Pass: PassOptions{Binary: "curl"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Environ = []string{}
			tc.opts.Runner = answering("")
			if _, err := New(tc.opts); err == nil {
				t.Error("a broken configuration was accepted")
			}
		})
	}
}

func TestNew_DefaultsToTheProcessEnvironmentAndRealRunner(t *testing.T) {
	t.Setenv("LATEM_SECRET_TEST_ONLY_A", "hunter2")

	c, err := New(Options{Keychain: Off})
	if err != nil {
		t.Fatal(err)
	}
	v, source, err := c.ResolveFrom(t.Context(), "TEST_ONLY_A")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "hunter2" || source != "env" {
		t.Errorf("value = %q from %q", v, source)
	}
}

func TestToggle(t *testing.T) {
	tests := []struct {
		toggle          Toggle
		platformDefault bool
		want            bool
	}{
		{Auto, true, true},
		{Auto, false, false},
		{On, false, true},
		{Off, true, false},
	}
	for _, tc := range tests {
		if got := tc.toggle.enabled(tc.platformDefault); got != tc.want {
			t.Errorf("Toggle(%d).enabled(%v) = %v, want %v", tc.toggle, tc.platformDefault, got, tc.want)
		}
	}
}

// scopedStub is a ScopedProvider that records what it was asked.
type scopedStub struct {
	values map[string]string // "scope parts/NAME", parts joined by "/"
	asked  []string
}

func (s *scopedStub) Resolve(ctx context.Context, name string) (Value, error) {
	return s.ResolveIn(ctx, nil, name)
}

func (s *scopedStub) ResolveIn(_ context.Context, scope []string, name string) (Value, error) {
	key := strings.Join(append(slices.Clone(scope), name), "/")
	s.asked = append(s.asked, key)
	if v, ok := s.values[key]; ok {
		return Value(v), nil
	}
	return nil, ErrNotFound
}

func (s *scopedStub) Name() string { return "scoped" }

func (s *scopedStub) Where(scope []string, name string) string {
	return strings.Join(append(slices.Clone(scope), name), "/")
}

func TestChain_AScopedStoreGetsTheScopesPartsAndAFlatStoreTheFlatName(t *testing.T) {
	flat := &stubProvider{name: "file", values: map[string]string{}}
	scoped := &scopedStub{values: map[string]string{"latem-cloud/production/TOKEN": "vault"}}
	c := NewChain(flat, scoped)

	v, source, err := c.ResolveInFrom(context.Background(), []string{"latem-cloud", "production"}, "TOKEN")
	if err != nil || string(v) != "vault" {
		t.Fatalf("ResolveInFrom = %q, %v", v, err)
	}
	if source != "scoped as latem-cloud/production/TOKEN" {
		t.Errorf("source = %q, want the entry it was found as", source)
	}
	if flat.calls != 1 {
		t.Errorf("flat store asked %d times", flat.calls)
	}
	if !slices.Equal(scoped.asked, []string{"latem-cloud/production/TOKEN"}) {
		t.Errorf("scoped store asked %q; the hyphen must reach it, since latem_cloud names no collection", scoped.asked)
	}
}

func TestChain_AScopeNoFlatNameCanHoldStillReachesAScopedStore(t *testing.T) {
	flat := &stubProvider{name: "file", values: map[string]string{}}
	scoped := &scopedStub{values: map[string]string{"1password-sync/TOKEN": "vault"}}

	v, err := NewChain(flat, scoped).ResolveIn(context.Background(), []string{"1password-sync"}, "TOKEN")
	if err != nil || string(v) != "vault" {
		t.Fatalf("ResolveIn = %q, %v", v, err)
	}
	if flat.calls != 0 {
		t.Error("a flat store was asked for a name starting with a digit")
	}
}

func TestResolveScoped_AScopedStoresAnswerNamesTheEntryNotTheFlatName(t *testing.T) {
	scoped := &scopedStub{values: map[string]string{"app/production/TOKEN": longEnough("vault")}}
	set, _, err := ResolveScoped(context.Background(), NewChain(scoped), &fakeRegistrar{},
		[]string{"TOKEN"}, [][]string{{"app", "production"}, {"app"}, nil})
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Source("TOKEN"); got != "scoped as app/production/TOKEN" {
		t.Errorf("source = %q", got)
	}
}
