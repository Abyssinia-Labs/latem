package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/config"
)

// The onboarding commands edit config.toml as text, for the reason
// writeRegistration gives: it is a file people write by hand, with their
// comments, their order and their formatting in it, and decoding it into a
// struct and encoding it back would destroy all three. Each command changes
// the tables it owns and leaves every other byte where it was.
//
// This is not a TOML parser. It finds table headers — `[a.b]` and `[[a]]`,
// indented or not, since §8's own examples indent `[runner.docker]` — and
// treats each as owning the lines up to the next one. Every edit is then
// re-validated by the real loader before it is written; see [editConfig].

// tomlBlock is one table header and the lines it owns.
type tomlBlock struct {
	path  []string // the header's key, unquoted: [repos."next.js"] is {repos, next.js}
	array bool     // [[...]]
	// start is the header's line; end is one past the table's last line that
	// is neither blank nor a comment. The blank lines and comments between end
	// and the next header introduce that header, so they are not this table's
	// to delete.
	start, end int
	indent     string
}

var (
	bareKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	headerPattern  = regexp.MustCompile(`^(\[\[?)(.*?)(\]\]?)\s*(#.*)?$`)
)

// splitLines keeps each line's newline, so joining them is the original text.
func splitLines(body string) []string {
	if body == "" {
		return nil
	}
	return strings.SplitAfter(body, "\n")
}

// parseHeader reads a table header line, or reports that it is not one. A
// line inside a multi-line array can start with `[` too, and fails here
// because its contents are not a key.
func parseHeader(line string) (path []string, array bool, ok bool) {
	trimmed := strings.TrimSpace(line)
	m := headerPattern.FindStringSubmatch(trimmed)
	if m == nil || (m[1] == "[[") != (m[3] == "]]") {
		return nil, false, false
	}
	path, ok = parseDottedKey(m[2])
	return path, m[1] == "[[", ok
}

// parseDottedKey splits `a."b.c".d` into its parts.
func parseDottedKey(s string) ([]string, bool) {
	var parts []string
	rest := strings.TrimSpace(s)
	for {
		var part string
		switch {
		case rest == "":
			return nil, false
		case rest[0] == '"':
			end := closingQuote(rest)
			if end < 0 {
				return nil, false
			}
			unq, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				return nil, false
			}
			part, rest = unq, rest[end+1:]
		case rest[0] == '\'':
			end := strings.IndexByte(rest[1:], '\'')
			if end < 0 {
				return nil, false
			}
			part, rest = rest[1:end+1], rest[end+2:]
		default:
			end := strings.IndexAny(rest, ". \t")
			if end < 0 {
				end = len(rest)
			}
			part, rest = rest[:end], rest[end:]
			if !bareKeyPattern.MatchString(part) {
				return nil, false
			}
		}
		parts = append(parts, part)
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return parts, true
		}
		if rest[0] != '.' {
			return nil, false
		}
		rest = strings.TrimSpace(rest[1:])
	}
}

