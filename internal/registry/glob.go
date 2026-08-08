package registry

import (
	"path"
	"strings"
)

// matchPath reports whether name matches pattern with path-aware globbing.
//
// The semantics, from RUNNER_SPEC §5 (INV-5): a single star matches within one
// segment and does not cross a "/", while a double star matches zero or more
// whole segments.
//
// So release/* matches refs/heads/release/1.2 but not
// refs/heads/release/1.2/hotfix, and release/** matches both. This distinction
// is the difference between "the release branches" and "the release branches
// and anything anyone nests under them", and a policy language that cannot
// express the narrower one is not much of a policy language.
func matchPath(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// Trailing ** matches whatever remains, including nothing.
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}

		if len(name) == 0 {
			return false
		}
		// path.Match on a single segment: the segment contains no "/", so
		// path.Match's own rule that * does not cross a separator is exactly
		// the behaviour wanted here.
		ok, err := path.Match(pattern[0], name[0])
		if err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// validGlob reports whether a pattern compiles. A malformed policy glob is a
// config error caught at load, not a silent never-match at admission — the
// failure mode of the latter is a policy that appears to be in force and is
// not.
func validGlob(pattern string) bool {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}
