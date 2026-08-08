package secrets

import (
	"context"
	"fmt"
	"strings"
)

// EnvPrefix is the prefix the environment provider reads under (§15).
const EnvPrefix = "LATEM_SECRET_"

// EnvProvider resolves LATEM_SECRET_<NAME> from the agent's own environment.
//
// It is first in the chain because it is the override: a launchd plist, a
// systemd unit, a CI harness, or an operator debugging a signing failure can
// supply one value without touching any store, and it wins. That ordering is
// also what makes the rest of the chain testable in situ — `LATEM_SECRET_X=…
// latem run` answers "is it the secret or is it the build?" in one command.
//
// One honest limitation, stated here because the package's whole discipline is
// []byte-not-string (finding F10): a process environment is strings by
// definition, and Go hands it to us as []string. The value already exists as
// an immutable string in the process image before this package sees it, and
// nothing here can zero that. What this provider can do — and does — is copy
// into []byte at the first opportunity and never create another string from
// it. The unzeroable copy is the operating system's, not ours.
type EnvProvider struct {
	values map[string][]byte
}

var _ Provider = (*EnvProvider)(nil)

// NewEnvProvider snapshots environ, which is os.Environ's KEY=VALUE form.
//
// The environment is captured once rather than read per call so that a run
// resolves against the environment the agent started with. An agent that
// picked up a value some later SIGHUP-adjacent code path had mutated would
// make two runs of the same commit differ for reasons nothing records.
func NewEnvProvider(environ []string) *EnvProvider {
	p := &EnvProvider{values: make(map[string][]byte)}
	for _, kv := range environ {
		key, value, found := strings.Cut(kv, "=")
		if !found || !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, EnvPrefix)
		if validName(name) != nil {
			continue
		}
		p.values[name] = []byte(value)
	}
	return p
}

// Resolve returns a copy of LATEM_SECRET_<name>.
//
// A variable that is set but empty is a hit, not a miss. Falling through would
// mean an operator who exported LATEM_SECRET_MATCH_PASSWORD= to disable a
// keychain entry silently got the keychain entry anyway; instead the empty
// value reaches [ResolveAll] and becomes E_MISSING_SECRET naming the secret,
// which is the answer they were owed.
func (p *EnvProvider) Resolve(_ context.Context, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	v, ok := p.values[name]
	if !ok {
		return nil, fmt.Errorf("env: %s%s is not set: %w", EnvPrefix, name, ErrNotFound)
	}
	// Copied because the caller owns what it gets and will zero it; the
	// snapshot has to survive a second resolution of the same name.
	return append(Value(nil), v...), nil
}

// Name identifies this provider in diagnostics.
func (p *EnvProvider) Name() string { return "env" }
