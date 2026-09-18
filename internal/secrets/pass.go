package secrets

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// DefaultPassPrefix namespaces latem's entries inside a password store that
// almost certainly holds the operator's personal passwords too.
const DefaultPassPrefix = "latem/"

// passPrefixPattern bounds the configured store prefix. It is joined to a
// secret name and passed as one argv element, so it must not begin with `-`
// and must not contain `..` — the second because a pass store is a directory
// tree of GPG files and `../` in an entry path escapes it.
var passPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// PassOptions configures the pass/gopass provider.
type PassOptions struct {
	// Binary is "pass" or "gopass". Empty disables the provider entirely,
	// which is the default: most machines do not have a password store, and a
	// provider that is not configured should not be in the chain producing a
	// failed exec per secret per run.
	Binary string
	// Prefix is joined to the secret name to form the store path. Empty uses
	// [DefaultPassPrefix]; a single "/" means the store root.
	Prefix string
}

// PassProvider resolves names from a `pass` or `gopass` store.
//
// It reads the WHOLE entry, not its first line. The `pass` convention is that
// line one is the password and the rest is metadata, and that convention
// cannot represent APP_STORE_CONNECT_KEY_P8 — a PEM block is nine lines and
// all nine are the secret. Storing a latem secret in `pass` therefore means
// storing exactly the secret and nothing else in the entry, which is stated
// here because the alternative reading loses eight lines of a signing key at
// the point where the build fails and nobody suspects the password manager.
type PassProvider struct {
	binary string
	prefix string
	runner Runner
}

var _ Provider = (*PassProvider)(nil)

// NewPassProvider returns a provider backed by opts.Binary.
func NewPassProvider(opts PassOptions, runner Runner) (*PassProvider, error) {
	switch opts.Binary {
	case "pass", "gopass":
	default:
		return nil, fmt.Errorf("secrets: pass binary %q must be \"pass\" or \"gopass\"", opts.Binary)
	}

	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPassPrefix
	}
	if prefix == "/" {
		prefix = ""
	}
	if prefix != "" {
		if !passPrefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") {
			return nil, fmt.Errorf("secrets: pass prefix %q must match %s and must not contain \"..\"",
				prefix, passPrefixPattern)
		}
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
	}

	if runner == nil {
		runner = ExecRunner{}
	}
	return &PassProvider{binary: opts.Binary, prefix: prefix, runner: runner}, nil
}

// Name identifies this provider in diagnostics.
func (p *PassProvider) Name() string { return "pass" }

// Resolve runs `<binary> show <prefix><name>`.
//
// Any non-zero exit is treated as "not in the store" and falls through, which
// is a concession to the tools rather than a design choice: `pass` exits 1 for
// a missing entry, for a locked GPG agent, and for a corrupt store alike, and
// the message distinguishing them is prose that differs between pass and
// gopass and between their versions. Parsing it would be a guess. The
// underlying [CommandError] is kept in the chain so `latem doctor` can still
// print what the tool actually said.
func (p *PassProvider) Resolve(ctx context.Context, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("pass: %w", err)
	}

	argv := []string{p.binary, "show", p.prefix + name}
	out, _, err := p.runner.Run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("pass: %s is not readable: %w: %w", p.prefix+name, ErrNotFound, err)
	}
	return trimCommandOutput(out), nil
}
