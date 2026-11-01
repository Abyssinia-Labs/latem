package needs

import (
	"regexp"
	"strings"
)

// A shell script read for the commands it runs, not run (A59).
//
// This is not a shell parser and does not try to be one: it finds the words
// in command position, which is the question — does this job run xcodebuild —
// and nothing else. What it must not do is mistake an argument, a comment, a
// heredoc's body or a string for a command, because each of those is where
// the words "ios", "android" and "docker" sit in jobs that need none of them.
//
// Three passes. Logical lines: continuations joined, a string that spans lines
// kept whole, comments and heredoc bodies dropped. Substitutions: each $(…)
// and `…` lifted out as a command of its own, and replaced in its line by a
// placeholder word. Words: each line split into simple commands, quotes
// resolved.

// command is one simple command: its words with leading assignments and
// wrappers removed, and where it was written.
type command struct {
	words []string
	// wrappers are what ran it: sudo, env, bundle exec.
	wrappers []string
	env      map[string]string // NAME=value written before it, or exported earlier
	line     int               // 1-based, in the script it came from
	text     string            // that line, trimmed, for evidence
}

// commands lists the simple commands in script, in order.
func commands(script string) []command {
	var out []command
	exported := map[string]string{}
	for _, ll := range logicalLines(script) {
		for _, piece := range liftSubstitutions(ll.text, 0) {
			for _, words := range splitCommands(piece) {
				c, ok := simplify(words, exported)
				if !ok {
					continue
				}
				c.line, c.text = ll.line, trimmed(ll.first)
				out = append(out, c)
			}
		}
	}
	return out
}

type logicalLine struct {
	text  string
	line  int
	first string // the first physical line, for evidence
}

// logicalLines joins continued lines, keeps a quoted string that spans lines
// in one, and drops comments and heredoc bodies.
func logicalLines(script string) []logicalLine {
	lines := strings.Split(script, "\n")
	var out []logicalLine
	var b strings.Builder
	var quote byte
	start := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if b.Len() == 0 && quote == 0 {
			start = i
		}
		var heredocs []heredoc
		for j := 0; j < len(line); j++ {
			c := line[j]
			switch {
			case quote == '\'':
				if c == '\'' {
					quote = 0
				}
			case quote == '"':
				if c == '\\' && j+1 < len(line) {
					b.WriteByte(c)
					j++
					c = line[j]
				} else if c == '"' {
					quote = 0
				}
			case c == '\'' || c == '"':
				quote = c
			case c == '\\':
				if j+1 < len(line) {
					b.WriteByte(c)
					j++
					c = line[j]
				}
			case c == '#' && (j == 0 || strings.IndexByte(" \t;|&(", line[j-1]) >= 0):
				j = len(line) // a comment, to the end of the line
				continue
			case c == '<' && strings.HasPrefix(line[j:], "<<") && !strings.HasPrefix(line[j:], "<<<"):
				if h, n, ok := heredocAt(line[j:]); ok {
					heredocs = append(heredocs, h)
					j += n - 1
					continue
				}
			}
			b.WriteByte(c)
		}
		if quote != 0 {
			b.WriteByte('\n') // a string that spans lines stays one word
			continue
		}
		if s := b.String(); strings.HasSuffix(strings.TrimRight(s, " \t"), "\\") {
			b.Reset()
			b.WriteString(strings.TrimSuffix(strings.TrimRight(s, " \t"), "\\"))
			b.WriteByte(' ')
			continue
		}
		out = append(out, logicalLine{text: b.String(), line: start + 1, first: lines[start]})
		b.Reset()
		// A heredoc's body is data, whatever words it holds.
		for _, h := range heredocs {
			for i+1 < len(lines) {
				i++
				body := lines[i]
				if h.dash {
					body = strings.TrimLeft(body, "\t")
				}
				if strings.TrimRight(body, " \t\r") == h.delim {
					break
				}
			}
		}
	}
	if b.Len() > 0 {
		out = append(out, logicalLine{text: b.String(), line: start + 1, first: lines[start]})
	}
	return out
}

type heredoc struct {
	delim string
	dash  bool
}

