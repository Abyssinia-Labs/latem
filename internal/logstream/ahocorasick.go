package logstream

// Aho-Corasick multi-pattern matcher.
//
// Written here rather than taken as a dependency because RUNNER_SPEC §3 asks
// for dependency additions to be justified in review, and this is ~120 lines
// of well-understood algorithm sitting in the hottest path in the system:
// every line of every build passes through it.
//
// The naive alternative — looping strings.Index over N registered secrets per
// line — is O(N × line length). A job declaring a dozen secrets, each expanded
// into six or seven forms by the §16 rules, gives eighty-odd literals scanned
// against every line of a verbose Gradle build. This is one pass regardless of
// how many secrets are registered.

// acNode is one state in the automaton.
type acNode struct {
	children map[byte]int
	fail     int
	// outputs holds the lengths of every pattern ending at this state,
	// including those reachable through fail links. Merging them at build time
	// means matching never has to walk the fail chain to collect results.
	outputs []int
}

type acMatcher struct {
	nodes []acNode
}

// newACMatcher builds an automaton. Patterns shorter than one byte, and
// duplicates, are ignored.
func newACMatcher(patterns []string) *acMatcher {
	m := &acMatcher{nodes: []acNode{{children: map[byte]int{}}}}

	for _, p := range patterns {
		if p == "" {
			continue
		}
		state := 0
		for i := 0; i < len(p); i++ {
			c := p[i]
			child, ok := m.nodes[state].children[c]
			if !ok {
				m.nodes = append(m.nodes, acNode{children: map[byte]int{}})
				child = len(m.nodes) - 1
				m.nodes[state].children[c] = child
			}
			state = child
		}
		m.nodes[state].outputs = appendUnique(m.nodes[state].outputs, len(p))
	}

	m.buildFailLinks()
	return m
}

// buildFailLinks computes suffix links breadth-first, merging each state's
// outputs with those of its fail target.
func (m *acMatcher) buildFailLinks() {
	queue := make([]int, 0, len(m.nodes))

	// Depth-1 states fail to the root.
	for _, child := range m.nodes[0].children {
		m.nodes[child].fail = 0
		queue = append(queue, child)
	}

	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]

		for c, child := range m.nodes[state].children {
			// Follow fail links from the parent until a state has this edge.
			fail := m.nodes[state].fail
			for {
				if next, ok := m.nodes[fail].children[c]; ok && next != child {
					fail = next
					break
				}
				if fail == 0 {
					break
				}
				fail = m.nodes[fail].fail
			}
			m.nodes[child].fail = fail

			for _, l := range m.nodes[fail].outputs {
				m.nodes[child].outputs = appendUnique(m.nodes[child].outputs, l)
			}
			queue = append(queue, child)
		}
	}
}

// next returns the state reached from state on byte c, following fail links.
func (m *acMatcher) next(state int, c byte) int {
	for {
		if child, ok := m.nodes[state].children[c]; ok {
			return child
		}
		if state == 0 {
			return 0
		}
		state = m.nodes[state].fail
	}
}

// findAll reports every occurrence of every pattern, as half-open byte ranges.
// Matches may overlap; resolving that is the caller's job — see [spans.add].
//
// The matcher is read-only once built, so this is safe to call concurrently.
func (m *acMatcher) findAll(s string, emit func(start, end int)) {
	if m == nil || len(m.nodes) <= 1 {
		return
	}
	state := 0
	for i := 0; i < len(s); i++ {
		state = m.next(state, s[i])
		for _, length := range m.nodes[state].outputs {
			emit(i-length+1, i+1)
		}
	}
}

// empty reports whether the automaton holds no patterns.
func (m *acMatcher) empty() bool { return m == nil || len(m.nodes) <= 1 }

func appendUnique(xs []int, v int) []int {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}
