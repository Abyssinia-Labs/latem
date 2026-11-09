package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ResolveWorkflowJob compiles one job of one workflow file, for a workflow
// run's job (A52).
//
// A run that names its workflow names a file and a job key in it, so there is
// no search and no ambiguity: `web` in checks.yml and `web` in
// deploy-staging.yml are two jobs. .latem/jobs.yaml is not read — the event
// planned a workflow, and a native job of the same name is a different job.
//
// The scope must carry the needs context the run arrived with. A job whose
// if: is false comes back with [registry.Job.Skip] set and is not validated,
// because none of it runs. jobKey may be a job path, a job of a workflow the
// file calls (A82): [CompileJobPath].
func ResolveWorkflowJob(
	ctx context.Context, workspace, file, jobKey string, sc expr.Scope, opts Options,
) (*registry.Registry, Source, error) {
	if !IsWorkflowPath(file) {
		return nil, Source{}, badRegistry("%q is not a workflow file", file)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, Source{}, &registry.Error{Code: protocol.ErrNoRegistry, Msg: "opening the workspace", Err: err}
	}
	defer func() { _ = root.Close() }()

	data, err := readCapped(root.FS(), file, MaxWorkflowBytes)
	if err != nil {
		return nil, Source{}, &registry.Error{
			Code: protocol.ErrNoRegistry,
			Msg:  fmt.Sprintf("reading %s at this commit", file),
			Err:  err,
		}
	}
	w, err := Parse(data, file)
	if err != nil {
		return nil, Source{}, err
	}
	name := w.Name
	if name == "" {
		name = file
	}
	src := Source{Path: file, Workflow: name}

	opts.Actions = rootLoader{root: root}
	job, declaredIn, key, err := CompileJobPath(ctx, root.FS(), w, jobKey, sc, opts)
	if err != nil {
		return nil, Source{}, err
	}
	reg := RegistryFor(jobKey, job)
	if job.Skip {
		return reg, src, nil
	}
	if declaredIn != w {
		src.JobFile, src.Job = declaredIn.Path, key
	}
	// A job path is no registry job's name, and the registry's name rule is
	// jobs.yaml's; the job is checked under its own id in its own file, and
	// admitted under the path the run names it by.
	if err := RegistryFor(key, job).Validate(); err != nil {
		return nil, Source{}, &registry.Error{
			Code: protocol.ErrBadRegistry,
			Msg:  fmt.Sprintf("%s: job %q compiled to a registry job that does not validate", file, jobKey),
			Err:  err,
		}
	}
	return reg, src, nil
}

// NeedsContext is the needs context a workflow run's job reads: each needed
// job's result and outputs (A52).
func NeedsContext(needs map[string]protocol.NeedResult) map[string]any {
	out := make(map[string]any, len(needs))
	for key, n := range needs {
		outputs := make(map[string]any, len(n.Outputs))
		for k, v := range n.Outputs {
			outputs[k] = v
		}
		out[key] = map[string]any{"result": n.Result, "outputs": outputs}
	}
	return out
}

// jobContexts are what a job's if: may read, as GitHub allows it.
var jobContexts = map[string]bool{"github": true, "needs": true, "vars": true, "inputs": true}

// jobCondition answers a workflow run's job's if: as the run starts (A52).
//
// The status functions answer against its needs: success() when every needed
// job succeeded, failure() when one failed, cancelled() when one was
// cancelled. A condition that calls none of them is `success() && (…)`, as on
// GitHub — the control plane has already skipped such a job after a failed
// need, and this keeps the machine's answer the same if it did not.
// VarsStopJob reports whether a job's if: reads the vars context and is false
// against sc, with the repository's variables in it: a job this machine's
// variables keep off it, whatever else it needs. An if: that cannot be read
// stops nothing here, because the compile that reaches it will say why.
func VarsStopJob(w *Workflow, jobName string, sc expr.Scope, vars func(string) map[string]string) bool {
	declared, ok := w.Jobs[jobName]
	if !ok || strings.TrimSpace(declared.If) == "" {
		return false
	}
	prog, err := conditionProgram(declared.If)
	if err != nil {
		return false
	}
	if contexts, _ := prog.References(); !slices.Contains(contexts, "vars") {
		return false
	}
	if vars != nil {
		sc = withContext(sc, "vars", stringsToAny(vars("")))
	}
	if sc, err = typedInputs(w, sc); err != nil {
		return false
	}
	run, err := jobCondition(declared.If, sc)
	return err == nil && !run
}