// closingQuote is the index of the quote ending a basic string at s[0].
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// tomlBlocks lists every table header in body, in order.
func tomlBlocks(lines []string) []tomlBlock {
	var blocks []tomlBlock
	for i, line := range lines {
		path, array, ok := parseHeader(line)
		if !ok {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		blocks = append(blocks, tomlBlock{path: path, array: array, start: i, indent: indent})
	}
	for i := range blocks {
		next := len(lines)
		if i+1 < len(blocks) {
			next = blocks[i+1].start
		}
		end := next
		for end > blocks[i].start+1 && isBlankOrComment(lines[end-1]) {
			end--
		}
		blocks[i].end = end
	}
	return blocks
}

func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// hasPrefix reports whether path starts with prefix.
func hasPrefix(path, prefix []string) bool {
	if len(path) < len(prefix) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

func samePath(a, b []string) bool { return len(a) == len(b) && hasPrefix(a, b) }

// tomlKey renders one key part: bare where TOML allows it, quoted otherwise.
func tomlKey(part string) string {
	if bareKeyPattern.MatchString(part) {
		return part
	}
	return tomlString(part)
}

// tomlHeader renders a table header for path.
func tomlHeader(path ...string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = tomlKey(p)
	}
	return "[" + strings.Join(parts, ".") + "]"
}

// tomlString renders s as a TOML basic string.
//
// Not %q. Go's quoting writes \a, \v and \x7f, which TOML does not have, so a
// value holding one would produce a file the loader refuses — and the gate in
// [editConfig] would then refuse the edit, for a reason that has nothing to do
// with anything the operator wrote.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlStringList renders a list of strings.
func tomlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = tomlString(v)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// removeTables deletes every table whose path starts with prefix, and returns
// how many it removed.
func removeTables(body string, prefix []string) (string, int) {
	lines := splitLines(body)
	drop := make([]bool, len(lines))
	n := 0
	for _, b := range tomlBlocks(lines) {
		if hasPrefix(b.path, prefix) {
			n++
			for i := b.start; i < b.end; i++ {
				drop[i] = true
			}
		}
	}
	if n == 0 {
		return body, 0
	}
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	return collapseBlankLines(strings.Join(kept, "")), n
}

// collapseBlankLines leaves at most one blank line between two others, so a
// removed table does not leave a hole the size of itself.
func collapseBlankLines(body string) string {
	lines := splitLines(body)
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.Join(out, "")
}

// insertAfter places text after the last table whose path starts with
// prefix, or at the end of the file when there is none. Grouping a new repo
// with the others keeps the file readable the way its author laid it out.
func insertAfter(body string, prefix []string, text string) string {
	lines := splitLines(body)
	at := -1
	for _, b := range tomlBlocks(lines) {
		if hasPrefix(b.path, prefix) {
			at = b.end
		}
	}
	text = strings.TrimRight(text, "\n") + "\n"
	if at < 0 {
		return strings.TrimRight(body, "\n") + "\n\n" + text
	}
	head := strings.TrimRight(strings.Join(lines[:at], ""), "\n")
	tail := strings.Join(lines[at:], "")
	return head + "\n\n" + text + "\n" + strings.TrimLeft(tail, "\n")
}

// setTableKey sets key = literal in the table at path, creating the table
// after the last table under parent when it does not exist.
func setTableKey(body string, path, parent []string, key, literal string) (string, error) {
	lines := splitLines(body)
	for _, b := range tomlBlocks(lines) {
		if samePath(b.path, path) && !b.array {
			return setBlockKey(lines, b, key, literal)
		}
	}
	return insertAfter(body, parent, tomlHeader(path...)+"\n"+tomlKey(key)+" = "+literal+"\n"), nil
}

// setBlockKey sets key = literal in the table b, replacing the key's line or
// adding one after the table's last, at its indent.
func setBlockKey(lines []string, b tomlBlock, key, literal string) (string, error) {
	indent := b.indent
	for i := b.start + 1; i < b.end; i++ {
		k := tomlKeyOf(lines[i])
		if k == "" {
			continue
		}
		indent = lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
		if k != key {
			continue
		}
		if !singleLineValue(lines[i]) {
			return "", fmt.Errorf("%s.%s spans more than one line in config.toml; edit it by hand",
				strings.Join(b.path, "."), key)
		}
		lines[i] = indent + tomlKey(key) + " = " + literal + "\n"
		return strings.Join(lines, ""), nil
	}
	line := indent + tomlKey(key) + " = " + literal + "\n"
	out := append(append(append([]string{}, lines[:b.end]...), line), lines[b.end:]...)
	if b.end > 0 && !strings.HasSuffix(lines[b.end-1], "\n") {
		out[b.end-1] += "\n"
	}
	return strings.Join(out, ""), nil
}

// setRunnerKey sets key = literal in the table [runner.<sub>] of the
// [[runner]] named name, creating that table after the runner's own last
// one when it does not exist. The runner is found by position: the loaded
// config keeps the file's order of [[runner]] tables, and a name is a value
// this does not parse.
func setRunnerKey(body string, cfg *config.Config, name string, sub []string, key, literal string) (string, error) {
	n := slices.IndexFunc(cfg.Runners, func(r *config.Runner) bool { return r.Name == name })
	if n < 0 {
		return "", fmt.Errorf("config.toml has no runner %q", name)
	}
	lines := splitLines(body)
	blocks := tomlBlocks(lines)
	runner, path := []string{"runner"}, append([]string{"runner"}, sub...)
	seen := -1
	for i, b := range blocks {
		if !b.array || !samePath(b.path, runner) {
			continue
		}
		if seen++; seen != n {
			continue
		}
		last := b
		for _, c := range blocks[i+1:] {
			if c.array || !hasPrefix(c.path, runner) {
				break
			}
			if samePath(c.path, path) {
				return setBlockKey(lines, c, key, literal)
			}
			last = c
		}
		indent := b.indent + "  "
		text := indent + tomlHeader(path...) + "\n" + indent + tomlKey(key) + " = " + literal + "\n"
		out := strings.TrimRight(strings.Join(lines[:last.end], ""), "\n") + "\n\n" + text
		if rest := strings.TrimLeft(strings.Join(lines[last.end:], ""), "\n"); rest != "" {
			out += "\n" + rest
		}
		return out, nil
	}
	return "", fmt.Errorf("config.toml has %d [[runner]] tables; runner %q is number %d", seen+1, name, n+1)
}

// removeTableKey deletes key from the table at path, and the table with it
// when nothing is left in it. It reports whether the key was there.
func removeTableKey(body string, path []string, key string) (string, bool, error) {
	lines := splitLines(body)
	for _, b := range tomlBlocks(lines) {
		if !samePath(b.path, path) || b.array {
			continue
		}
		for i := b.start + 1; i < b.end; i++ {
			if tomlKeyOf(lines[i]) != key {
				continue
			}
			if !singleLineValue(lines[i]) {
				return "", false, fmt.Errorf("%s.%s spans more than one line in config.toml; edit it by hand",
					strings.Join(path, "."), key)
			}
			rest := append(append([]string{}, lines[:i]...), lines[i+1:]...)
			empty := true
			for j := b.start + 1; j < b.end-1; j++ {
				if tomlKeyOf(rest[j]) != "" {
					empty = false
				}
			}
			out := strings.Join(rest, "")
			if empty {
				out, _ = removeTables(out, path)
			}
			return out, true, nil
		}
	}
	return body, false, nil
}

// singleLineValue reports whether a key's value ends on its own line. A
// multi-line string or array cannot be replaced one line at a time.
func singleLineValue(line string) bool {
	_, v, _ := strings.Cut(line, "=")
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, `"""`), strings.HasPrefix(v, `'''`):
		return strings.Count(v, v[:3]) >= 2
	case strings.HasPrefix(v, "["):
		return strings.Count(v, "[") == strings.Count(v, "]")
	case strings.HasPrefix(v, "{"):
		return strings.Count(v, "{") == strings.Count(v, "}")
	}
	return true
}

