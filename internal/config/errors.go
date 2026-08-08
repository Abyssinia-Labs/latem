package config

import (
	"errors"
	"sort"
	"strings"
)

// Error is a config that could not be used, carrying every reason at once.
//
// Reporting one problem per attempt turns fixing a config into a series of
// restarts, and a build machine is exactly the thing nobody wants to restart
// repeatedly. internal/registry made the same choice for jobs.yaml; the two
// files are edited by the same person on the same afternoon and should behave
// the same way.
type Error struct {
	// Path is the config file the problems belong to. It may be empty for a
	// [Parse] result, which has no file.
	Path     string
	Problems []error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("config")
	if e.Path != "" {
		b.WriteString(" ")
		b.WriteString(e.Path)
	}
	switch len(e.Problems) {
	case 0:
		b.WriteString(" is invalid")
		return b.String()
	case 1:
		b.WriteString(": ")
		b.WriteString(e.Problems[0].Error())
		return b.String()
	}
	b.WriteString(" has ")
	b.WriteString(itoa(len(e.Problems)))
	b.WriteString(" problems:")
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.Error())
	}
	return b.String()
}

// Unwrap exposes the individual problems to [errors.Is] and [errors.As].
func (e *Error) Unwrap() []error { return e.Problems }

// Problems returns the individual failures in an error produced by this
// package, so `latem doctor` can render them one per line with its own
// formatting rather than parsing the joined message back apart.
func Problems(err error) []error {
	var e *Error
	if errors.As(err, &e) {
		return e.Problems
	}
	if err == nil {
		return nil
	}
	return []error{err}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// sortedKeys gives validation a stable iteration order. Map order otherwise
// reshuffles the problem list between runs of an unchanged file, which makes a
// diff of two `latem doctor` outputs useless.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
