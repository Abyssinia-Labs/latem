package logstream

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func matches(m *acMatcher, s string) []span {
	var out []span
	m.findAll(s, func(start, end int) { out = append(out, span{start, end}) })
	sort.Slice(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	return out
}

// brute is the obvious O(N × len) implementation the automaton replaces. It is
// the reference the automaton is checked against.
//
// It deduplicates its pattern list because the automaton does: a value
// registered twice — which happens whenever two of the §16 expansion rules
// produce the same form — must report one match, not two.
func brute(patterns []string, s string) []span {
	var out []span
	seen := map[string]bool{}
	for _, p := range patterns {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				out = append(out, span{i, i + len(p)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	return out
}

func TestACMatcher_Basics(t *testing.T) {
	m := newACMatcher([]string{"he", "she", "his", "hers"})

	got := matches(m, "ushers")
	want := []span{{1, 4}, {2, 4}, {2, 6}} // she, he, hers

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestACMatcher_Empty(t *testing.T) {
	if !newACMatcher(nil).empty() {
		t.Error("a matcher with no patterns should report empty")
	}
	if !newACMatcher([]string{""}).empty() {
		t.Error("an empty pattern should be ignored")
	}
	if newACMatcher([]string{"x"}).empty() {
		t.Error("a matcher with a pattern should not report empty")
	}
	// Matching against an empty automaton must be a no-op, not a panic.
	newACMatcher(nil).findAll("anything", func(int, int) {
		t.Error("empty matcher produced a match")
	})
}

func TestACMatcher_OverlappingAndNested(t *testing.T) {
	m := newACMatcher([]string{"abc", "bcd", "c", "abcd"})
	got := matches(m, "abcd")

	// Every occurrence is reported: the automaton finds them all and span
	// merging in Redact decides what to do about overlaps.
	if len(got) != 4 {
		t.Errorf("got %v, want abc, c, abcd and bcd", got)
	}
}

func TestACMatcher_PatternIsSuffixOfAnother(t *testing.T) {
	// The classic fail-link case: matching "ers" must still be reported when
	// the automaton is deep inside "hers".
	m := newACMatcher([]string{"hers", "ers", "rs"})
	got := matches(m, "hers")
	if len(got) != 3 {
		t.Errorf("got %v, want all three suffixes", got)
	}
}

func TestACMatcher_DuplicatePatterns(t *testing.T) {
	m := newACMatcher([]string{"abc", "abc", "abc"})
	if got := matches(m, "abc"); len(got) != 1 {
		t.Errorf("duplicate patterns produced %d matches, want 1", len(got))
	}
}

// The automaton is a performance optimisation over the obvious loop, so it has
// to agree with the obvious loop on everything.
func TestACMatcher_AgreesWithBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := "abc"

	randStr := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}

	for trial := 0; trial < 300; trial++ {
		n := 1 + rng.Intn(5)
		patterns := make([]string, n)
		for i := range patterns {
			patterns[i] = randStr(1 + rng.Intn(4))
		}
		text := randStr(rng.Intn(40))

		got := matches(newACMatcher(patterns), text)
		want := brute(patterns, text)

		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("trial %d disagreed\n  patterns: %q\n  text:     %q\n  got:      %v\n  want:     %v",
				trial, patterns, text, got, want)
		}
	}
}

func TestACMatcher_BinarySafe(t *testing.T) {
	// Log lines are bytes, not necessarily valid UTF-8 — a build can emit
	// anything into stdout.
	m := newACMatcher([]string{"\x00\xff\xfe"})
	if got := matches(m, "prefix\x00\xff\xfesuffix"); len(got) != 1 {
		t.Errorf("got %v, want one match on a binary pattern", got)
	}
}
