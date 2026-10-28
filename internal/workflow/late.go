package workflow

import (
	"fmt"
	"sort"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// lateExpr is a compiled expression that waits for the step it belongs to.
//
// It implements [registry.LateValue]. The admission-time scope — github,
// inputs, env, vars — is captured at compile time; what a [registry.StepScope]
// adds at run time is layered on top: `steps`, `job`, and `runner`. Nothing
// the admission scope already answered can change, so the two never conflict.
type lateExpr struct {
	prog *expr.Program
	base expr.Scope
	refs []string
}

// newLate binds a program to the admission scope and records which step ids
// it reads, for the registry's ordering check.
func newLate(prog *expr.Program, base expr.Scope) *lateExpr {
	return &lateExpr{prog: prog, base: base, refs: stepRefs(prog)}
}

// Resolve evaluates against the admission scope plus what the run loop knows.
func (l *lateExpr) Resolve(sc registry.StepScope) (string, error) {
	v, err := l.prog.Eval(l.scope(sc))
	if err != nil {
		return "", fmt.Errorf("${{ %s }}: %w", l.prog.Source(), err)
	}
	return expr.Stringify(v), nil
}

// Holds is Resolve for a condition, keeping the expression language's own
// truthiness rather than re-deriving it from a rendered string — "false" the
// string is truthy, false the value is not. It is what makes *lateExpr a
// [registry.Condition] as well as a [registry.LateValue].
func (l *lateExpr) Holds(sc registry.StepScope) (bool, error) {
	v, err := l.prog.Eval(l.scope(sc))
	if err != nil {
		return false, fmt.Errorf("${{ %s }}: %w", l.prog.Source(), err)
	}
	return expr.IsTruthy(v), nil
}

func (l *lateExpr) References() []string { return l.refs }
func (l *lateExpr) Source() string       { return l.prog.Source() }

func (l *lateExpr) scope(sc registry.StepScope) expr.Scope { return lateScope(l.base, sc) }

// lateTemplate is a late env value that mixes text with expressions:
// `Bearer ${{ secrets.TOKEN }}`. The whole value is evaluated as the step is
// about to run, against the same scope a [lateExpr] gets, and is still only
// ever an environment value — never command text (A42, A50).
type lateTemplate struct {
	src   string
	progs []*expr.Program
	base  expr.Scope
	refs  []string
}

func newLateTemplate(src string, spans []expr.Span, base expr.Scope) *lateTemplate {
	t := &lateTemplate{src: src, base: base}
	set := map[string]bool{}
	for _, sp := range spans {
		t.progs = append(t.progs, sp.Prog)
		for _, id := range stepRefs(sp.Prog) {
			set[id] = true
		}
	}
	for id := range set {
		t.refs = append(t.refs, id)
	}
	sort.Strings(t.refs)
	return t
}

// Resolve renders the template against the admission scope plus what the run
// loop knows.
func (t *lateTemplate) Resolve(sc registry.StepScope) (string, error) {
	out, err := expr.EvalString(t.src, lateScope(t.base, sc))
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.src, err)
	}
	return out, nil
}

func (t *lateTemplate) References() []string { return t.refs }
func (t *lateTemplate) Source() string       { return t.src }

// lateScope layers run-time facts over the admission scope.
//
// Status maps onto both the status functions and `job.status`, so that
// `if: failure()` and `if: job.status == 'failure'` — which workflows use
// interchangeably — agree with each other.
//
// secrets holds the values this run resolved for the names the job reads
// (A50). A name that resolved nowhere is absent, so it reads as null and
// renders as the empty string, which is GitHub's behaviour.
func lateScope(base expr.Scope, sc registry.StepScope) expr.Scope {
	out := expr.Scope{
		Contexts:     make(map[string]expr.Value, len(base.Contexts)+4),
		WorkspaceDir: base.WorkspaceDir,
	}
	for k, v := range base.Contexts {
		out.Contexts[k] = v
	}
	secrets := make(map[string]any, len(sc.Secrets))
	for k, v := range sc.Secrets {
		secrets[k] = v
	}
	out.Contexts["secrets"] = secrets

	switch sc.Status {
	case registry.StatusFailure:
		out.Status = expr.StatusFailure
	case registry.StatusCancelled:
		out.Status = expr.StatusCancelled
	default:
		out.Status = expr.StatusSuccess
	}
	out.Contexts["job"] = map[string]any{"status": statusName(sc.Status)}

	// GitHub's steps context: every earlier step with an id, with its outputs,
	// outcome and conclusion. The outcome is what happened and the conclusion
	// what counts after continue-on-error (A50); a skipped step is present as
	// skipped, with no outputs.
	steps := make(map[string]any, len(sc.Results)+len(sc.Outputs))
	entry := func(id string) map[string]any {
		if e, ok := steps[id].(map[string]any); ok {
			return e
		}
		e := map[string]any{"outputs": map[string]any{}, "outcome": "success", "conclusion": "success"}
		steps[id] = e
		return e
	}
	for id, r := range sc.Results {
		e := entry(id)
		e["outcome"], e["conclusion"] = r.Outcome, r.Conclusion
	}
	for id, outputs := range sc.Outputs {
		o := make(map[string]any, len(outputs))
		for k, v := range outputs {
			o[k] = v
		}
		entry(id)["outputs"] = o
	}
	out.Contexts["steps"] = steps

	out.Contexts["latem"] = map[string]any{"action_dir": sc.ActionsDir}
	out.Contexts["runner"] = map[string]any{
		"os":   RunnerOS(sc.Runner.OS),
		"arch": RunnerArch(sc.Runner.Arch),
		"name": sc.Runner.Name,
		"temp": sc.TempDir,
	}
	return out
}

func statusName(s registry.StepStatus) string {
	switch s {
	case registry.StatusFailure:
		return "failure"
	case registry.StatusCancelled:
		return "cancelled"
	}
	return "success"
}

// RunnerOS and RunnerArch render in GitHub's spelling, which is what a
// workflow compares against: `runner.os == 'macOS'`, `runner.arch == 'ARM64'`.
// The comparison is case-insensitive in the expression language, so this is
// for the value a step *reads*, not for the test to pass. The run loop uses
// them for $RUNNER_OS and $RUNNER_ARCH too (A50), so the variable and the
// context cannot disagree.
func RunnerOS(os string) string {
	switch os {
	case "macos", "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return os
}

func RunnerArch(arch string) string {
	switch arch {
	case "arm64":
		return "ARM64"
	case "amd64", "x64":
		return "X64"
	}
	return arch
}

// stepRefs lists the step ids an expression reads through the steps context:
// `steps.<id>.…`. Only a literal id counts. `steps[expr]` with a computed
// key cannot be checked at admission and is left to resolve to null, which is
// what GitHub does with it too.
func stepRefs(prog *expr.Program) []string {
	set := map[string]bool{}
	for _, id := range prog.StepReferences() {
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
