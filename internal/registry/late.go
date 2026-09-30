package registry

import (
	"fmt"
	"sort"
	"strings"
)

// LateValue is an environment value, or a condition, that cannot be known
// until the step is about to run.
//
// It exists for A42 phases 2 and 3 and for nothing in the native format: a
// jobs.yaml author cannot write one, because the fields that hold them are
// excluded from decoding. The compiler in internal/workflow populates them for
// `${{ steps.x.outputs.y }}`, `${{ job.status }}` and `if: always()` — the
// expressions whose answer does not exist at admission.
//
// This is an interface rather than a concrete expression type so that this
// package stays what its doc comment says it is: pure, and about the native
// registry. The dependency runs the other way — internal/workflow implements
// LateValue by wrapping its evaluator — and the run loop only ever speaks
// [StepScope], which is defined here.
//
// The design consequence worth knowing: a late binding is *only ever an
// environment value or a condition*. A command is never late. `run:` is
// compiled at admission and does not change shape afterwards; what a late
// binding changes is the value of a variable the command already references.
// That is A42's rule carried through to run time, and it is why INV-3's
// argv discipline needs no new case here.
type LateValue interface {
	// Resolve computes the value against what is known when the step runs.
	Resolve(StepScope) (string, error)
	lateRefs
}

// Condition is a step's `if:`, answered as the step is about to run.
//
// It is its own interface rather than a LateValue whose string the run loop
// inspects, because truthiness belongs to the expression language: "false"
// the string is truthy and false the value is not, and a loop re-deriving
// that from rendered text would get it wrong for exactly the values a
// condition is most often about.
type Condition interface {
	// Holds reports whether the step runs.
	Holds(StepScope) (bool, error)
	lateRefs
}

// lateRefs is what validation needs from either kind.
type lateRefs interface {
	// References names the step ids this reads, so [Registry.Validate] can
	// refuse a reference to a step that does not exist or comes later —
	// before anything runs, rather than as an empty string eight minutes in.
	References() []string
	// Source is the expression text, for messages that quote it.
	Source() string
}

// StepScope is what the run loop knows at the moment a step is about to run
// and an admission-time scope did not.
type StepScope struct {
	// Status is the job's status so far: success until a step fails or the
	// run is cancelled. It is what `if: failure()` and `${{ job.status }}`
	// answer against.
	Status StepStatus
	// Outputs holds every earlier step's outputs, by step id.
	Outputs map[string]map[string]string
	// Runner describes the machine the run landed on. Chosen at gate 13, which
	// is after the registry was compiled at gate 11, so it is late by
	// construction rather than by nature.
	Runner RunnerFacts
	// ActionsDir is where a fetched action's files are readable from inside
	// the step, which is the executor's answer and therefore late for exactly
	// the same reason the runner is (A42 phase 5).
	ActionsDir string
}

// StepStatus is the job's status as seen from inside it.
type StepStatus int

const (
	StatusSuccess StepStatus = iota
	StatusFailure
	StatusCancelled
)

// RunnerFacts is what an expression may learn about the runner.
type RunnerFacts struct {
	OS   string // "macos" | "linux"
	Arch string // "arm64" | "x64"
	Name string
}

// validateLate checks a step's late bindings and condition against the steps
// that precede it.
//
// Two rules. Keys obey the same grammar as steps[].env, including the LATEM_
// reservation — a late binding is an env value and gets no exemption for
// arriving later. And every referenced step id must belong to an *earlier*
// step: a reference forward, or to nothing, is refused here with the name in
// the message, because the alternative is an empty string at run time that
// looks like the earlier step simply produced no output.
func validateLate(s Step, earlier map[string]bool) []error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	for _, k := range sortedKeys(s.Late) {
		if !identPattern.MatchString(k) {
			add("late env: key %q must match %s", k, identPattern)
		}
		if strings.HasPrefix(k, "LATEM_") {
			add("late env: key %q is reserved — LATEM_* is the agent's namespace", k)
		}
		if _, static := s.Env[k]; static {
			add("late env: key %q is also set statically; one value per key", k)
		}
		for _, p := range refProblems(s.Late[k], earlier) {
			add("late env.%s: %s", k, p)
		}
	}
	if s.If != nil {
		for _, p := range refProblems(s.If, earlier) {
			add("if: %s", p)
		}
	}
	return problems
}

func refProblems(v lateRefs, earlier map[string]bool) []string {
	if v == nil {
		return nil
	}
	var out []string
	refs := v.References()
	sort.Strings(refs)
	for _, id := range refs {
		if !earlier[id] {
			out = append(out, fmt.Sprintf(
				"%q refers to step %q, which is not an earlier step of this job", v.Source(), id))
		}
	}
	return out
}