func jobCondition(cond string, sc expr.Scope) (bool, error) {
	status := needsStatus(sc)
	if strings.TrimSpace(cond) == "" {
		return status == expr.StatusSuccess, nil
	}
	prog, err := conditionProgram(cond)
	if err != nil {
		return false, err
	}
	contexts, funcs := prog.References()
	for _, c := range contexts {
		if !jobContexts[c] {
			return false, fmt.Errorf("${{ %s }} reads %s, which a job's if: cannot — it is answered before any step runs",
				strings.TrimSpace(prog.Source()), c)
		}
	}
	sc.Status = status
	v, err := prog.Eval(sc)
	if err != nil {
		return false, err
	}
	if !slices.ContainsFunc(funcs, func(f string) bool { return lateFuncs[f] }) {
		return status == expr.StatusSuccess && expr.IsTruthy(v), nil
	}
	return expr.IsTruthy(v), nil
}

// needsStatus is what the status functions answer for a job, from the
// results of the jobs it needs. A failure outweighs a cancellation, which
// outweighs a skip.
func needsStatus(sc expr.Scope) expr.Status {
	needs, _ := sc.Contexts["needs"].(map[string]any)
	status := expr.StatusSuccess
	for _, n := range needs {
		entry, _ := n.(map[string]any)
		switch entry["result"] {
		case protocol.NeedFailure:
			return expr.StatusFailure
		case protocol.NeedCancelled:
			status = expr.StatusCancelled
		case protocol.NeedSkipped:
			if status == expr.StatusSuccess {
				status = expr.StatusSkipped
			}
		}
	}
	return status
}

// compileOutputs compiles a job's outputs: — name to value — to values
// answered as the job ends, against its steps (A52).
func compileOutputs(raw json.RawMessage, sc expr.Scope) (map[string]registry.LateValue, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var decl map[string]string
	if err := json.Unmarshal(raw, &decl); err != nil {
		return nil, fmt.Errorf("expected a map of names to values, got %s", raw)
	}
	out := make(map[string]registry.LateValue, len(decl))
	for _, name := range slices.Sorted(maps.Keys(decl)) {
		v := decl[name]
		spans, err := expr.Spans(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, sp := range spans {
			if _, err := classify(sp.Prog, sc); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		out[name] = newLateTemplate(v, spans, sc)
	}
	return out, nil
}

// outputSecrets lists the secrets a job's outputs read, so the run resolves
// them — and the redactor knows them, which is what drops such an output.
func outputSecrets(outputs map[string]registry.LateValue) []string {
	var steps []registry.Step
	for _, v := range outputs {
		steps = append(steps, registry.Step{Late: map[string]registry.LateValue{"_": v}})
	}
	return secretNames(steps)
}

func mergeNames(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	set := map[string]bool{}
	for _, n := range append(slices.Clone(a), b...) {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// dispatchDeclarations declares a workflow_dispatch's inputs to admission
// (gate 14), as strings: the plan has already checked them against the
// workflow's own types and filled in its defaults, and every job of the run
// is given them (A52). Nil for a workflow that is not dispatched.
func dispatchDeclarations(w *Workflow) map[string]*registry.Input {
	on, err := w.triggers()
	if err != nil {
		return nil
	}
	raw, ok := on[EventWorkflowDispatch]
	if !ok {
		return nil
	}
	t, err := decodeTrigger(EventWorkflowDispatch, raw)
	if err != nil || len(t.Inputs) == 0 {
		return nil
	}
	out := make(map[string]*registry.Input, len(t.Inputs))
	for name := range t.Inputs {
		out[name] = &registry.Input{Type: registry.InputString}
	}
	return out
}

// typedInputs gives a workflow_dispatch's inputs the types the workflow
// declares — a boolean input is a boolean in the inputs context, a number a
// number — leaving every other input as the string it arrived as.
func typedInputs(w *Workflow, sc expr.Scope) (expr.Scope, error) {
	on, err := w.triggers()
	if err != nil {
		return sc, nil // the plan reported it; a job is not the place to again
	}
	raw, ok := on[EventWorkflowDispatch]
	if !ok {
		return sc, nil
	}
	t, err := decodeTrigger(EventWorkflowDispatch, raw)
	if err != nil {
		return sc, fmt.Errorf("on.workflow_dispatch: %w", err)
	}
	given, _ := sc.Contexts["inputs"].(map[string]any)
	typed := maps.Clone(given)
	if typed == nil {
		typed = map[string]any{}
	}
	for name, in := range t.Inputs {
		s, isString := given[name].(string)
		if !isString {
			continue
		}
		switch in.Type {
		case "boolean":
			typed[name] = s == "true"
		case "number":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				typed[name] = f
			}
		}
	}
	return withContext(sc, "inputs", typed), nil
}
