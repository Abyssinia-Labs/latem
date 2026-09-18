package secrets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// CommandProvider runs an operator-defined program per secret — the last link
// in the §15 chain, configured as:
//
//	[secrets.commands]
//	MATCH_PASSWORD = "op read op://Private/match/password"
//
// A name with no configured command is [ErrNotFound]. A name with one whose
// program fails is not: the operator said explicitly where this secret comes
// from, so falling through to "no provider held it" would throw away the only
// diagnosis anyone has — that `op` was not signed in.
type CommandProvider struct {
	commands map[string][]string
	runner   Runner
}

var _ Provider = (*CommandProvider)(nil)

// NewCommandProvider parses every configured command up front.
//
// Up front is the point. A command string that cannot be turned into an argv
// is a config error, and RUNNER_SPEC §8 is unambiguous that config errors are
// fatal at load rather than discovered later, "in a build, where it is
// expensive to diagnose".
func NewCommandProvider(commands map[string]string, runner Runner) (*CommandProvider, error) {
	p := &CommandProvider{commands: make(map[string][]string, len(commands)), runner: runner}
	if p.runner == nil {
		p.runner = ExecRunner{}
	}

	// Sorted so a config with two broken entries always reports the same one
	// first; map order otherwise makes the error message flap between runs.
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		if err := validName(name); err != nil {
			return nil, fmt.Errorf("secrets: [secrets.commands]: %w", err)
		}
		argv, err := ParseCommand(commands[name])
		if err != nil {
			return nil, fmt.Errorf("secrets: [secrets.commands] %s: %w", name, err)
		}
		p.commands[name] = argv
	}
	return p, nil
}

// Name identifies this provider in diagnostics.
func (p *CommandProvider) Name() string { return "command" }

// Configured returns the secret names this provider can answer for, sorted.
func (p *CommandProvider) Configured() []string {
	names := make([]string, 0, len(p.commands))
	for name := range p.commands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Resolve runs the command configured for name.
func (p *CommandProvider) Resolve(ctx context.Context, name string) (Value, error) {
	if err := validName(name); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	argv, ok := p.commands[name]
	if !ok {
		return nil, fmt.Errorf("command: no [secrets.commands] entry for %s: %w", name, ErrNotFound)
	}

	// Note what is missing: no ErrNotFound wrapping, in any branch. A missing
	// binary is ErrNotFound for the keychain because `security` being absent
	// means "this is not a Mac"; here it means the operator named a program
	// that is not installed, which is a configuration they have to fix and not
	// a provider to skip past.
	out, _, err := p.runner.Run(ctx, argv)
	if err != nil {
		return nil, fmt.Errorf("command: %s: %w", name, err)
	}
	return trimCommandOutput(out), nil
}

// shellMeta are the characters a shell always interprets. They are rejected
// rather than passed through; see [ParseCommand].
const shellMeta = "|&;<>()$`\n\r"

// ParseCommand splits a configured command string into an argv slice.
//
// A [secrets.commands] entry is one string, and something has to turn it into
// a process. The obvious something is `sh -c`, and INV-3 forbids it: the agent
// never hands a constructed string to a shell, because the moment a shell is
// in the picture the safety of every value that reaches it becomes a question
// to be re-answered rather than an invariant. So the string is tokenised here,
// with shell *quoting* rules and no shell *semantics*: whitespace separates
// words, '…' is literal, "…" groups, and a backslash escapes one character.
//
// Anything a shell would interpret is rejected rather than passed through, and
// that half is the one that earns the function. Handing a program `$HOME` as
// five literal characters is not graceful degradation; it is the command
// quietly doing something other than what the operator wrote, discovered when
// a signing step fails at the end of a forty-minute build. Failing at config
// load — "latem does not run a shell; write the expanded form" — costs one
// line of config and no incidents. The same argument covers a leading `~`,
// which is why it is refused too.
func ParseCommand(s string) ([]string, error) {
	var (
		argv []string
		cur  strings.Builder
		word bool // a word is in progress, possibly an empty quoted one
	)

	flush := func() {
		if word {
			argv = append(argv, cur.String())
			cur.Reset()
			word = false
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			flush()

		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			word = true

		case c == '"':
			i++
			closed := false
			for ; i < len(s); i++ {
				if s[i] == '"' {
					closed = true
					break
				}
				if s[i] == '\\' {
					i++
					if i >= len(s) {
						return nil, errors.New("unterminated double quote")
					}
					cur.WriteByte(s[i])
					continue
				}
				// A shell expands these inside double quotes; this does not.
				if s[i] == '$' || s[i] == '`' {
					return nil, fmt.Errorf("%q inside double quotes needs a shell, "+
						"and latem does not run one: write the expanded value, "+
						"or escape it as \\%c to pass it literally", s[i], s[i])
				}
				cur.WriteByte(s[i])
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
			word = true

		case c == '\\':
			i++
			if i >= len(s) {
				return nil, errors.New("trailing backslash")
			}
			cur.WriteByte(s[i])
			word = true

		case strings.IndexByte(shellMeta, c) >= 0:
			return nil, fmt.Errorf("%q needs a shell, and latem does not run one (INV-3): "+
				"quote it to pass it literally, or move the logic into a script", c)

		case c == '~' && !word:
			return nil, errors.New("a leading ~ is expanded by a shell, and latem does not " +
				"run one: write the absolute path")

		default:
			cur.WriteByte(c)
			word = true
		}
	}
	flush()

	if len(argv) == 0 {
		return nil, errors.New("command is empty")
	}
	if strings.HasPrefix(argv[0], "-") {
		return nil, fmt.Errorf("command starts with %q, which is an option rather than a program", argv[0])
	}
	return argv, nil
}