var heredocPattern = regexp.MustCompile(`^<<(-?)\s*(?:'([^']+)'|"([^"]+)"|\\?([A-Za-z_][A-Za-z0-9_]*))`)

// heredocAt reads a heredoc operator at the start of s, and how much of s it
// took.
func heredocAt(s string) (heredoc, int, bool) {
	m := heredocPattern.FindStringSubmatch(s)
	if m == nil {
		return heredoc{}, 0, false
	}
	return heredoc{delim: m[2] + m[3] + m[4], dash: m[1] == "-"}, len(m[0]), true
}

// maxSubstitutionDepth bounds $( $( … ) ) nesting.
const maxSubstitutionDepth = 8

// liftSubstitutions returns text with each command substitution replaced by
// a placeholder word, followed by each substitution's own text, recursively.
// Arithmetic, $(( … )), is not a command and is replaced without being read.
func liftSubstitutions(text string, depth int) []string {
	var outer strings.Builder
	var inner []string
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(text):
			outer.WriteByte(c)
			i++
			c = text[i]
		case c == '\'' && quote == 0:
			quote = c
		case c == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = c
			}
		case c == '$' && strings.HasPrefix(text[i:], "$(("):
			end := matching(text, i+1, '(', ')')
			if end < 0 {
				end = len(text) - 1
			}
			outer.WriteString("0")
			i = end
			continue
		case c == '$' && strings.HasPrefix(text[i:], "$("), c == '`':
			var body string
			if c == '`' {
				end := strings.IndexByte(text[i+1:], '`')
				if end < 0 {
					end = len(text) - i - 1
				}
				body, i = text[i+1:i+1+end], i+1+end
			} else {
				end := matching(text, i+1, '(', ')')
				if end < 0 {
					end = len(text)
				}
				body, i = text[i+2:min(end, len(text))], end
			}
			outer.WriteString("$SUBSTITUTION")
			if depth < maxSubstitutionDepth {
				inner = append(inner, liftSubstitutions(body, depth+1)...)
			}
			continue
		}
		outer.WriteByte(c)
	}
	return append([]string{outer.String()}, inner...)
}

// matching is the index of the close that balances the open at text[at], or
// -1, skipping quoted text.
func matching(text string, at int, open, close byte) int {
	depth := 0
	var quote byte
	for i := at; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitCommands splits one line into simple commands' words: separators end
// a command, quotes are resolved, and a redirection's target is dropped.
func splitCommands(text string) [][]string {
	var out [][]string
	var words []string
	var w strings.Builder
	inWord, redirect := false, false
	endWord := func() {
		if inWord {
			if !redirect {
				words = append(words, w.String())
			}
			redirect = false
			w.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
		}
		words, redirect = nil, false
	}
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				w.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(text):
				i++
				w.WriteByte(text[i])
			default:
				w.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\':
			if i+1 < len(text) {
				i++
				w.WriteByte(text[i])
				inWord = true
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			endWord()
		case strings.IndexByte(";|&(){}", c) >= 0:
			endCommand()
		case c == '>' || c == '<':
			// 2>&1, >file, < in: the target is not an argument.
			endWord()
			for i+1 < len(text) && (text[i+1] == '>' || text[i+1] == '&') {
				i++
			}
			if i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9' {
				i++ // >&2
				continue
			}
			redirect = true
		default:
			if c >= '0' && c <= '9' && !inWord && i+1 < len(text) && text[i+1] == '>' {
				continue // the 2 of 2>file
			}
			w.WriteByte(c)
			inWord = true
		}
	}
	endCommand()
	return out
}

var assignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// envAssignment is a word env(1) takes as NAME=value. A shell's names are
// narrow and env's are not: Gradle's ORG_GRADLE_PROJECT_android.injected.… has
// dots in it, and the build behind one was read as a command of that name.
var envAssignment = regexp.MustCompile(`^[^=\s-][^=\s]*=`)

// keywords are shell syntax that can stand where a command does.
var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true,
	"while": true, "until": true, "!": true, "time": true,
}

