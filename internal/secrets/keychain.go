package secrets

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
)

// KeychainService is the service every latem keychain item is filed under, so
// that `security find-generic-password -s latem -a <NAME> -w` is the documented
// way in and out (§15).
const KeychainService = "latem"

// keychainNotFound is what `security` exits with for errSecItemNotFound. It is
// the difference between "this Mac has no such item", which is the chain
// working correctly, and "the keychain is locked", which is an operator
// problem the chain must not paper over by falling through.
const keychainNotFound = 44

// servicePattern bounds the configured service name. It becomes an argv
// element, so the leading character class matters more than the rest: a
// service name starting with `-` would be read by `security` as a flag.
var servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// KeychainProvider resolves names from the macOS Keychain.
//
// It holds no macOS-specific code and no build tag. Everything platform-shaped
// about it is in the argv, which means the provider is exercised on Linux CI
// with a fake [Runner] rather than being the one link in the chain that only
// gets tested by hand on somebody's laptop. [New] is what decides the provider
// belongs in the chain at all, from runtime.GOOS.
type KeychainProvider struct {
	service string
	runner  Runner
}

var _ Provider = (*KeychainProvider)(nil)

// NewKeychainProvider returns a provider reading items filed under service.
// An empty service uses [KeychainService].
func NewKeychainProvider(service string, runner Runner) (*KeychainProvider, error) {
	if service == "" {
		service = KeychainService
	}
	if !servicePattern.MatchString(service) {
		return nil, fmt.Errorf("secrets: keychain service %q must match %s", service, servicePattern)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &KeychainProvider{service: service, runner: runner}, nil
}

// Name identifies this provider in diagnostics.
func (p *KeychainProvider) Name() string { return "keychain" }

// Resolve shells out to `security`, without a shell.
//
// Three outcomes are not failures and fall through to the next provider: the
// item is absent (exit 44), `security` is not installed (this is not a Mac),
// and the name is not one this provider could hold. Everything else — a locked
// keychain most of all — stops the chain, because a Mac whose keychain will
// not unlock should say so at admission rather than quietly resolve the secret
// from somewhere the operator did not intend.
func (p *KeychainProvider) Resolve(ctx context.Context, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("keychain: %w", err)
	}

	argv := []string{"security", "find-generic-password", "-s", p.service, "-a", name, "-w"}
	out, err := p.runner.Run(ctx, argv)
	if err != nil {
		if notInstalled(err) {
			return nil, fmt.Errorf("keychain: %w: %w", ErrNotFound, err)
		}
		if exitCodeOf(err) == keychainNotFound {
			return nil, fmt.Errorf("keychain: no item for %s: %w: %w", name, ErrNotFound, err)
		}
		return nil, fmt.Errorf("keychain: %w", err)
	}
	return trimCommandOutput(out), nil
}

// trimCommandOutput removes the single trailing newline every one of these
// tools appends.
//
// Exactly one, never all of them: `security -w` prints the password and then a
// newline, so the last newline is the tool's and any before it are the
// secret's. Trimming greedily would silently corrupt a PEM, which ends with a
// newline that OpenSSL cares about.
func trimCommandOutput(out []byte) Value {
	out = bytes.TrimSuffix(out, []byte("\n"))
	out = bytes.TrimSuffix(out, []byte("\r"))
	return Value(out)
}
