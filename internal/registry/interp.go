package registry

import (
	"fmt"
	"strings"
)

// inputPrefix is the only legal expansion namespace.
const inputPrefix = "input."

// eachExpansion returns the inner text of every ${...} in s.
//
// Scanning by hand rather than by regexp is deliberate: a regexp that only
// matches well-formed expansions would silently pass over a malformed one,
// and "${input.CHANNEL" reaching a shell as a literal is precisely the class
// of surprise this syntax exists to prevent.
func eachExpansion(s string) ([]string, error) {
	var found []string
	for i := 0; i < len(s); {
		rel := strings.Index(s[i:], "${")
		if rel < 0 {
			break
		}
		start := i + rel

		if strings.HasPrefix(s[start:], "${{") {
			return nil, fmt.Errorf(
				"${{ ... }} is not supported; this is not GitHub Actions expression syntax. " +
					"The only expansion is ${input.NAME}, and branching belongs in the shell script")
		}

		rest := s[start+2:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return nil, fmt.Errorf("unterminated ${ in %q", s)
		}
		found = append(found, rest[:end])
		i = start + 2 + end + 1
	}
	return found, nil
}

// rejectInterpolation fails if s contains any expansion at all.
//
// Applied to steps[].run. RUNNER_SPEC §9 confines ${input.NAME} to env values
// and never substitutes into a command, so an expansion appearing in run means
// the author expected something that will not happen. Rejecting is kinder than
// passing a literal "${input.CHANNEL}" to bash.
func rejectInterpolation(s string) error {
	found, err := eachExpansion(s)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf(
			"${%s} is not substituted into run, only into steps[].env — "+
				"declare it in env and reference the variable instead", found[0])
	}
	return nil
}

// validateInterpolation checks every expansion in an env value.
func validateInterpolation(s string, inputs map[string]*Input) error {
	found, err := eachExpansion(s)
	if err != nil {
		return err
	}
	for _, inner := range found {
		name, ok := strings.CutPrefix(inner, inputPrefix)
		if !ok {
			// Secrets land here, which is how "env may not reference secrets"
			// is enforced: it falls out of the syntax rather than needing a
			// rule of its own. Secrets reach the environment by name because
			// they were declared in secrets:, never by substitution.
			return fmt.Errorf(
				"${%s} is not a valid expansion; the only form is ${input.NAME}", inner)
		}
		if !identPattern.MatchString(name) {
			return fmt.Errorf("${%s}: input name %q must match %s", inner, name, identPattern)
		}
		if _, declared := inputs[name]; !declared {
			return fmt.Errorf("${%s}: input %q is not declared by this job", inner, name)
		}
	}
	return nil
}

// substitute replaces every ${input.NAME} with its coerced value.
//
// Called only after [Job.CoerceInputs], and only on env values that already
// passed [validateInterpolation], so every name is known to be declared and
// every value is known to be type-correct. The result goes into an environment
// map — never into a command string, and never through a shell.
func substitute(s string, values map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		rel := strings.Index(s[i:], "${")
		if rel < 0 {
			b.WriteString(s[i:])
			break
		}
		start := i + rel
		b.WriteString(s[i:start])

		rest := s[start+2:]
		end := strings.Index(rest, "}")
		if end < 0 {
			// Unreachable after validation; write it back rather than panic.
			b.WriteString(s[start:])
			break
		}
		name := strings.TrimPrefix(rest[:end], inputPrefix)
		b.WriteString(values[name])
		i = start + 2 + end + 1
	}
	return b.String()
}
