package ghcmd

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxFileBytes bounds one of the key/value files per step.
//
// The same 64 KB §9 already gives $LATEM_ENV, and for the same reason: the
// file is job-authored and the agent reads it into memory and then believes
// it. Past the cap the tail is discarded along with the partial line at the
// cut, so that half a value never becomes a whole one.
const MaxFileBytes = 64 << 10

// keyPattern is §9's grammar for a key a job may set — the POSIX identifier
// rule. It is why a line like `PATH=x; rm -rf /` cannot become anything but a
// variable named PATH: the key is checked and the value is never interpreted.
var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Assignment is one key and its value.
type Assignment struct {
	Key   string
	Value string
}

// ParseKeyValueFile reads the format $GITHUB_ENV and $GITHUB_OUTPUT share.
//
// Two forms per assignment:
//
//	KEY=value
//	KEY<<DELIMITER
//	…any number of lines…
//	DELIMITER
//
// protected reports keys the job may not set. It carries the same duty it has
// for $LATEM_ENV: every LATEM_* name, so a job cannot spoof the agent's own
// variables, and every name already holding an injected credential, so a job
// cannot overwrite one.
//
// Order is preserved and duplicates are kept rather than collapsed, because
// last-writer-wins is the caller's rule to apply and a caller that wants to
// warn about a key set twice cannot do so from a map.
//
// On the heredoc form and its sharp edge: a value containing a line equal to
// the delimiter ends the value there, and what follows is read as further
// assignments. That is inherent to the format — GitHub's own answer is to tell
// authors to pick an unguessable delimiter — and it is bounded here rather than
// solved: the keys that can be set that way still have to be identifiers and
// still have to pass protected, so the worst case is an ordinary variable the
// job could have set anyway, not a credential and not a LATEM_ name.
func ParseKeyValueFile(data []byte, protected func(string) bool) ([]Assignment, []string) {
	var warnings []string
	body, truncated := capBytes(data)
	if truncated {
		warnings = append(warnings, fmt.Sprintf(
			"the file exceeded the %d byte limit; everything past the last complete line before the cut was ignored", MaxFileBytes))
	}

	var out []Assignment
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}

		if key, delim, ok := cutHeredoc(line); ok {
			value, consumed, closed := readHeredoc(lines, i+1, delim)
			i = consumed
			if !closed {
				// Nothing is taken from an unterminated block. The author
				// wrote a delimiter that never arrived, so where the value was
				// meant to end is not knowable, and guessing "end of file"
				// would silently set a variable to the rest of the step's
				// bookkeeping.
				warnings = append(warnings, fmt.Sprintf(
					"%s: the heredoc was never closed, so nothing was set from it", describe(key)))
				continue
			}
			if w := admit(key, protected); w != "" {
				warnings = append(warnings, w)
				continue
			}
			out = append(out, Assignment{Key: key, Value: value})
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// The line is neither KEY=value nor KEY<<DELIM. It is not quoted
			// back: a malformed line may be anything, including a secret a
			// script printed to the wrong descriptor, and echoing it to
			// explain that it was dropped would be its own leak.
			warnings = append(warnings, fmt.Sprintf(
				"line %d is neither KEY=value nor KEY<<DELIMITER, and was ignored", i+1))
			continue
		}
		if w := admit(key, protected); w != "" {
			warnings = append(warnings, w)
			continue
		}
		out = append(out, Assignment{Key: key, Value: value})
	}
	return out, warnings
}

// ParsePathFile reads $GITHUB_PATH, which is one directory per line and has no
// keys to validate — the caller prepends them to PATH.
func ParsePathFile(data []byte) ([]string, []string) {
	var warnings []string
	body, truncated := capBytes(data)
	if truncated {
		warnings = append(warnings, fmt.Sprintf(
			"the file exceeded the %d byte limit; everything past the last complete line before the cut was ignored", MaxFileBytes))
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out, warnings
}

// admit returns a warning when a key may not be set, and "" when it may.
func admit(key string, protected func(string) bool) string {
	if !keyPattern.MatchString(key) {
		// The key is quoted only when it is already known to be a well-formed
		// identifier; here it is not, so it is described by shape.
		return fmt.Sprintf("a key of %d bytes is not an identifier, and was ignored", len(key))
	}
	if protected != nil && protected(key) {
		return fmt.Sprintf("%q is set by the agent or holds a declared secret, and was not overwritten", key)
	}
	return ""
}

// describe names a key in a warning only when it is safe to echo.
func describe(key string) string {
	if keyPattern.MatchString(key) {
		return fmt.Sprintf("%q", key)
	}
	return "a key that is not an identifier"
}

// cutHeredoc recognises `KEY<<DELIMITER`.
func cutHeredoc(line string) (key, delim string, ok bool) {
	key, delim, ok = strings.Cut(line, "<<")
	if !ok {
		return "", "", false
	}
	delim = strings.TrimSpace(delim)
	if delim == "" {
		return key, "", false
	}
	return key, delim, true
}

// readHeredoc collects lines until the delimiter, returning the index of the
// delimiter line so the caller's loop resumes after it.
func readHeredoc(lines []string, from int, delim string) (value string, last int, closed bool) {
	var collected []string
	for i := from; i < len(lines); i++ {
		if lines[i] == delim {
			return strings.Join(collected, "\n"), i, true
		}
		collected = append(collected, lines[i])
	}
	return "", len(lines) - 1, false
}

// capBytes trims to the cap at a line boundary.
func capBytes(data []byte) ([]byte, bool) {
	if len(data) <= MaxFileBytes {
		return data, false
	}
	cut := data[:MaxFileBytes]
	if i := strings.LastIndexByte(string(cut), '\n'); i >= 0 {
		return cut[:i+1], true
	}
	return nil, true
}

// AccumulatePath folds one step's $GITHUB_PATH lines into the additions so
// far. Each line is prepended, in file order, so the last line written ends up
// first on PATH — which is what GitHub does, and what a step that writes two
// directories and expects the second to win relies on.
func AccumulatePath(sofar, fromFile []string) []string {
	out := sofar
	for _, dir := range fromFile {
		out = append([]string{dir}, out...)
	}
	return out
}

// PrependPath renders PATH for a step: the additions, then whatever PATH was.
// An empty current PATH yields the additions alone rather than a trailing
// separator, which some shells read as "and the current directory".
func PrependPath(adds []string, current string) string {
	if len(adds) == 0 {
		return current
	}
	joined := strings.Join(adds, ":")
	if current == "" {
		return joined
	}
	return joined + ":" + current
}
