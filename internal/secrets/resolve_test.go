package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeRegistrar records both a copy of what it was given and the live slice.
// The copy is what assertions read; the live slice is how the tests prove that
// Set.Zero reaches the same buffer that was registered.
type fakeRegistrar struct {
	mu     sync.Mutex
	copies [][]byte
	live   [][]byte
}

func (r *fakeRegistrar) Register(value []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.copies = append(r.copies, bytes.Clone(value))
	r.live = append(r.live, value)
}

func (r *fakeRegistrar) registered() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.copies))
	for _, v := range r.copies {
		out = append(out, string(v))
	}
	slices.Sort(out)
	return out
}

// registeringProvider fails the test if a value is handed out before it has
// been registered. It is the mechanical form of INV-4's ordering obligation.
type registeringProvider struct {
	values map[string]string
	reg    *fakeRegistrar
	t      *testing.T
	seen   []string
}

func (p *registeringProvider) Resolve(_ context.Context, name string) (Value, error) {
	v, ok := p.values[name]
	if !ok {
		return nil, ErrNotFound
	}
	p.seen = append(p.seen, name)
	// Every value resolved before this one must already be registered.
	p.reg.mu.Lock()
	registered := len(p.reg.copies)
	p.reg.mu.Unlock()
	if registered != len(p.seen)-1 {
		p.t.Errorf("resolving %s with %d values registered, want %d: a value existed unregistered "+
			"across a provider call (INV-4)", name, registered, len(p.seen)-1)
	}
	return Value(v), nil
}

func (p *registeringProvider) Name() string { return "test" }

func longEnough(s string) string {
	for len(s) < logstream.MinRedactableLength {
		s += "x"
	}
	return s
}

func TestResolveAll_ResolvesAndRegisters(t *testing.T) {
	reg := &fakeRegistrar{}
	p := NewChain(&stubProvider{name: "env", values: map[string]string{
		"MATCH_PASSWORD":           "hunter2!!",
		"APP_STORE_CONNECT_KEY_P8": pemFixture,
	}})

	set, err := ResolveAll(t.Context(), p, reg, []string{"MATCH_PASSWORD", "APP_STORE_CONNECT_KEY_P8"})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Zero()

	if got := set.Len(); got != 2 {
		t.Errorf("Len() = %d, want 2", got)
	}
	if got := set.Names(); !slices.Equal(got, []string{"MATCH_PASSWORD", "APP_STORE_CONNECT_KEY_P8"}) {
		t.Errorf("Names() = %q, not in declaration order", got)
	}

	v, ok := set.Value("MATCH_PASSWORD")
	if !ok || string(v) != "hunter2!!" {
		t.Errorf("Value = %q, %v", v, ok)
	}
	if set.Source("MATCH_PASSWORD") != "env" {
		t.Errorf("Source = %q, want env", set.Source("MATCH_PASSWORD"))
	}

	// Registered before the Set was observable, which is the whole obligation.
	want := []string{"hunter2!!", pemFixture}
	slices.Sort(want)
	if got := reg.registered(); !slices.Equal(got, want) {
		t.Errorf("registered %q, want %q", got, want)
	}
	if len(set.Warnings()) != 0 {
		t.Errorf("Warnings() = %v", set.Warnings())
	}
}

// The mechanical form of "registered before the value is used for anything
// else" (§15).
func TestResolveAll_RegistersEachValueBeforeResolvingTheNext(t *testing.T) {
	reg := &fakeRegistrar{}
	p := &registeringProvider{
		t:   t,
		reg: reg,
		values: map[string]string{
			"A": longEnough("a"),
			"B": longEnough("b"),
			"C": longEnough("c"),
		},
	}
	set, err := ResolveAll(t.Context(), p, reg, []string{"A", "B", "C"})
	if err != nil {
		t.Fatal(err)
	}
	set.Zero()
}

