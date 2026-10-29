package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// filterPattern is one entry of an `on:` filter — branches, tags, paths —
// compiled to a regular expression (A52).
//
// GitHub's syntax, as its documentation gives it: `*` matches anything but
// `/`; `**` matches anything, `/` included, and `**/` also matches nothing, so
// `**/x` finds `x` at the root; `?` and `+` make the character before them
// optional or repeated; `[...]` is a character class; a leading `!` negates.
// Everything else is literal.
type filterPattern struct {
	re     *regexp.Regexp
	negate bool
	src    string
}

func compileFilter(src string) (filterPattern, error) {
	p := filterPattern{src: src}
	s := src
	if strings.HasPrefix(s, "!") {
		p.negate, s = true, s[1:]
	}
	if s == "" {
		return p, fmt.Errorf("filter %q matches nothing", src)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '*' && i+1 < len(s) && s[i+1] == '*':
			i++
			if i+1 < len(s) && s[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?' || c == '+':
			// A quantifier on what came before, as GitHub defines them.
			b.WriteByte(c)
		case c == '[':
			end := strings.IndexByte(s[i+1:], ']')
			if end < 0 {
				return p, fmt.Errorf("filter %q has an unclosed [", src)
			}
			b.WriteString("[" + s[i+1:i+1+end] + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return p, fmt.Errorf("filter %q: %w", src, err)
	}
	p.re = re
	return p, nil
}

// filterList is a filter's patterns in order. A name matches the list when
// the last pattern that matches it is a positive one — so a later `!` pattern
// excludes what an earlier one included, and a later positive one brings it
// back, which is how GitHub reads the list.
type filterList []filterPattern

func compileFilters(srcs []string) (filterList, error) {
	out := make(filterList, 0, len(srcs))
	for _, s := range srcs {
		p, err := compileFilter(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (l filterList) matches(name string) bool {
	matched := false
	for _, p := range l {
		if p.re.MatchString(name) {
			matched = !p.negate
		}
	}
	return matched
}