// notCommands are syntax, or builtins that run nothing.
var notCommands = map[string]bool{
	"for": true, "case": true, "esac": true, "in": true, "function": true, "[": true, "[[": true,
	"test": true, "true": true, "false": true, ":": true, "return": true, "exit": true, "echo": true,
	"printf": true, "cd": true, "pushd": true, "popd": true, "set": true, "unset": true, "shift": true,
	"read": true, "local": true, "declare": true, "trap": true, "wait": true, "break": true,
	"continue": true, "$SUBSTITUTION": true,
}

// simplify removes what stands in front of the command a line runs —
// assignments, keywords, and wrappers that run another command — and reports
// whether a command is left. An export is remembered for the commands after
// it.
func simplify(words []string, exported map[string]string) (command, bool) {
	c := command{}
	i := 0
	for i < len(words) {
		if m := assignment.FindStringSubmatch(words[i]); m != nil {
			if c.env == nil {
				c.env = map[string]string{}
			}
			c.env[m[1]] = m[2]
			i++
			continue
		}
		if keywords[words[i]] {
			i++
			continue
		}
		break
	}
	if i == len(words) || notCommands[words[i]] {
		return c, false
	}
	if words[i] == "export" {
		for _, w := range words[i+1:] {
			if m := assignment.FindStringSubmatch(w); m != nil {
				exported[m[1]] = m[2]
			}
		}
		return c, false
	}
	rest, wrappers := unwrap(words[i:])
	if len(rest) == 0 {
		return c, false
	}
	c.words, c.wrappers = rest, wrappers
	for k, v := range exported {
		if c.env == nil {
			c.env = map[string]string{}
		}
		if _, set := c.env[k]; !set {
			c.env[k] = v
		}
	}
	return c, true
}

// unwrap removes wrappers that run the command after them, and says which it
// removed. `command -v`, `which` and `type` look a command up and run
// nothing.
func unwrap(words []string) (rest, wrappers []string) {
	for len(words) > 0 {
		w := words[0]
		switch w {
		case "sudo", "doas":
			words = skipFlags(words[1:], map[string]bool{"-u": true, "-g": true, "-C": true, "-D": true})
		case "env":
			words = skipFlags(words[1:], map[string]bool{"-u": true, "-C": true, "-S": true})
			for len(words) > 0 && envAssignment.MatchString(words[0]) {
				words = words[1:]
			}
		case "nice":
			words = skipFlags(words[1:], map[string]bool{"-n": true})
		case "nohup", "exec", "caffeinate", "stdbuf", "unbuffer", "arch", "xargs":
			words = skipFlags(words[1:], map[string]bool{"-I": true, "-n": true})
		case "timeout", "gtimeout":
			words = skipFlags(words[1:], map[string]bool{"-s": true, "-k": true})
			if len(words) > 0 {
				words = words[1:] // the duration
			}
		case "command":
			if len(words) > 1 && (words[1] == "-v" || words[1] == "-V") {
				return nil, wrappers
			}
			words = words[1:]
		case "which", "type", "hash":
			return nil, wrappers
		case "bunx", "npx":
			words = skipFlags(words[1:], map[string]bool{"-p": true, "--package": true})
		case "bundle", "pnpm", "yarn", "uv", "poetry", "pipenv", "bun":
			if len(words) > 1 && runsTheNext(w, words[1]) {
				wrappers = append(wrappers, w+" "+words[1])
				words = skipFlags(words[2:], nil)
				continue
			}
			return words, wrappers
		default:
			return words, wrappers
		}
		wrappers = append(wrappers, w)
	}
	return words, wrappers
}

// runsTheNext reports whether tool's subcommand sub runs the command after it,
// rather than a script of the project's (`bun run build`).
func runsTheNext(tool, sub string) bool {
	switch tool {
	case "bundle":
		return sub == "exec"
	case "pnpm", "yarn":
		return sub == "exec" || sub == "dlx"
	case "bun":
		return sub == "x"
	case "uv", "poetry", "pipenv":
		return sub == "run"
	}
	return false
}

// skipFlags drops leading flags, and the argument of a flag that takes one.
func skipFlags(words []string, takesArg map[string]bool) []string {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		if takesArg[words[0]] && len(words) > 1 {
			words = words[2:]
			continue
		}
		words = words[1:]
	}
	return words
}

func trimmed(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}
