package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"regexp"
)

// KeychainService is the service every latem keychain item is filed under, so
// that `security add-generic-password -s latem -a <NAME> -w …` is the documented
// way in (§15). The way out is -g rather than -w; see [KeychainProvider.Resolve]
// for why.
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

	// -g, not -w, and the value comes back on stderr.
	//
	// `security -w` prints a value containing any byte outside printable ASCII
	// — a newline above all — as bare hex digits, with nothing to say it did
	// so. That is unrecoverable: a secret whose literal value is "deadbeef" and
	// one whose value is the four bytes DE AD BE EF print identically, so no
	// amount of inspecting -w's output can tell them apart. Reading a PEM back
	// through -w yields "2d2d2d2d2d424547494e…", which resolves, passes
	// `doctor`, and reaches the build as a key that is not a key.
	//
	// -g prints the same value to stderr in a form that does say: `0x<HEX>`
	// followed by an escaped rendering when the bytes needed encoding, and a
	// plain quoted string when they did not. That prefix is the whole reason
	// this provider pays for the noisier flag.
	argv := []string{"security", "find-generic-password", "-s", p.service, "-a", name, "-g"}
	_, stderr, err := p.runner.Run(ctx, argv)
	if err != nil {
		if notInstalled(err) {
			return nil, fmt.Errorf("keychain: %w: %w", ErrNotFound, err)
		}
		if exitCodeOf(err) == keychainNotFound {
			return nil, fmt.Errorf("keychain: no item for %s: %w: %w", name, ErrNotFound, err)
		}
		return nil, fmt.Errorf("keychain: %w", err)
	}
	v, err := parseSecurityPassword(stderr)
	if err != nil {
		return nil, fmt.Errorf("keychain: %s: %w", name, err)
	}
	return v, nil
}

// passwordPrefix is what `security -g` puts before the value on stderr.
var passwordPrefix = []byte("password: ")

// parseSecurityPassword extracts the value from `security -g` stderr.
//
// Two forms, and the difference is load-bearing:
//
//	password: "hunter2"
//	password: 0x2D2D0A2D2D  "--\012--"
//
// The 0x form is authoritative — those are the exact bytes, and the quoted
// rendering beside it is lossy decoration. The bare quoted form appears only
// when every byte was printable ASCII and none was a backslash, which is what
// makes stripping one quote from each end exact rather than a guess: a value
// containing a `"` is printed unescaped (`password: "he said "hi""`), so
// unescaping would be wrong, while a value containing a `\` is pushed into the
// 0x form and never reaches this branch at all.
func parseSecurityPassword(stderr []byte) (Value, error) {
	for _, line := range bytes.Split(stderr, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, passwordPrefix) {
			continue
		}
		rest := line[len(passwordPrefix):]

		if hexDigits, ok := bytes.CutPrefix(rest, []byte("0x")); ok {
			// The hex runs to the first space; what follows is the escaped
			// rendering, which is discarded.
			if i := bytes.IndexByte(hexDigits, ' '); i >= 0 {
				hexDigits = hexDigits[:i]
			}
			decoded := make([]byte, hex.DecodedLen(len(hexDigits)))
			n, err := hex.Decode(decoded, hexDigits)
			if err != nil {
				return nil, fmt.Errorf("security printed a 0x value that is not hex: %w", err)
			}
			return Value(decoded[:n]), nil
		}

		if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
			return Value(rest[1 : len(rest)-1]), nil
		}
		return nil, fmt.Errorf("security printed a password line in no recognised form")
	}
	return nil, fmt.Errorf("security printed no password line")
}

// trimCommandOutput removes the single trailing newline every one of these
// tools appends.
//
// Exactly one, never all of them: `pass` and a configured command print the
// value and then a newline, so the last newline is the tool's and any before it
// are the secret's. Trimming greedily would silently corrupt a PEM, which ends
// with a newline that OpenSSL cares about.
//
// The keychain provider does not use this. `security -g` delimits the value
// itself, so there is no trailing newline of the tool's to remove.
func trimCommandOutput(out []byte) Value {
	out = bytes.TrimSuffix(out, []byte("\n"))
	out = bytes.TrimSuffix(out, []byte("\r"))
	return Value(out)
}
