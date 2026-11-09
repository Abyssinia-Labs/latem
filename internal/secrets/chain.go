package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"
)

// Toggle is a tri-state option.
//
// It exists because the sensible default for the Keychain provider is "yes on
// macOS", and a bool's zero value cannot say that — Options{} would silently
// mean "no Keychain" on the one platform the whole mobile pipeline runs on.
type Toggle uint8

// Toggle values. Auto is the zero value and follows the platform.
const (
	Auto Toggle = iota
	On
	Off
)

func (t Toggle) enabled(platformDefault bool) bool {
	switch t {
	case On:
		return true
	case Off:
		return false
	}
	return platformDefault
}

// Sourced is implemented by a Provider that can say which underlying provider
// answered.
//
// [Chain] implements it. It exists for `latem doctor --secrets`, which reports
// where each declared name came from — provider names and secret names, never
// values (§15) — and for the run row, so an operator debugging a stale
// credential can see that MATCH_PASSWORD came from the environment and not
// from the keychain entry they just rotated.
type Sourced interface {
	Provider
	ResolveFrom(ctx context.Context, name string) (value Value, provider string, err error)
}

// ScopedProvider is a Provider that keeps each scope's secrets apart, and so
// is asked with the scope's parts as they are rather than flattened into one
// name (A86).
//
// The flattening is [ScopedName], and it is lossy: latem-cloud and
// latem_cloud both become latem_cloud. That costs nothing in a store keyed by
// the flat name — the Keychain, secrets.env — and makes a store keyed by the
// repository itself unreachable, which is what OpenVault is: one collection
// per repository, named as the repository is. Resolve is the machine-wide
// name, ResolveIn with no scope.
type ScopedProvider interface {
	Provider
	ResolveIn(ctx context.Context, scope []string, name string) (Value, error)
}

// locator is a ScopedProvider that can say where it found a scoped name, for
// a run's provenance: "openvault as latem-cloud/production/TOKEN" names the
// entry an operator would go and rotate.
type locator interface {
	Where(scope []string, name string) string
}

// Chain resolves a name through its providers in order, first hit wins (§15).
//
// Two rules define it, and both are about failing loudly rather than
// plausibly:
//
// A provider that reports [ErrNotFound] is skipped, and nothing else is. Any
// other error stops the chain and is returned. The tempting alternative — try
// the next one, it might work — turns "secrets.env is world-readable" and "the
// keychain is locked" into "secret not found", which is the same message an
// operator gets for a typo, and sends them to the wrong half of the problem.
//
// A provider that returns an empty value has answered. It does not fall
// through. Exporting LATEM_SECRET_X= to override a stale keychain entry has to
// mean the override took effect, otherwise the stale entry wins and the
// operator has no way to see it. The empty value reaches [ResolveAll] and
// becomes E_MISSING_SECRET naming X, at admission.
type Chain struct {
	providers []Provider
}

var (
	_ Provider       = (*Chain)(nil)
	_ Sourced        = (*Chain)(nil)
	_ ScopedProvider = (*Chain)(nil)
)

// NewChain returns a chain over providers, in order. Nil providers are
// dropped, so a caller can build the slice with conditional entries.
func NewChain(providers ...Provider) *Chain {
	c := &Chain{providers: make([]Provider, 0, len(providers))}
	for _, p := range providers {
		if p != nil {
			c.providers = append(c.providers, p)
		}
	}
	return c
}

// Name identifies this provider in diagnostics.
func (c *Chain) Name() string { return "chain" }

// Providers returns the provider names in resolution order.
func (c *Chain) Providers() []string {
	out := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		out = append(out, p.Name())
	}
	return out
}

// Resolve returns the first provider's answer.
func (c *Chain) Resolve(ctx context.Context, name string) (Value, error) {
	v, _, err := c.ResolveFrom(ctx, name)
	return v, err
}

// ResolveFrom is [Chain.Resolve] plus the name of the provider that answered.
func (c *Chain) ResolveFrom(ctx context.Context, name string) (Value, string, error) {
	return c.ResolveInFrom(ctx, nil, name)
}

// ResolveIn returns name under scope's parts, a repository and then its
// environment, from the first provider that holds it.
func (c *Chain) ResolveIn(ctx context.Context, scope []string, name string) (Value, error) {
	v, _, err := c.ResolveInFrom(ctx, scope, name)
	return v, err
}

