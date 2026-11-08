package secrets

import (
	"context"
	"errors"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
)

// ScopeSeparator joins a scope's parts to a secret's name: a secret NAME for
// the repository app in its production environment is looked up as
// app__production__NAME (A50). Two underscores rather than a slash, because
// the name has to be a valid environment variable and a valid secrets.env key
// as well as a Keychain account.
const ScopeSeparator = "__"

// ScopedName is name under the given scope parts, each reduced to the
// characters a secret name may hold. With no parts it is name itself.
func ScopedName(name string, parts ...string) string {
	if len(parts) == 0 {
		return name
	}
	var b strings.Builder
	for _, p := range parts {
		for _, r := range p {
			switch {
			case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteRune('_')
			}
		}
		b.WriteString(ScopeSeparator)
	}
	b.WriteString(name)
	return b.String()
}

// ResolveScoped resolves names the way a workflow reads secrets (A50): each
// name is looked up under every scope in order, most specific first, and the
// first that answers wins. The value is kept under the bare name, which is
// what `${{ secrets.NAME }}` reads, and its source records the name it was
// found as.
//
// A name found under no scope, or only as an empty value, is returned in
// missing rather than failing the run: GitHub gives an unset secret as the
// empty string, and a workflow that tests whether `secrets.X` is empty depends
// on that. A backend that cannot answer still fails, as in [ResolveAll],
// because "the keychain is locked" is not "the secret is unset".
//
// Every value is registered with reg before it is stored (INV-4).
func ResolveScoped(
	ctx context.Context, p Provider, reg logstream.Registrar, names []string, scopes [][]string,
) (set *Set, missing []string, err error) {
	return ResolveAs(ctx, p, reg, names, func(name string) []string {
		out := make([]string, 0, len(scopes))
		for _, scope := range scopes {
			out = append(out, ScopedName(name, scope...))
		}
		return out
	})
}

// ResolveAs is [ResolveScoped] with each name's candidates given rather than
// made from scopes: name is looked up as each of lookup(name), in order, and
// kept under name. A called workflow's job reads a secret as whatever its
// caller passed it as (A81), so its candidates are not its own name's.
func ResolveAs(
	ctx context.Context, p Provider, reg logstream.Registrar, names []string, lookup func(name string) []string,
) (set *Set, missing []string, err error) {
	set = &Set{values: make(map[string]Value), sources: make(map[string]string)}
	if len(names) == 0 {
		return set, nil, nil
	}
	if reg == nil {
		return nil, nil, &Error{
			Names: append([]string(nil), names...),
			Msg: "a logstream.Registrar is required: a value not registered before the first " +
				"step cannot be redacted from the log (INV-4)",
		}
	}
	if p == nil {
		return set, append([]string(nil), names...), nil
	}

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if err := validName(name); err != nil {
			set.Zero()
			return nil, nil, &Error{Names: []string{name}, Msg: "secret name is not usable", Err: err}
		}

		found := false
		for _, candidate := range lookup(name) {
			if validName(candidate) != nil {
				// A scope whose parts cannot make a usable name (a repository
				// key starting with a digit) is skipped, not fatal: the next,
				// wider scope may still answer.
				continue
			}
			value, source, rerr := resolveFrom(ctx, p, candidate)
			switch {
			case rerr == nil:
			case errors.Is(rerr, ErrNotFound):
				continue
			default:
				set.Zero()
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, nil, ctxErr
				}
				return nil, nil, &Error{Names: []string{name}, Msg: "resolving " + candidate, Err: rerr}
			}
			if len(value) == 0 {
				continue
			}

			reg.Register(value)
			if logstream.TooShort(value) {
				set.warnings = append(set.warnings, Warning{Name: name, Kind: WarnTooShort})
			}
			set.names = append(set.names, name)
			set.values[name] = value
			if candidate != name {
				source += " as " + candidate
			}
			set.sources[name] = source
			found = true
			break
		}
		if !found {
			missing = append(missing, name)
		}
	}
	return set, missing, nil
}
