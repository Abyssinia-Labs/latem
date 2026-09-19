// Package expr evaluates the GitHub Actions expression language — the contents
// of a ${{ … }} — per amendment A42.
//
// The value model is JSON's, held in Go's ordinary types: nil, bool, float64,
// string, []any and map[string]any. That is not a shortcut. `fromJSON` and
// `toJSON` are two of the eleven functions, `needs.*.outputs` arrives as
// decoded JSON, and a model that had its own number type would have to convert
// at every boundary and would disagree with one of them eventually.
//
// What this package deliberately does not do is produce shell. A42 makes that
// normative: a context value reaches a step as an environment variable, never
// as substituted characters in a command string, because `${{ github.event.* }}`
// interpolated into `run:` is the injection class GitHub itself issues
// advisories for. Evaluation answers with a Value; deciding where it may be
// written is the caller's job, and [github.com/Abyssinia-Labs/latem/internal/workflow]
// is the caller that refuses to write one into a command.
package expr

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Value is a JSON value: nil, bool, float64, string, []any or map[string]any.
type Value = any

// truthy reports a value's truthiness using GitHub's rules, which are
// JavaScript's: null, false, 0, NaN and the empty string are false and
// everything else — including an empty array and an empty object — is true.
func truthy(v Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	default:
		return true
	}
}

// toNumber coerces for comparison. The rules are GitHub's own, and the reason
// they are written out rather than approximated is that two of them are
// surprising and load-bearing: an empty string is 0 rather than NaN, and any
// array or object is NaN, which makes every comparison against one false.
func toNumber(v Value) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		// ParseFloat handles the decimal and exponent forms. Hex is legal in
		// the expression grammar and is not something ParseFloat accepts with
		// a 0x prefix at bitSize 64, so it is taken separately.
		if len(s) > 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
			if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
				return float64(n)
			}
			return math.NaN()
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return n
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

// toString renders a value the way `format` and string coercion do.
//
// A float that holds an integer prints without a decimal point: `format('{0}',
// 1)` is "1" and not "1.000000". Getting that wrong is invisible in a unit test
// of the evaluator and extremely visible in a tag name.
func toString(v Value) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		switch {
		case math.IsNaN(t):
			return "NaN"
		case math.IsInf(t, 1):
			return "Infinity"
		case math.IsInf(t, -1):
			return "-Infinity"
		case t == math.Trunc(t) && math.Abs(t) < 1e21:
			return strconv.FormatFloat(t, 'f', -1, 64)
		default:
			return strconv.FormatFloat(t, 'g', -1, 64)
		}
	case string:
		return t
	case []any, filtered:
		return "Array"
	case map[string]any:
		return "Object"
	default:
		return fmt.Sprint(t)
	}
}

// equal implements == with GitHub's loose comparison.
//
// Two strings compare case-insensitively — that is not a typo and not a
// convenience, it is the documented behaviour, and a rule that is only written
// in prose gets reimplemented as a case-sensitive one by the next person.
// Anything else is coerced to a number first, so `'1' == 1` holds and
// `'abc' == 0` does not.
func equal(a, b Value) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.EqualFold(as, bs)
	}
	// Two composites are equal only by identity of content, and GitHub compares
	// them by reference — two separately-constructed empty objects are not
	// equal. Reference identity is not observable here, so content equality is
	// the closest honest answer and is what toJSON-based comparisons expect.
	if isComposite(a) && isComposite(b) {
		return toJSONString(a) == toJSONString(b)
	}
	if isComposite(a) != isComposite(b) {
		return false
	}
	x, y := toNumber(a), toNumber(b)
	if math.IsNaN(x) || math.IsNaN(y) {
		return false
	}
	return x == y
}

func isComposite(v Value) bool {
	switch v.(type) {
	case []any, filtered, map[string]any:
		return true
	}
	return false
}

// compare implements <, <=, > and >=. Every ordering comparison is numeric,
// including between two strings, and any NaN makes the answer false — so
// `'a' < 'b'` is false and so is `'a' >= 'b'`.
func compare(op string, a, b Value) bool {
	x, y := toNumber(a), toNumber(b)
	if math.IsNaN(x) || math.IsNaN(y) {
		return false
	}
	switch op {
	case "<":
		return x < y
	case "<=":
		return x <= y
	case ">":
		return x > y
	case ">=":
		return x >= y
	}
	return false
}

// toJSONString renders a value as JSON with object keys sorted, which is what
// makes it usable for the composite equality above as well as for `toJSON`.
func toJSONString(v Value) string {
	var b strings.Builder
	writeJSON(&b, v, "")
	return b.String()
}

func writeJSON(b *strings.Builder, v Value, indent string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool, float64:
		b.WriteString(toString(t))
	case string:
		b.WriteString(strconv.Quote(t))
	case filtered:
		writeJSON(b, t.plain(), indent)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		inner := indent + "  "
		b.WriteString("[\n")
		for i, e := range t {
			b.WriteString(inner)
			writeJSON(b, e, inner)
			if i < len(t)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
	case map[string]any:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		inner := indent + "  "
		b.WriteString("{\n")
		for i, k := range keys {
			b.WriteString(inner + strconv.Quote(k) + ": ")
			writeJSON(b, t[k], inner)
			if i < len(keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
	default:
		b.WriteString(strconv.Quote(toString(t)))
	}
}

// Stringify renders a value the way the expression language does when it lands
// in text — which is what a hoisted environment variable holds.
func Stringify(v Value) string { return toString(v) }