// ResolveInFrom is [Chain.ResolveIn] plus where the value was found.
//
// A [ScopedProvider] is asked with the scope's parts; every other provider
// with [ScopedName]'s flat name. A scope that flattens into no usable name — a
// repository key starting with a digit — is skipped by the flat providers
// alone, since the scoped ones may still hold it.
func (c *Chain) ResolveInFrom(ctx context.Context, scope []string, name string) (Value, string, error) {
	if err := validName(name); err != nil {
		return nil, "", err
	}
	flat := ScopedName(name, scope...)
	flatErr := validName(flat)
	if len(c.providers) == 0 {
		return nil, "", fmt.Errorf("secrets: no providers configured: %w", ErrNotFound)
	}

	var misses error
	for _, p := range c.providers {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		var (
			v   Value
			err error
		)
		sp, scoped := p.(ScopedProvider)
		switch {
		case scoped:
			v, err = sp.ResolveIn(ctx, scope, name)
		case flatErr != nil:
			err = fmt.Errorf("%s: %w: %w", p.Name(), flatErr, ErrNotFound)
		default:
			v, err = p.Resolve(ctx, flat)
		}
		switch {
		case err == nil:
			source := p.Name()
			if l, ok := p.(locator); ok && len(scope) > 0 {
				if where := l.Where(scope, name); where != "" {
					source += " as " + where
				}
			}
			return v, source, nil
		case errors.Is(err, ErrNotFound):
			// Accumulated rather than discarded: when nothing holds the name,
			// "keychain: no item; pass: not readable: gpg exited 2" is the
			// difference between a typo and a broken GPG agent, and neither is
			// visible from "not found" alone.
			misses = errors.Join(misses, err)
		default:
			return nil, p.Name(), err
		}
	}
	return nil, "", fmt.Errorf("secrets: no provider holds %s: %w (%w)", flat, ErrNotFound, misses)
}

// Options configures the standard §15 chain.
type Options struct {
	// Environ is the agent's environment in os.Environ's KEY=VALUE form. Nil
	// uses the agent's own.
	Environ []string
	// FilePath is the secrets.env to read, conventionally
	// ~/.latem/secrets.env. Empty disables the file provider.
	FilePath string
	// Keychain enables the macOS Keychain provider. Auto — the zero value —
	// enables it on darwin only.
	Keychain Toggle
	// KeychainService overrides the service items are filed under. Empty uses
	// [KeychainService].
	KeychainService string
	// Pass configures pass/gopass. A zero PassOptions disables it.
	Pass PassOptions
	// Commands is the [secrets.commands] table: secret name to command string.
	Commands map[string]string
	// OpenVault puts OpenVault in the chain, read from the service at
	// [OpenVaultSocket] of Environ. Off is the default: a machine reads its
	// secrets from OpenVault because its config says so (`[secrets]
	// openvault = true`), never because a vault happens to be there — a vault
	// whose service is not running would otherwise fail every job on it.
	OpenVault bool
	// Runner executes external programs. Nil uses [ExecRunner]; a test
	// substitutes a fake and the chain never touches a real credential store.
	Runner Runner
	// Timeout bounds one external command. Zero uses [DefaultTimeout]. It is
	// ignored when Runner is set, because a caller supplying a Runner owns its
	// behaviour.
	Timeout time.Duration
}

// New assembles the resolution chain of RUNNER_SPEC §15, in order: environment,
// OpenVault when configured, secrets.env, macOS Keychain, pass/gopass,
// per-secret command.
//
// OpenVault comes straight after the environment (A86). It is the one copy
// the machines share, kept in step by OpenVault's sync; a value in this
// machine's secrets.env or Keychain is a copy somebody made, and copies are
// what go stale. Read after them, a rotated token in the vault would lose to
// the old one left in the file, and nothing would say so. The environment
// stays the override, as it is for every store.
//
// It returns an error rather than skipping a misconfigured provider. A machine
// whose [secrets.commands] entry does not parse has a config bug, and starting
// anyway means the bug surfaces as a missing secret in whichever build first
// declares that name — days later, attributed to the wrong thing.
func New(opts Options) (*Chain, error) {
	runner := opts.Runner
	if runner == nil {
		runner = ExecRunner{Timeout: opts.Timeout}
	}

	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}

	providers := []Provider{NewEnvProvider(environ)}

	if opts.OpenVault {
		socket, err := OpenVaultSocket(environ)
		if err != nil {
			return nil, err
		}
		providers = append(providers, NewOpenVaultProvider(socket, opts.Timeout))
	}

	if opts.FilePath != "" {
		providers = append(providers, NewFileProvider(opts.FilePath))
	}

	if opts.Keychain.enabled(runtime.GOOS == "darwin") {
		kc, err := NewKeychainProvider(opts.KeychainService, runner)
		if err != nil {
			return nil, err
		}
		providers = append(providers, kc)
	}

	if opts.Pass.Binary != "" {
		pp, err := NewPassProvider(opts.Pass, runner)
		if err != nil {
			return nil, err
		}
		providers = append(providers, pp)
	}

	if len(opts.Commands) > 0 {
		cp, err := NewCommandProvider(opts.Commands, runner)
		if err != nil {
			return nil, err
		}
		providers = append(providers, cp)
	}

	return NewChain(providers...), nil
}
