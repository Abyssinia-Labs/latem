package expr

import (
	"fmt"
	"strings"
)

// Span is one ${{ … }} occurrence inside a template string.
type Span struct {
	Start int // index of the "${{"
	End   int // index just past the "}}"
	Prog  *Program
}

// Spans finds and compiles every ${{ … }} in s.
//
// It is exported because the caller that matters does not want the substituted
// string at all. A42 forbids writing a context value into a command, so
// [github.com/Abyssinia-Labs/latem/internal/workflow] takes the spans, binds
// each one to an environment variable, and rewrites the command to reference
// the variable — the value never becomes shell text, so a branch named
// `; rm -rf /` is a branch name.
func Spans(s string) ([]Span, error) {
	var out []Span
	for i := 0; i < len(s); {
		start := strings.Index(s[i:], "${{")
		if start < 0 {
			break
		}
		start += i
		end := findClose(s, start+3)
		if end < 0 {
			return nil, fmt.Errorf("unclosed ${{ at offset %d", start)
		}
		prog, err := Parse(s[start+3 : end])
		if err != nil {
			return nil, fmt.Errorf("in ${{ … }} at offset %d: %w", start, err)
		}
		out = append(out, Span{Start: start, End: end + 2, Prog: prog})
		i = end + 2
	}
	return out, nil
}

// findClose locates the "}}" that closes an expression, skipping any that falls
// inside a single-quoted string. Without that, `${{ contains(x, '}}') }}` would
// terminate in the middle of a literal.
func findClose(s string, from int) int {
	inString := false
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			if inString && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case !inString && s[i] == '}' && i+1 < len(s) && s[i+1] == '}':
			return i
		}
	}
	return -1
}

// Has reports whether s contains an expression at all, which is the cheap check
// a parser wants before doing any of the above.
func Has(s string) bool { return strings.Contains(s, "${{") }

// EvalString substitutes every expression in s and returns the resulting text.
//
// This is correct for the places a workflow uses a value as a value — `if:`,
// `env:`, `with:`, `timeout-minutes:`. It is *not* how a `run:` is built; see
// [Spans].
func EvalString(s string, sc Scope) (string, error) {
	spans, err := Spans(s)
	if err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return s, nil
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp.Start])
		v, err := sp.Prog.Eval(sc)
		if err != nil {
			return "", err
		}
		b.WriteString(toString(v))
		last = sp.End
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// EvalValue evaluates s and preserves the value's type when s is exactly one
// expression and nothing else — `${{ fromJSON(x) }}` yields an array rather
// than the string "Array". Any surrounding text makes the result a string,
// which is the same rule GitHub applies.
func EvalValue(s string, sc Scope) (Value, error) {
	spans, err := Spans(s)
	if err != nil {
		return nil, err
	}
	if len(spans) == 1 && spans[0].Start == 0 && spans[0].End == len(s) {
		return spans[0].Prog.Eval(sc)
	}
	return EvalString(s, sc)
}

// Truthy evaluates s as a condition, which is what `if:` needs.
//
// A bare `if:` in a workflow may omit the ${{ }} — `if: github.ref == 'x'` is
// legal and common — so a string with no expression markers is compiled as an
// expression rather than treated as a literal true.
func Truthy(s string, sc Scope) (bool, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return true, nil
	}
	if !Has(trimmed) {
		prog, err := Parse(trimmed)
		if err != nil {
			return false, err
		}
		v, err := prog.Eval(sc)
		if err != nil {
			return false, err
		}
		return truthy(v), nil
	}
	v, err := EvalValue(trimmed, sc)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}
