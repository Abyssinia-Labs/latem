package secrets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
)

// WarningKind classifies a resolution warning.
//
// It is a closed enum rather than free text because warnings are printed,
// logged, and shown by `latem doctor`, and a free-text field is somewhere a
// value can end up. Nothing in this package can put a secret into a [Warning].
type WarningKind string

// WarnTooShort marks a secret that resolved shorter than
// [logstream.MinRedactableLength] and will therefore appear in logs verbatim
// (RUNNER_SPEC §16). Nothing else will tell the operator this.
const WarnTooShort WarningKind = "too_short"

// Warning is something an operator should know about a resolved secret.
type Warning struct {
	// Name is the secret's declared name.
	Name string
	// Kind is why it is being mentioned. There is no field for the value.
	Kind WarningKind
}

func (w Warning) String() string {
	if w.Kind == WarnTooShort {
		return fmt.Sprintf(
			"secret %s resolved to fewer than %d characters and will NOT be redacted from logs",
			w.Name, logstream.MinRedactableLength)
	}
	return fmt.Sprintf("secret %s: %s", w.Name, w.Kind)
}

// Set holds one run's resolved secrets.
//
// A Set can only be produced by [ResolveAll], and ResolveAll registers every
// value with the redactor before it returns one. INV-4's ordering obligation —
// registered before the first step starts, and before the value is used for
// anything else — is therefore a property of the type rather than a rule
// somebody has to remember: there is no way to be holding a resolved value
// that skipped registration, because there is no other way to be holding a
// resolved value.
//
// Values are []byte and are zeroed by [Set.Zero] when the run reaches a
// terminal state. Conversion to string happens exactly once, outside this
// package, when the orchestrator builds the executor's environment map — and
// that map is dropped with the run (§15, finding F10).
type Set struct {
	mu      sync.RWMutex
	names   []string
	values  map[string]Value
	sources map[string]string
	// files is the file name each name that resolved to a file goes by
	// (A88). Its bytes are in values, and are zeroed with them.
	files    map[string]string
	warnings []Warning
	zeroed   bool
}

// Names returns the resolved secret names in declaration order.
func (s *Set) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.names)
}

// Len returns how many secrets resolved.
func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.names)
}

// Value returns the live buffer for name.
//
// It is the buffer, not a copy: [Set.Zero] has to be able to reach every byte
// it handed out, and a copy is a byte it cannot reach. Callers must not retain
// it past Zero, and must not append to it.
func (s *Set) Value(name string) (Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[name]
	return v, ok
}

// FileName is the name of the file name resolved to, when it is a file (A88):
// a step gets the path of a copy by that name instead of the bytes.
func (s *Set) FileName(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	file, ok := s.files[name]
	return file, ok
}

// Source returns which provider answered for name — "env", "file", "keychain",
// "pass", "command" — or the empty string. Provenance is not secret and
// survives [Set.Zero].
func (s *Set) Source(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[name]
}

// Warnings returns everything an operator should be told about this set.
func (s *Set) Warnings() []Warning {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.warnings)
}

// Zeroed reports whether [Set.Zero] has been called.
func (s *Set) Zeroed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.zeroed
}

// Zero overwrites every value in place. Call it when the run reaches a
// terminal state; it is idempotent, so the deferred call and the explicit one
// on the error path can both stand.
//
// Names, provenance, and warnings deliberately survive. None of them is
// secret, and a post-mortem — "which provider did MATCH_PASSWORD come from on
// the run that failed?" — is exactly the question asked after the run is over.
func (s *Set) Zero() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.values {
		v.Zero()
	}
	clear(s.values)
	s.zeroed = true
}