// A declared secret that resolves empty is E_MISSING_SECRET at admission, not
// a runtime surprise 40 minutes in.
func TestResolveAll_MissingAndEmptyAreBothMissingSecret(t *testing.T) {
	reg := &fakeRegistrar{}
	p := NewChain(&stubProvider{name: "env", values: map[string]string{
		"PRESENT": "hunter2!!",
		"EMPTY":   "",
	}})

	set, err := ResolveAll(t.Context(), p, reg, []string{"PRESENT", "EMPTY", "ABSENT"})
	if set != nil {
		t.Error("a Set was returned alongside an error")
	}
	if err == nil {
		t.Fatal("a job with an unresolvable secret was admitted")
	}
	if got := CodeOf(err); got != protocol.ErrMissingSecret {
		t.Errorf("CodeOf = %q, want %q", got, protocol.ErrMissingSecret)
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %T, want *Error", err)
	}
	// Both failures reported together: an operator setting up a machine wants
	// the list, not a guessing game.
	if !slices.Equal(e.Names, []string{"ABSENT", "EMPTY"}) {
		t.Errorf("Names = %q, want both failures", e.Names)
	}
	for _, want := range []string{"ABSENT", "EMPTY", "empty value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

// Nothing that resolved may outlive a failed admission, and the buffer that
// has to be zeroed is the same one the redactor was handed.
func TestResolveAll_ZeroesWhatItResolvedWhenAdmissionFails(t *testing.T) {
	reg := &fakeRegistrar{}
	p := NewChain(&stubProvider{name: "env", values: map[string]string{"PRESENT": "hunter2!!"}})

	if _, err := ResolveAll(t.Context(), p, reg, []string{"PRESENT", "ABSENT"}); err == nil {
		t.Fatal("expected an error")
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.live) != 1 {
		t.Fatalf("registered %d values, want 1", len(reg.live))
	}
	for _, b := range reg.live[0] {
		if b != 0 {
			t.Fatalf("the resolved value survived a failed admission: %q", reg.live[0])
		}
	}
}

// Nothing else tells the operator that a secret is too short to be redacted.
func TestResolveAll_WarnsAboutShortValuesByNameOnly(t *testing.T) {
	reg := &fakeRegistrar{}
	short := "dev"
	p := NewChain(&stubProvider{name: "env", values: map[string]string{
		"SHORT": short,
		"LONG":  longEnough("hunter"),
	}})

	set, err := ResolveAll(t.Context(), p, reg, []string{"SHORT", "LONG"})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Zero()

	warnings := set.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("Warnings() = %v, want exactly one", warnings)
	}
	if warnings[0].Name != "SHORT" || warnings[0].Kind != WarnTooShort {
		t.Errorf("warning = %+v", warnings[0])
	}
	text := warnings[0].String()
	if !strings.Contains(text, "SHORT") {
		t.Errorf("warning %q does not name the secret", text)
	}
	if strings.Contains(text, short) {
		t.Errorf("warning %q quotes the value", text)
	}
}

func TestWarning_UnknownKind(t *testing.T) {
	got := Warning{Name: "A", Kind: WarningKind("weird")}.String()
	if !strings.Contains(got, "A") || !strings.Contains(got, "weird") {
		t.Errorf("String() = %q", got)
	}
}

// A run whose secrets were never registered logs them in cleartext. The
// convenience of a nil Registrar is indistinguishable from that bug.
func TestResolveAll_RequiresARegistrar(t *testing.T) {
	p := NewChain(&stubProvider{name: "env", values: map[string]string{"A": "hunter2!!"}})

	_, err := ResolveAll(t.Context(), p, nil, []string{"A"})
	if err == nil {
		t.Fatal("a nil Registrar was accepted")
	}
	if !strings.Contains(err.Error(), "INV-4") {
		t.Errorf("err = %q, which does not say which invariant it protects", err)
	}
	if CodeOf(err) != protocol.ErrMissingSecret {
		t.Errorf("CodeOf = %q", CodeOf(err))
	}
}

func TestResolveAll_RequiresAProvider(t *testing.T) {
	if _, err := ResolveAll(t.Context(), nil, &fakeRegistrar{}, []string{"A"}); err == nil {
		t.Error("a nil provider was accepted")
	}
}

func TestResolveAll_NoDeclaredSecrets(t *testing.T) {
	set, err := ResolveAll(t.Context(), NewChain(), &fakeRegistrar{}, nil)
	if err != nil {
		t.Fatalf("a job declaring no secrets failed admission: %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len() = %d", set.Len())
	}
}

func TestResolveAll_DeduplicatesNames(t *testing.T) {
	reg := &fakeRegistrar{}
	p := &stubProvider{name: "env", values: map[string]string{"A": "hunter2!!"}}

	set, err := ResolveAll(t.Context(), NewChain(p), reg, []string{"A", "A", "A"})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Zero()

	if p.calls != 1 {
		t.Errorf("provider called %d times for one distinct name", p.calls)
	}
	if set.Len() != 1 {
		t.Errorf("Len() = %d, want 1", set.Len())
	}
}

func TestResolveAll_RejectsANameThatIsNotUsable(t *testing.T) {
	_, err := ResolveAll(t.Context(), NewChain(&stubProvider{name: "env"}), &fakeRegistrar{},
		[]string{"-w"})
	if err == nil {
		t.Fatal("a secret name that is an option to `security` was accepted")
	}
	if CodeOf(err) != protocol.ErrMissingSecret {
		t.Errorf("CodeOf = %q", CodeOf(err))
	}
}

// A hard provider failure must stop admission rather than be reported as a
// missing secret alongside the others.
func TestResolveAll_APolicyFailureStopsImmediately(t *testing.T) {
	second := &stubProvider{name: "env", values: map[string]string{"B": "hunter2!!"}}
	p := NewChain(&stubProvider{name: "file", err: errors.New("mode 0644")})

	_, err := ResolveAll(t.Context(), p, &fakeRegistrar{}, []string{"A", "B"})
	if err == nil {
		t.Fatal("a broken provider was tolerated")
	}
	if !strings.Contains(err.Error(), "mode 0644") {
		t.Errorf("err = %q, which loses the operator's diagnosis", err)
	}
	if second.calls != 0 {
		t.Error("resolution continued past a hard failure")
	}
}

// A run torn down mid-resolution was interrupted, not misconfigured; reporting
// E_MISSING_SECRET would send an operator hunting for a credential that is
// fine.
func TestResolveAll_CancellationIsNotAMissingSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p := NewChain(&stubProvider{name: "env", values: map[string]string{"A": "hunter2!!"}})
	_, err := ResolveAll(ctx, p, &fakeRegistrar{}, []string{"A"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := CodeOf(err); got != "" {
		t.Errorf("CodeOf = %q, want the empty code so the orchestrator reports E_INTERRUPTED", got)
	}
}

// The point of []byte over string (finding F10): the buffer handed to a caller
// is the buffer Zero reaches.
func TestSet_ZeroOverwritesTheBufferItHandedOut(t *testing.T) {
	reg := &fakeRegistrar{}
	p := NewChain(&stubProvider{name: "env", values: map[string]string{"A": "hunter2!!"}})

	set, err := ResolveAll(t.Context(), p, reg, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}

	held, _ := set.Value("A")
	set.Zero()

	for _, b := range held {
		if b != 0 {
			t.Fatalf("a value handed to a caller survived Zero: %q", held)
		}
	}
	if _, ok := set.Value("A"); ok {
		t.Error("Value still answers after Zero")
	}
	if !set.Zeroed() {
		t.Error("Zeroed() = false after Zero")
	}

	// Idempotent, so the deferred call and the error-path call can both stand.
	set.Zero()

	// Provenance is not secret and a post-mortem still wants it.
	if set.Source("A") != "env" {
		t.Errorf("Source = %q after Zero, want env", set.Source("A"))
	}
	if !slices.Equal(set.Names(), []string{"A"}) {
		t.Errorf("Names() = %q after Zero", set.Names())
	}
}

func TestSet_IsSafeForConcurrentUse(t *testing.T) {
	reg := &fakeRegistrar{}
	p := NewChain(&stubProvider{name: "env", values: map[string]string{"A": "hunter2!!"}})
	set, err := ResolveAll(t.Context(), p, reg, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			set.Value("A")
			set.Names()
			set.Source("A")
			set.Warnings()
			set.Len()
			set.Zeroed()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		set.Zero()
	}()
	wg.Wait()
}

func TestDiagnose(t *testing.T) {
	p := NewChain(
		&stubProvider{name: "env", values: map[string]string{"FROM_ENV": longEnough("a"), "SHORT": "dev"}},
		&stubProvider{name: "keychain", values: map[string]string{"FROM_KEYCHAIN": longEnough("b"), "EMPTY": ""}},
	)

	got := Diagnose(t.Context(), p, []string{"FROM_ENV", "FROM_KEYCHAIN", "SHORT", "EMPTY", "ABSENT", "-w"})
	want := []Status{
		{Name: "FROM_ENV", Provider: "env", Found: true},
		{Name: "FROM_KEYCHAIN", Provider: "keychain", Found: true},
		{Name: "SHORT", Provider: "env", Found: true, Short: true},
		{Name: "EMPTY", Provider: "keychain", Problem: "resolved to an empty value"},
		{Name: "ABSENT", Problem: "no provider holds this secret"},
	}

	if len(got) != len(want)+1 {
		t.Fatalf("Diagnose returned %d statuses, want %d", len(got), len(want)+1)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("status %d = %+v, want %+v", i, got[i], w)
		}
	}
	if got[len(want)].Problem == "" {
		t.Error("an unusable secret name was reported as fine")
	}
}

// `latem doctor --secrets` reports names only, never values (§15).
func TestDiagnose_ReportsNoValues(t *testing.T) {
	const secret = "hunter2-do-not-print"
	p := NewChain(&stubProvider{name: "env", values: map[string]string{"A": secret}})

	for _, st := range Diagnose(t.Context(), p, []string{"A"}) {
		if strings.Contains(fmt.Sprintf("%+v", st), secret) {
			t.Errorf("Status = %+v leaks the value", st)
		}
	}
}

func TestDiagnose_HandlesABrokenProvider(t *testing.T) {
	p := NewChain(&stubProvider{name: "file", err: errors.New("mode 0644")})
	got := Diagnose(t.Context(), p, []string{"A"})
	if len(got) != 1 || got[0].Found || !strings.Contains(got[0].Problem, "mode 0644") {
		t.Errorf("Diagnose = %+v", got)
	}
}

func TestDiagnose_HandlesANilProvider(t *testing.T) {
	got := Diagnose(t.Context(), nil, []string{"A"})
	if len(got) != 1 || got[0].Problem == "" {
		t.Errorf("Diagnose = %+v", got)
	}
}

// Diagnose has nothing to attach an INV-4 ordering obligation to, so it must
// not leave values lying around either.
func TestDiagnose_ZeroesEveryValueItResolves(t *testing.T) {
	held := Value("hunter2!!")
	p := &sharedValueProvider{value: held}

	Diagnose(t.Context(), p, []string{"A"})

	for _, b := range held {
		if b != 0 {
			t.Fatalf("Diagnose left a value in memory: %q", held)
		}
	}
}

type sharedValueProvider struct{ value Value }

func (p *sharedValueProvider) Resolve(context.Context, string) (Value, error) { return p.value, nil }
func (p *sharedValueProvider) Name() string                                   { return "shared" }

func TestError_MessageShapes(t *testing.T) {
	cause := errors.New("boom")
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{"message and cause", &Error{Msg: "resolving A", Err: cause}, "E_MISSING_SECRET: resolving A: boom"},
		{"message only", &Error{Msg: "resolving A"}, "E_MISSING_SECRET: resolving A"},
		{"cause only", &Error{Err: cause}, "E_MISSING_SECRET: boom"},
		{"neither", &Error{}, "E_MISSING_SECRET"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if !errors.Is(tc.err, tc.err.Err) && tc.err.Err != nil {
				t.Error("Unwrap does not reach the cause")
			}
		})
	}
}

func TestCodeOf_IgnoresUnrelatedErrors(t *testing.T) {
	if got := CodeOf(errors.New("something else")); got != "" {
		t.Errorf("CodeOf = %q, want the empty code", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Errorf("CodeOf(nil) = %q", got)
	}
}
