// Package ghcmd parses the two side channels a GitHub Actions step uses to
// talk back to its runner: workflow commands written to stdout, and the
// key/value files named by $GITHUB_ENV, $GITHUB_OUTPUT, $GITHUB_PATH and
// $GITHUB_STEP_SUMMARY.
//
// A42 phase 2. Nothing here moves the trust boundary: both channels are
// already inside it, written by a step that is already running the
// repository's own commands. What they do change is what the agent *believes*
// afterwards, which is why every value read here is bounded, every key is
// validated, and nothing is ever dropped in silence — a job author whose
// `echo` did not take effect is told why, in the log of the step that wrote it.
//
// The one command with a security job is `::add-mask::`. It hands a value to
// the redactor, which is INV-4's mechanism, so a token a step discovers at run
// time is redacted from that point on by the same code path as a declared
// secret. Redaction still happens at the point of emission; this only adds to
// what is registered.
package ghcmd

import (
	"strings"
)

// Command is one `::name key=value::data` line.
type Command struct {
	// Name is lowercased: GitHub matches these case-insensitively.
	Name string
	// Params are the comma-separated key=value pairs before the second `::`.
	Params map[string]string
	// Value is the text after the second `::`, unescaped.
	Value string
}

// Param reads a parameter, answering "" when it is absent.
func (c Command) Param(name string) string { return c.Params[name] }

// Known reports whether this is a command the runner acts on.
//
// [ParseLine] answers the grammar question and this one answers the semantic
// one, and they are separate because a consumer that removes command lines
// from the log must only remove the ones it understood. `::std::vector<int>`
// in a compiler diagnostic is well-formed by the grammar — name `std`, value
// `vector<int>` — and a consumer that suppressed it on that basis would delete
// the error the developer is trying to read. Anything not on this list is
// output that happens to look like a command, and stays output.
func (c Command) Known() bool { return knownCommands[c.Name] }

// knownCommands is GitHub's set. `set-output` and `save-state` are deprecated
// in favour of the $GITHUB_OUTPUT file and are listed because workflows in the
// wild still emit them: recognising one is what lets the caller say so, where
// dropping it silently would present as an output that never arrived.
var knownCommands = map[string]bool{
	"add-mask":      true,
	"group":         true,
	"endgroup":      true,
	"debug":         true,
	"notice":        true,
	"warning":       true,
	"error":         true,
	"add-path":      true, // deprecated, superseded by $GITHUB_PATH
	"set-env":       true, // deprecated, superseded by $GITHUB_ENV
	"set-output":    true, // deprecated, superseded by $GITHUB_OUTPUT
	"save-state":    true,
	"echo":          true,
	"stop-commands": true,
}

// Deprecated reports whether a known command has been superseded by a file.
func (c Command) Deprecated() bool { return deprecatedCommands[c.Name] }

var deprecatedCommands = map[string]bool{
	"add-path": true, "set-env": true, "set-output": true, "save-state": true,
}

// maxCommandBytes bounds one line considered as a command.
//
// A step's stdout is job-authored and arbitrarily long. A line past this is
// log output that happens to start with `::`, not a command anyone wrote, and
// treating it as one means unescaping and storing it.
const maxCommandBytes = 32 << 10

// ParseLine reads one line of step output as a workflow command.
//
// The bool is false for ordinary output, which is almost every line, so the
// cheap prefix test comes first.
//
// The grammar is GitHub's: `::name::`, `::name::data`, or
// `::name k=v,k2=v2::data`. An unterminated command — `::foo` with no closing
// `::` — is not a command; it is a line of output that begins with two colons,
// and C++ scope resolution in a compiler's diagnostics is the common case.
func ParseLine(line string) (Command, bool) {
	s := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if !strings.HasPrefix(s, "::") || len(s) > maxCommandBytes {
		return Command{}, false
	}
	rest := s[2:]

	end := strings.Index(rest, "::")
	if end < 0 {
		return Command{}, false
	}
	head, data := rest[:end], rest[end+2:]

	name, paramText, _ := strings.Cut(head, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || !isCommandName(name) {
		return Command{}, false
	}

	cmd := Command{Name: name, Params: map[string]string{}, Value: unescapeData(data)}
	for _, pair := range strings.Split(paramText, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		cmd.Params[strings.ToLower(k)] = unescapeProperty(v)
	}
	return cmd, true
}

// isCommandName keeps the grammar narrow so that a line of prose beginning
// with `::` does not become a command with a name made of arbitrary bytes.
func isCommandName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// unescapeData reverses the escaping GitHub applies to a command's data.
//
// Only these three, and %25 last would be wrong: decoding %25 first would turn
// the literal text `%250A` — which escapes to mean a literal "%0A" — into a
// newline. Replacing left to right in one pass avoids re-scanning what was
// just written.
func unescapeData(s string) string { return unescape(s, dataEscapes) }

// unescapeProperty additionally decodes the separators, which a property value
// cannot contain literally.
func unescapeProperty(s string) string { return unescape(s, propertyEscapes) }

var (
	dataEscapes     = map[string]string{"0A": "\n", "0D": "\r", "25": "%"}
	propertyEscapes = map[string]string{"0A": "\n", "0D": "\r", "25": "%", "3A": ":", "2C": ","}
)

// unescape decodes %XX sequences in one left-to-right pass.
//
// One pass, rather than a sequence of strings.ReplaceAll calls: replacing %25
// with "%" in a separate pass over the whole string lets the output of one
// replacement be read as the input of another, so `%250A` would decode to a
// newline instead of the literal `%0A` the author escaped.
func unescape(s string, table map[string]string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) {
			if rep, ok := table[strings.ToUpper(s[i+1:i+3])]; ok {
				b.WriteString(rep)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