// editConfig applies edit to config.toml and installs the result, under the
// rule writeRegistration states: this command must never make a loadable
// config unloadable. A result that does not load is refused, and the file is
// left as it was, unless the original did not load either — then the edit
// cannot have broken anything, and the loader's complaint comes back as a
// warning.
func editConfig(paths config.Paths, edit func(body string) (string, error)) (warning string, err error) {
	original, err := os.ReadFile(paths.File)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("there is no %s yet; `latem setup` writes one", paths.File)
		}
		return "", fmt.Errorf("reading %s: %w", paths.File, err)
	}
	updated, err := edit(string(original))
	if err != nil {
		return "", err
	}
	if updated == string(original) {
		return "", nil
	}

	_, loadedBefore := config.Parse(original, config.Options{Dir: paths.Dir})
	_, loadsAfter := config.Parse([]byte(updated), config.Options{Dir: paths.Dir})
	switch {
	case loadsAfter == nil:
	case loadedBefore == nil:
		return "", fmt.Errorf("the edited config would not load, so %s was left as it was: %w", paths.File, loadsAfter)
	default:
		warning = fmt.Sprintf("%s still does not load, and this did not change that:\n  %v", paths.File, loadsAfter)
	}
	if err := writeFileAtomic(paths.File, []byte(updated), 0o600); err != nil {
		return "", err
	}
	return warning, nil
}