// ResolveAll resolves every declared name through p and registers each value
// with reg before returning.
//
// Ordering, spelled out because it is INV-4: a value is handed to reg
// immediately after it is resolved and before it is stored anywhere this
// package will hand it back from. The window in which a resolved secret exists
// unregistered is one function call wide, and no [Set] observable by a caller
// ever contains an unregistered value.
//
// Every declared name must resolve to something non-empty. Both failure modes
// — no provider held it, and a provider held an empty value — are
// E_MISSING_SECRET, reported together rather than one per attempt, because an
// operator setting up a machine wants the list and not a guessing game. §15 is
// explicit that this is an admission-time answer: "not a runtime surprise 40
// minutes in".
//
// reg is required. A nil Registrar is an error rather than a permitted
// convenience, because the convenience is indistinguishable from the bug: a
// run whose secrets were never registered logs them in cleartext, and the
// invariant it breaks is the one that says secrets never leave the machine.
func ResolveAll(ctx context.Context, p Provider, reg logstream.Registrar, names []string) (*Set, error) {
	if p == nil {
		return nil, &Error{Names: slices.Clone(names), Msg: "no secret provider is configured"}
	}
	if reg == nil {
		return nil, &Error{
			Names: slices.Clone(names),
			Msg: "a logstream.Registrar is required: a value not registered before the first " +
				"step cannot be redacted from the log (INV-4)",
		}
	}

	set := &Set{values: make(map[string]Value), sources: make(map[string]string)}
	var notFound, empty []string
	seen := make(map[string]bool, len(names))

	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true

		if err := validName(name); err != nil {
			set.Zero()
			return nil, &Error{Names: []string{name}, Msg: "declared secret is not usable", Err: err}
		}

		value, source, err := resolveFrom(ctx, p, name)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotFound):
			notFound = append(notFound, name)
			continue
		default:
			set.Zero()
			// A cancelled run is not a missing secret. Returning the context
			// error unwrapped keeps CodeOf out of it, so the orchestrator
			// reports E_INTERRUPTED instead of sending an operator hunting for
			// a credential that was never the problem.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, &Error{Names: []string{name}, Msg: "resolving " + name, Err: err}
		}

		if len(value) == 0 {
			empty = append(empty, name)
			continue
		}

		// INV-4, and the reason this loop is not two loops: registration
		// precedes storage, so no code path can observe the value first.
		reg.Register(value)

		if logstream.TooShort(value) {
			set.warnings = append(set.warnings, Warning{Name: name, Kind: WarnTooShort})
		}

		set.names = append(set.names, name)
		set.values[name] = value
		set.sources[name] = source
	}

	if len(notFound)+len(empty) > 0 {
		set.Zero()
		return nil, missingError(notFound, empty)
	}
	return set, nil
}

func missingError(notFound, empty []string) *Error {
	var parts []string
	if len(notFound) > 0 {
		parts = append(parts, "no provider holds "+strings.Join(notFound, ", "))
	}
	if len(empty) > 0 {
		parts = append(parts, "resolved to an empty value: "+strings.Join(empty, ", "))
	}
	names := slices.Concat(notFound, empty)
	slices.Sort(names)
	return &Error{Names: names, Msg: strings.Join(parts, "; ")}
}

// resolveFrom asks p which provider answered when p can say.
func resolveFrom(ctx context.Context, p Provider, name string) (Value, string, error) {
	if s, ok := p.(Sourced); ok {
		return s.ResolveFrom(ctx, name)
	}
	v, err := p.Resolve(ctx, name)
	return v, p.Name(), err
}

// Status is what `latem doctor --secrets` reports for one declared name.
//
// Every field is safe to print. There is no field capable of holding a value,
// and Problem is built from this package's own errors, which are value-free by
// construction — a failed program's stderr is reachable only through
// [CommandError.Diagnostic] and is deliberately not here.
type Status struct {
	// Name is the declared secret name.
	Name string
	// Provider is which provider answered, or "" if none did.
	Provider string
	// Found reports whether a non-empty value was produced.
	Found bool
	// Short reports whether the value is below [logstream.MinRedactableLength]
	// and will therefore appear in logs verbatim.
	Short bool
	// Problem explains a failure, value-free. Empty when Found.
	Problem string
}

// Diagnose resolves names for reporting only and zeroes every value before it
// returns.
//
// It exists so `latem doctor --secrets` can answer "will this machine be able
// to run that job?" without running it, and without a Registrar: nothing here
// is used, logged, or passed to a step, so the INV-4 ordering obligation that
// [ResolveAll] enforces has nothing to attach to. That asymmetry is the reason
// they are two functions rather than one with a flag.
func Diagnose(ctx context.Context, p Provider, names []string) []Status {
	out := make([]Status, 0, len(names))
	for _, name := range names {
		st := Status{Name: name}

		if err := validName(name); err != nil {
			st.Problem = err.Error()
			out = append(out, st)
			continue
		}
		if p == nil {
			st.Problem = "no secret provider is configured"
			out = append(out, st)
			continue
		}

		value, source, err := resolveFrom(ctx, p, name)
		switch {
		case errors.Is(err, ErrNotFound):
			st.Problem = "no provider holds this secret"
		case err != nil:
			st.Problem = err.Error()
		case len(value) == 0:
			st.Provider = source
			st.Problem = "resolved to an empty value"
		default:
			st.Found = true
			st.Provider = source
			st.Short = logstream.TooShort(value)
		}
		value.Zero()

		out = append(out, st)
	}
	return out
}
