package expr

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokOp
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokComma
	tokDot
	tokStar
)

type token struct {
	kind tokenKind
	text string
	num  float64
	pos  int
}

// lex turns expression source into tokens.
//
// The grammar is small enough that a hand-written scanner is shorter than the
// machinery to generate one, and it keeps the error messages positional: a
// caller gets "at offset 12" rather than a parser-generator's vocabulary.
func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		case c == '(':
			out = append(out, token{kind: tokLParen, text: "(", pos: i})
			i++
		case c == ')':
			out = append(out, token{kind: tokRParen, text: ")", pos: i})
			i++
		case c == '[':
			out = append(out, token{kind: tokLBracket, text: "[", pos: i})
			i++
		case c == ']':
			out = append(out, token{kind: tokRBracket, text: "]", pos: i})
			i++
		case c == ',':
			out = append(out, token{kind: tokComma, text: ",", pos: i})
			i++
		case c == '.':
			out = append(out, token{kind: tokDot, text: ".", pos: i})
			i++
		case c == '*':
			out = append(out, token{kind: tokStar, text: "*", pos: i})
			i++

		case c == '\'':
			// Single quotes only, and '' is an escaped quote. A double-quoted
			// string is not a string in this language; it is a syntax error,
			// and saying so is more useful than accepting it, because YAML
			// makes it very easy to write one by accident.
			j := i + 1
			var b strings.Builder
			for {
				if j >= len(src) {
					return nil, fmt.Errorf("unterminated string starting at offset %d", i)
				}
				if src[j] == '\'' {
					if j+1 < len(src) && src[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					j++
					break
				}
				b.WriteByte(src[j])
				j++
			}
			out = append(out, token{kind: tokString, text: b.String(), pos: i})
			i = j

		case c == '"':
			return nil, fmt.Errorf(`double-quoted string at offset %d: expression strings use single quotes ('like this')`, i)

		case isDigitByte(c):
			j := i
			if strings.HasPrefix(strings.ToLower(src[i:]), "0x") {
				j = i + 2
				for j < len(src) && isHexByte(src[j]) {
					j++
				}
				n, err := strconv.ParseUint(src[i+2:j], 16, 64)
				if err != nil {
					return nil, fmt.Errorf("bad hex number at offset %d", i)
				}
				out = append(out, token{kind: tokNumber, num: float64(n), text: src[i:j], pos: i})
				i = j
				continue
			}
			for j < len(src) && (isDigitByte(src[j]) || src[j] == '.') {
				j++
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && isDigitByte(src[j]) {
					j++
				}
			}
			n, err := strconv.ParseFloat(src[i:j], 64)
			if err != nil {
				return nil, fmt.Errorf("bad number %q at offset %d", src[i:j], i)
			}
			out = append(out, token{kind: tokNumber, num: n, text: src[i:j], pos: i})
			i = j

		case isIdentStart(rune(c)):
			j := i
			for j < len(src) && isIdentPart(rune(src[j])) {
				j++
			}
			out = append(out, token{kind: tokIdent, text: src[i:j], pos: i})
			i = j

		default:
			op, n := lexOperator(src[i:])
			if n == 0 {
				return nil, fmt.Errorf("unexpected character %q at offset %d", string(c), i)
			}
			out = append(out, token{kind: tokOp, text: op, pos: i})
			i += n
		}
	}
	out = append(out, token{kind: tokEOF, pos: len(src)})
	return out, nil
}

// lexOperator matches the two-character operators before the one-character
// ones, so that `!=` is never read as `!` followed by `=`.
func lexOperator(s string) (string, int) {
	for _, op := range []string{"==", "!=", "<=", ">=", "&&", "||"} {
		if strings.HasPrefix(s, op) {
			return op, 2
		}
	}
	for _, op := range []string{"<", ">", "!"} {
		if strings.HasPrefix(s, op) {
			return op, 1
		}
	}
	return "", 0
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isHexByte(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isIdentStart(r rune) bool {
	return r == '_' || r == '-' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return isIdentStart(r) || unicode.IsDigit(r)
}
