package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A job may call another workflow of its repository instead of running steps:
// `uses: ./.github/workflows/package.yml` (A82). Such a job is planned as the
// called workflow's jobs, into the caller's own workflow run, each under a job
// path — the caller's id, "/", the called job's id — and named as GitHub
// names it, "release / pack". When one of them runs it is compiled from the
// files at the run's commit, walking the same calls down from the triggered
// workflow. No file from outside that commit is read: a workflow of another
// repository is refused by name (INV-5).

// EventWorkflowCall is the trigger a workflow that can be called declares.
// Nothing is planned for it as an event: a caller plans what it calls.
const EventWorkflowCall = "workflow_call"

// callFilePattern is the file a call may name: directly under
// .github/workflows, as protocol's workflow pattern has it, since GitHub calls
// no other.
var callFilePattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9._-]*\.ya?ml$`)

// remoteCallPattern is a workflow of another repository, owner/repo/path@ref.
var remoteCallPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[^@]+@.+$`)

// localCall is the file a job's uses: calls, when it is a workflow of this
// repository.
func localCall(uses string) (string, error) {
	u := strings.TrimSpace(uses)
	rest, local := strings.CutPrefix(u, "./")
	switch {
	case local && strings.Contains(rest, "@"):
		return "", fmt.Errorf("uses: %s: a workflow of this repository is called without @ref, "+
			"since it is read from the caller's own commit", uses)
	case local && callFilePattern.MatchString(rest):
		return rest, nil
	case local:
		return "", fmt.Errorf("uses: %s is not a file directly under %s, and GitHub calls no other", uses, Dir)
	case remoteCallPattern.MatchString(u):
		return "", fmt.Errorf("uses: calls %s, a workflow in another repository, which latem does not run: "+
			"what it runs would come from outside the commit this run is pinned to (A82, INV-5)", uses)
	}
	return "", fmt.Errorf("uses: %q is neither ./%s/<file> nor owner/repo/%s/<file>@ref", uses, Dir, Dir)
}

// callSpec is what a job that calls a workflow says of the call.
type callSpec struct {
	file string
	// with is the call's with:, each value a string (which may hold
	// expressions), a boolean or a number.
	with map[string]any
	// inherit is `secrets: inherit`.
	inherit bool
	// secrets maps a secret of the called workflow to the caller's
	// expression for it. Nil when the caller passes none.
	secrets map[string]string
}

// readCall reads a job of w that calls a workflow. What GitHub refuses on such
// a job is refused here too, and what it allows and latem does not plan yet
// is refused by name.
func readCall(w *Workflow, key string) (*callSpec, error) {
	j := w.Jobs[key]
	file, err := localCall(j.Uses)
	if err != nil {
		return nil, err
	}
	var theirs []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"runs-on", len(j.RunsOn) > 0},
		{"steps", len(j.Steps) > 0},
		{"env", len(j.Env) > 0},
		{"defaults", j.Defaults != nil},
		{"timeout-minutes", j.TimeoutMins != nil},
		{"container", len(j.Container) > 0},
		{"services", len(j.Services) > 0},
		{"outputs", len(j.Outputs) > 0},
		{"environment", len(j.Environment) > 0},
		{"continue-on-error", len(j.ContinueOnError) > 0},
	} {
		if f.set {
			theirs = append(theirs, f.name)
		}
	}
	if len(theirs) > 0 {
		return nil, fmt.Errorf("a job that calls a workflow has no %s: those belong to the called "+
			"workflow's jobs, and GitHub refuses them here too", strings.Join(theirs, ", "))
	}
	var notYet []string
	if len(j.Strategy) > 0 {
		notYet = append(notYet, "strategy:")
	}
	if len(j.Concurrency) > 0 {
		notYet = append(notYet, "concurrency:")
	}
	if len(notYet) > 0 {
		return nil, fmt.Errorf("a call with %s is not planned yet (A82)", strings.Join(notYet, " or "))
	}

	spec := &callSpec{file: file, with: map[string]any{}}
	if len(j.With) > 0 && string(j.With) != "null" {
		var with map[string]any
		if err := json.Unmarshal(j.With, &with); err != nil {
			return nil, fmt.Errorf("with: expected a map of inputs, got %s", j.With)
		}
		for name, v := range with {
			switch v.(type) {
			case nil:
			case string, bool, float64:
				spec.with[name] = v
			default:
				return nil, fmt.Errorf("with.%s: an input is a string, a boolean or a number, not %s", name, describe(v))
			}
		}
	}
	if len(j.Secrets) > 0 && string(j.Secrets) != "null" {
		var word string
		if json.Unmarshal(j.Secrets, &word) == nil {
			if word != "inherit" {
				return nil, fmt.Errorf("secrets: expected inherit or a map of secrets, got %q", word)
			}
			spec.inherit = true
		} else if err := json.Unmarshal(j.Secrets, &spec.secrets); err != nil {
			return nil, fmt.Errorf("secrets: expected inherit or a map of secrets, got %s", j.Secrets)
		}
	}
	return spec, nil
}

// calledWorkflow is a workflow read as one a job calls: what its
// on.workflow_call declares.
type calledWorkflow struct {
	w       *Workflow
	inputs  map[string]callInput
	secrets map[string]callSecret
	outputs map[string]callOutput
}

type callInput struct {
	Description string          `json:"description"`
	Required    bool            `json:"required"`
	Type        string          `json:"type"`
	Default     json.RawMessage `json:"default"`
}

type callSecret struct {
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type callOutput struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// readCalled reads a called workflow from the caller's commit and checks what
// its on.workflow_call declares: each input's type, each secret's name, and
// that each output reads the jobs of the called workflow and nothing else.
func readCalled(fsys fs.FS, file string) (*calledWorkflow, error) {
	data, err := readCapped(fsys, file, MaxWorkflowBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("uses: there is no %s at this commit", file)
	}
	if err != nil {
		return nil, err
	}
	w, err := Parse(data, file)
	if err != nil {
		return nil, err
	}
	on, err := w.triggers()
	if err != nil {
		return nil, err
	}
	raw, ok := on[EventWorkflowCall]
	if !ok {
		return nil, fmt.Errorf("%s has no on.workflow_call, so no job can call it", file)
	}
	var decl struct {
		Inputs  map[string]callInput  `json:"inputs"`
		Secrets map[string]callSecret `json:"secrets"`
		Outputs map[string]callOutput `json:"outputs"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&decl); err != nil {
			return nil, fmt.Errorf("%s: on.workflow_call: %w", file, err)
		}
	}
	cw := &calledWorkflow{w: w, inputs: decl.Inputs, secrets: decl.Secrets, outputs: decl.Outputs}
	for _, name := range slices.Sorted(maps.Keys(cw.inputs)) {
		in := cw.inputs[name]
		switch in.Type {
		case "string", "boolean", "number":
		case "":
			return nil, fmt.Errorf("%s: on.workflow_call.inputs.%s has no type; it is a string, a boolean or a number",
				file, name)
		default:
			return nil, fmt.Errorf("%s: on.workflow_call.inputs.%s: %q is not a string, a boolean or a number",
				file, name, in.Type)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cw.secrets)) {
		if strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
			return nil, fmt.Errorf("%s: on.workflow_call.secrets.%s: GitHub keeps names beginning GITHUB_ for itself",
				file, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cw.outputs)) {
		if err := cw.checkOutput(cw.outputs[name].Value); err != nil {
			return nil, fmt.Errorf("%s: on.workflow_call.outputs.%s: %w", file, name, err)
		}
	}
	return cw, nil
}

// checkOutput refuses an output that reads anything but the called workflow's
// jobs, or a job it does not have. Outputs are answered for the jobs that need
// the caller, from the results the run carries, and nothing else is there.
func (cw *calledWorkflow) checkOutput(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("has no value")
	}
	spans, err := expr.Spans(value)
	if err != nil {
		return err
	}
	for _, sp := range spans {
		src := strings.TrimSpace(sp.Prog.Source())
		contexts, funcs := sp.Prog.References()
		for _, c := range contexts {
			if c != "jobs" {
				return fmt.Errorf("${{ %s }} reads %s; a called workflow's output reads its jobs, "+
					"${{ jobs.<id>.outputs.<name> }}", src, c)
			}
		}
		for _, f := range funcs {
			if lateFuncs[f] || f == "hashfiles" {
				return fmt.Errorf("${{ %s }} calls %s(), which an output cannot", src, f)
			}
		}
		keys, _ := sp.Prog.ContextKeys("jobs")
		for _, k := range keys {
			if !slices.ContainsFunc(cw.w.JobNames(), func(id string) bool { return strings.EqualFold(id, k) }) {
				return fmt.Errorf("${{ %s }} reads jobs.%s, which is not a job of %s", src, k, cw.w.Path)
			}
		}
	}
	return nil
}

// checkSecrets refuses a call's secrets: that the called workflow does not
// declare, that pass anything but one secret by name, or that leave out one
// it requires.
func (cw *calledWorkflow) checkSecrets(spec *callSpec) error {
	if spec.inherit {
		return nil
	}
	declared := func(name string) bool {
		return slices.ContainsFunc(slices.Collect(maps.Keys(cw.secrets)), func(d string) bool { return strings.EqualFold(d, name) })
	}
	var unknown []string
	for _, name := range slices.Sorted(maps.Keys(spec.secrets)) {
		if !declared(name) {
			unknown = append(unknown, name)
			continue
		}
		if _, err := secretRef(spec.secrets[name]); err != nil {
			return fmt.Errorf("secrets.%s: %w", name, err)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("secrets: %s declares no secret %s", cw.w.Path, strings.Join(unknown, ", "))
	}
	for _, name := range slices.Sorted(maps.Keys(cw.secrets)) {
		passed := slices.ContainsFunc(slices.Collect(maps.Keys(spec.secrets)), func(p string) bool { return strings.EqualFold(p, name) })
		if cw.secrets[name].Required && !passed {
			return fmt.Errorf("secrets: %s requires the secret %s; pass it, or pass secrets: inherit", cw.w.Path, name)
		}
	}
	return nil
}

// secretRefPattern is the one form latem passes a secret in: by name.
var secretRefPattern = regexp.MustCompile(`^\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)

// secretRef is the caller's secret a call passes, from ${{ secrets.NAME }}.
// Anything else — a literal, or an expression over several — is refused: a
// literal would put a secret's value in the compiled job, which is kept, and a
// computed one would be answered from secrets the job is not given.
func secretRef(value string) (string, error) {
	m := secretRefPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return "", fmt.Errorf("%q: latem passes a secret written ${{ secrets.NAME }}, and nothing else (A82)", value)
	}
	return m[1], nil
}

// passSecrets is what each secret of a called workflow reads as (A82), given
// what each of its caller's reads as — nil for the triggered workflow, whose
// every secret is itself. Keys are upper-cased, as secret names do not go by
// case. inherit passes the caller's on as they are; a map passes one each; no
// secrets: passes none, and the result is empty, not nil. GITHUB_TOKEN is not
// passed: every job has its own (A52).
func passSecrets(spec *callSpec, from map[string]string) (map[string]string, error) {
	if spec.inherit {
		return from, nil
	}
	out := make(map[string]string, len(spec.secrets))
	for name, value := range spec.secrets {
		src, err := secretRef(value)
		if err != nil {
			return nil, fmt.Errorf("secrets.%s: %w", name, err)
		}
		if from != nil {
			src = from[strings.ToUpper(src)]
		}
		out[strings.ToUpper(name)] = src
	}
	return out, nil
}

// lateInput stands in a plan's inputs context for an input of a called
// workflow that only a run can answer (A82): its caller's with: reads needs, a
// github property a plan does not have, or an input that is itself one of
// these. planReads refuses to read one, so nothing planned depends on a value
// the plan made up.
type lateInput struct{ from string }

// withContexts are what a call's with: may read, as GitHub allows it.
var withContexts = map[string]bool{
	"github": true, "needs": true, "strategy": true, "matrix": true, "inputs": true, "vars": true,
}

// callInputs answers a call's with: as the called workflow declares its
// inputs, against the caller's scope (A82). An input it does not declare, and
// a required one left out, are refused; a default fills in the rest, and each
// value takes its input's type. planning is the plan asking: a value the plan
// cannot answer is a lateInput, which a run answers.
func callInputs(cw *calledWorkflow, spec *callSpec, sc expr.Scope, planning bool) (map[string]any, error) {
	var unknown []string
	for _, name := range slices.Sorted(maps.Keys(spec.with)) {
		if _, ok := cw.inputs[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("with: %s declares no input %s", cw.w.Path, strings.Join(unknown, ", "))
	}
	out := make(map[string]any, len(cw.inputs))
	for _, name := range slices.Sorted(maps.Keys(cw.inputs)) {
		in := cw.inputs[name]
		v, given := spec.with[name]
		where := "with." + name
		if !given {
			if in.Required {
				return nil, fmt.Errorf("with: %s requires the input %s", cw.w.Path, name)
			}
			v, where = inputDefault(in), cw.w.Path+": on.workflow_call.inputs."+name+".default"
		}
		if s, isString := v.(string); isString && expr.Has(s) {
			late, err := withReads(s, sc)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
			if late && planning {
				out[name] = lateInput{from: s}
				continue
			}
			if v, err = expr.EvalValue(s, sc); err != nil {
				return nil, fmt.Errorf("%s: %w", where, err)
			}
		}
		typed, err := callValue(name, in.Type, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		out[name] = typed
	}
	return out, nil
}

// inputDefault is an input's default, or its type's zero value: GitHub's
// false for a boolean, 0 for a number and "" for a string.
func inputDefault(in callInput) any {
	var v any
	if len(in.Default) > 0 && json.Unmarshal(in.Default, &v) == nil && v != nil {
		return v
	}
	switch in.Type {
	case "boolean":
		return false
	case "number":
		return float64(0)
	}
	return ""
}

// callValue is an input's value as its type has it. A string carrying a
// boolean or a number is read as one, which is how a value an expression
// computed arrives: needs.<id>.outputs are strings.
func callValue(name, typ string, v any) (any, error) {
	switch typ {
	case "string":
		switch t := v.(type) {
		case string:
			return t, nil
		case bool, float64, nil:
			return expr.Stringify(t), nil
		}
	case "boolean":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			switch t {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
		}
	case "number":
		switch t := v.(type) {
		case float64:
			return t, nil
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f, nil
			}
		}
	}
	shown := describe(v)
	switch t := v.(type) {
	case string:
		shown = strconv.Quote(t)
	case bool, float64:
		shown = expr.Stringify(t)
	}
	return nil, fmt.Errorf("the input %s is a %s, and %s is not one", name, typ, shown)
}

// withReads refuses a with: value that reads what a call's with: cannot, and
// reports whether a plan can answer it: one reading needs, a github property
// the plan does not hold, or an input the plan could not answer either, is
// answered by the run.
func withReads(s string, sc expr.Scope) (late bool, err error) {
	spans, err := expr.Spans(s)
	if err != nil {
		return false, err
	}
	github, _ := sc.Contexts["github"].(map[string]any)
	for _, sp := range spans {
		src := strings.TrimSpace(sp.Prog.Source())
		contexts, funcs := sp.Prog.References()
		for _, f := range funcs {
			if lateFuncs[f] || f == "hashfiles" {
				return false, fmt.Errorf("${{ %s }} calls %s(), which a call's with: cannot", src, f)
			}
		}
		for _, c := range contexts {
			if !withContexts[c] {
				return false, fmt.Errorf("${{ %s }} reads %s, which a call's with: cannot: it reads github, "+
					"needs, strategy, matrix, inputs and vars, and a secret goes in the call's own secrets: block", src, c)
			}
			late = late || c == "needs"
		}
		keys, _ := sp.Prog.ContextKeys("github")
		for _, k := range keys {
			if _, ok := github[strings.ToLower(k)]; !ok {
				late = true
			}
		}
		if _, _, ok := readsLateInput(sp.Prog, sc); ok {
			late = true
		}
	}
	return late, nil
}

// readsLateInput reports an input prog reads that sc holds as a lateInput.
func readsLateInput(prog *expr.Program, sc expr.Scope) (string, lateInput, bool) {
	inputs, _ := sc.Contexts["inputs"].(map[string]any)
	keys, computed := prog.ContextKeys("inputs")
	contexts, _ := prog.References()
	whole := computed || (len(keys) == 0 && slices.Contains(contexts, "inputs"))
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		li, isLate := inputs[name].(lateInput)
		if !isLate {
			continue
		}
		if whole || slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, name) }) {
			return name, li, true
		}
	}
	return "", lateInput{}, false
}

// callSite is where a workflow's jobs go in a plan (A82): the triggered
// workflow's at the top, and a called workflow's under the job that calls it.
type callSite struct {
	// prefix starts each job's key, "release/", and name each job's name,
	// "Release / ". Both are empty at the top.
	prefix, name string
	// caller is the calling job's planned key, for messages.
	caller string
	// needs are the caller's needs, as planned keys. Every job of the call
	// waits for them, and carries them, so that the caller's if: and with:,
	// which read them, can be answered on the machine that runs it.
	needs []string
	// checksStatus is set when an if: on the way down calls a status
	// function: then the machine, not the control plane, decides after a
	// need that did not succeed.
	checksStatus bool
	// grant is what the caller's token may do. A called job may ask for
	// less, never more. Nil at the top.
	grant map[string]string
	// reads are the variables the calls on the way down read (A68).
	reads []string
	// files are the workflows calling down to here, the triggered one first.
	files []string
}

// needsOf is a job's needs as planned keys: the caller's, then each job it
// needs, where one that calls a workflow is every job of that call. multi
// reports a need on a call of more than one job: the control plane, which
// skips a job after any need that did not succeed, would skip it after a job of
// the call that skipped while the rest succeeded, which GitHub counts as the
// call succeeding — so the machine decides instead.
func (s *callSite) needsOf(w *Workflow, j *Job, keysOf map[string][]string) (needs []string, multi bool) {
	needs = slices.Clone(s.needs)
	for _, n := range uniqueNeeds(j.Needs) {
		keys := keysOf[n]
		if nj := w.Jobs[n]; nj != nil && nj.Uses != "" && len(keys) > 1 {
			multi = true
		}
		for _, k := range keys {
			if !slices.Contains(needs, k) {
				needs = append(needs, k)
			}
		}
	}
	return needs, multi
}

// place puts a planned job of w where the site says.
func (s *callSite) place(pj *protocol.PlannedJob, w *Workflow, j *Job, keysOf map[string][]string) {
	pj.Key = s.prefix + pj.Key
	pj.Name = s.name + pj.Name
	var multi bool
	pj.Needs, multi = s.needsOf(w, j, keysOf)
	pj.IfChecksStatus = pj.IfChecksStatus || s.checksStatus || multi
	if s.grant != nil {
		if over := exceeds(pj.Permissions, s.grant); len(over) > 0 {
			addPlanError(pj, fmt.Sprintf("permissions: asks for %s, and %s, which calls it, grants less; "+
				"a called workflow's token can only be narrower than its caller's", strings.Join(over, ", "), s.caller))
		}
	}
	if len(s.reads) > 0 {
		pj.ReadsVars = mergeVarReads(pj.ReadsVars, s.reads)
	}
}

func addPlanError(pj *protocol.PlannedJob, msg string) {
	if pj.Error == "" {
		pj.Error = msg
	} else {
		pj.Error += "; " + msg
	}
}

// permissionLevel orders a token's levels; a scope not named has none.
var permissionLevel = map[string]int{"read": 1, "write": 2}

// exceeds names each scope perms asks for at a level grant does not give.
func exceeds(perms, grant map[string]string) []string {
	var over []string
	for _, scope := range slices.Sorted(maps.Keys(perms)) {
		if permissionLevel[perms[scope]] > permissionLevel[grant[scope]] {
			over = append(over, scope+": "+perms[scope])
		}
	}
	return over
}

// mergeVarReads is the union of two lists of variable names (A68).
func mergeVarReads(a, b []string) []string {
	set := map[string]bool{}
	for _, n := range append(slices.Clone(a), b...) {
		set[n] = true
	}
	if set[protocol.AllVars] || len(set) > maxVarReads {
		return []string{protocol.AllVars}
	}
	return slices.Sorted(maps.Keys(set))
}

// workflowJobs plans a workflow's jobs, in order, where site puts them.
func (p *planner) workflowJobs(
	w *Workflow, order []string, sc expr.Scope, perms map[string]string, site *callSite,
) []protocol.PlannedJob {
	keysOf := make(map[string][]string, len(order))
	var out []protocol.PlannedJob
	for _, key := range order {
		j := w.Jobs[key]
		var jobs []protocol.PlannedJob
		if j.Uses != "" {
			jobs = p.call(w, key, sc, perms, site, keysOf)
		} else {
			jobs = p.jobs(w, key, sc, perms)
			for i := range jobs {
				site.place(&jobs[i], w, j, keysOf)
			}
		}
		for _, pj := range jobs {
			if !slices.Contains(keysOf[key], pj.Key) {
				keysOf[key] = append(keysOf[key], pj.Key)
			}
		}
		out = append(out, jobs...)
	}
	return out
}

// call plans a job that calls a workflow (A82): the called workflow's jobs,
// under job paths that start with the caller's, after the jobs the caller
// needs. A call that cannot be planned is one job, under the caller's own
// path, carrying why: it fails alone, and the jobs that need it are skipped.
func (p *planner) call(
	w *Workflow, key string, sc expr.Scope, perms map[string]string, site *callSite, keysOf map[string][]string,
) []protocol.PlannedJob {
	j := w.Jobs[key]
	at := site.prefix + key
	name := key
	var problems []string
	if j.Name != "" {
		if n, err := planString(j.Name, sc); err != nil {
			problems = append(problems, "name: "+err.Error())
		} else if n = strings.TrimSpace(n); n != "" {
			name = n
		}
	}
	needs, multi := site.needsOf(w, j, keysOf)
	checks := site.checksStatus || multi
	if j.If != "" {
		if prog, err := conditionProgram(j.If); err != nil {
			problems = append(problems, "if: "+err.Error())
		} else {
			_, funcs := prog.References()
			checks = checks || slices.ContainsFunc(funcs, func(f string) bool { return lateFuncs[f] })
		}
	}
	grant := perms
	if own, declared, err := planPermissions(j.Permissions); err != nil {
		problems = append(problems, "permissions: "+err.Error())
	} else if declared {
		grant = own
	}
	reads := mergeVarReads(site.reads, placeVarReads(w, j))
	failed := func(why ...string) []protocol.PlannedJob {
		return []protocol.PlannedJob{{
			Key: at, Name: site.name + name, Needs: needs, Permissions: grant, IfChecksStatus: checks,
			TimeoutMinutes: float64(DefaultJobTimeout / minute), ReadsVars: reads, Error: strings.Join(why, "; "),
		}}
	}
	if len(problems) > 0 {
		return failed(problems...)
	}
	if site.grant != nil {
		if over := exceeds(grant, site.grant); len(over) > 0 {
			return failed(fmt.Sprintf("permissions: asks for %s, and %s, which calls it, grants less; "+
				"a called workflow's token can only be narrower than its caller's", strings.Join(over, ", "), site.caller))
		}
	}
	spec, err := readCall(w, key)
	if err != nil {
		return failed(err.Error())
	}
	if p.opts.NoCalls {
		return failed(fmt.Sprintf("uses: calls %s, and the control plane this plan is for does not schedule "+
			"a called workflow's jobs: that takes protocol 2.8 (A82)", spec.file))
	}
	if slices.Contains(site.files, spec.file) {
		return failed(fmt.Sprintf("uses: calls %s, which is already calling down to here (%s)",
			spec.file, strings.Join(append(slices.Clone(site.files), spec.file), " → ")))
	}
	if len(site.files) > protocol.MaxCallDepth {
		return failed(fmt.Sprintf("uses: calls %s, %d calls down from %s; latem plans calls %d deep (A82)",
			spec.file, len(site.files), site.files[0], protocol.MaxCallDepth))
	}
	cw, err := readCalled(p.fsys, spec.file)
	if err != nil {
		return failed(err.Error())
	}
	if err := cw.checkSecrets(spec); err != nil {
		return failed(err.Error())
	}
	if len(cw.w.Concurrency) > 0 {
		return failed(spec.file + ": concurrency: on a called workflow is not planned yet (A82)")
	}
	inputs, err := callInputs(cw, spec, sc, true)
	if err != nil {
		return failed(err.Error())
	}
	calleePerms := grant
	if own, declared, err := planPermissions(cw.w.Permissions); err != nil {
		return failed(spec.file + ": permissions: " + err.Error())
	} else if declared {
		calleePerms = own
	}
	order, err := jobOrder(cw.w)
	if err != nil {
		return failed(err.Error())
	}
	sub := &callSite{
		prefix: at + "/", name: site.name + name + " / ", caller: at,
		needs: needs, checksStatus: checks, grant: grant, reads: reads,
		files: append(slices.Clone(site.files), spec.file),
	}
	return p.workflowJobs(cw.w, order, withContext(sc, "inputs", inputs), calleePerms, sub)
}

// CalledJob finds the job a job path names (A82): the workflow that declares
// it, read from fsys through each caller's uses:, and its id there. A job's
// own id names itself, in w.
func CalledJob(fsys fs.FS, w *Workflow, jobPath string) (*Workflow, string, error) {
	segs := strings.Split(jobPath, "/")
	if len(segs)-1 > protocol.MaxCallDepth {
		return nil, "", badRegistry("%s: %q is more than %d calls deep", w.Path, jobPath, protocol.MaxCallDepth)
	}
	for _, seg := range segs[:len(segs)-1] {
		if err := isCaller(w, seg, jobPath); err != nil {
			return nil, "", err
		}
		spec, err := readCall(w, seg)
		if err != nil {
			return nil, "", badRegistry("%s: job %q: %v", w.Path, seg, err)
		}
		cw, err := readCalled(fsys, spec.file)
		if err != nil {
			return nil, "", badRegistry("%s: job %q: %v", w.Path, seg, err)
		}
		w = cw.w
	}
	leaf := segs[len(segs)-1]
	if w.Jobs[leaf] == nil {
		return nil, "", unknownJob(w, leaf)
	}
	return w, leaf, nil
}

func isCaller(w *Workflow, seg, jobPath string) error {
	j := w.Jobs[seg]
	if j == nil {
		return unknownJob(w, seg)
	}
	if j.Uses == "" {
		return badRegistry("%s: job %q calls no workflow, so %s names no job", w.Path, seg, jobPath)
	}
	return nil
}

func unknownJob(w *Workflow, key string) error {
	return &registry.Error{
		Code: protocol.ErrUnknownJob,
		Msg:  fmt.Sprintf("%s declares no job %q; it has %s", w.Path, key, strings.Join(w.JobNames(), ", ")),
	}
}

// calledJob is a run's job as its job path names it, and what compiling it
// takes from the calls above it (A82).
type calledJob struct {
	w   *Workflow
	key string
	// scope is the job's workflow's: the run's github and vars, the inputs
	// its caller gave it, and its own needs by the ids it needs them by.
	scope expr.Scope
	// secretsFrom is [registry.Job.SecretsFrom].
	secretsFrom map[string]string
	// skipped is set when a caller's if: was false; skipReason is that
	// condition.
	skipped    bool
	skipReason string
}

// resolveCall walks a run's job path down from the triggered workflow, as the
// run starts (A82). Each caller's if: is answered against its own needs, as
// GitHub answers it before the called workflow starts, and a false one skips
// the job; each with: is answered against the caller's scope into the called
// workflow's inputs; each secrets: decides what the level below reads its
// secrets as. sc is the run's, its needs keyed as the run carries them. A job's
// own id is a path of one: nothing is called, and only its needs are read by
// the ids it needs them by.
func resolveCall(fsys fs.FS, w *Workflow, jobPath string, sc expr.Scope, opts Options) (*calledJob, error) {
	raw, _ := sc.Contexts["needs"].(map[string]any)
	if opts.Vars != nil {
		sc = withContext(sc, "vars", stringsToAny(opts.Vars("")))
	}
	segs := strings.Split(jobPath, "/")
	if len(segs)-1 > protocol.MaxCallDepth {
		return nil, badRegistry("%s: %q is more than %d calls deep", w.Path, jobPath, protocol.MaxCallDepth)
	}
	scope, err := typedInputs(w, sc)
	if err != nil {
		return nil, badRegistry("%s: %v", w.Path, err)
	}
	cur := &calledJob{w: w, key: segs[0], scope: scope}
	files := []string{w.Path}
	prefix := ""
	// A caller's false if: skips the job, and the rest of the path is still
	// read, so a path that names no job is an error whatever the ifs say.
	var skip *calledJob
	for i, seg := range segs[:len(segs)-1] {
		if err := isCaller(cur.w, seg, jobPath); err != nil {
			return nil, err
		}
		caller := cur.w.Jobs[seg]
		spec, err := readCall(cur.w, seg)
		if err != nil {
			return nil, badRegistry("%s: job %q: %v", cur.w.Path, seg, err)
		}
		if slices.Contains(files, spec.file) {
			return nil, badRegistry("%s: job %q calls %s, which is already calling down to here", cur.w.Path, seg, spec.file)
		}
		cw, err := readCalled(fsys, spec.file)
		if err != nil {
			return nil, badRegistry("%s: job %q: %v", cur.w.Path, seg, err)
		}
		if err := cw.checkSecrets(spec); err != nil {
			return nil, badRegistry("%s: job %q: %v", cur.w.Path, seg, err)
		}
		files = append(files, spec.file)
		if skip != nil {
			prefix += seg + "/"
			cur = &calledJob{w: cw.w, key: segs[i+1]}
			continue
		}
		needs, err := jobNeeds(fsys, cur.w, seg, prefix, raw)
		if err != nil {
			return nil, err
		}
		callerScope := withContext(cur.scope, "needs", needs)
		if !opts.IgnoreJobIf {
			run, err := jobCondition(caller.If, callerScope)
			if err != nil {
				return nil, badRegistry("%s: job %q: if: %v", cur.w.Path, seg, err)
			}
			if !run {
				reason := strings.TrimSpace(caller.If)
				if reason != "" {
					reason = fmt.Sprintf("%s (on %s, which calls this job)", reason, prefix+seg)
				}
				skip = &calledJob{skipped: true, skipReason: reason}
				prefix += seg + "/"
				cur = &calledJob{w: cw.w, key: segs[i+1]}
				continue
			}
		}
		inputs, err := callInputs(cw, spec, callerScope, false)
		if err != nil {
			return nil, badRegistry("%s: job %q: %v", cur.w.Path, seg, err)
		}
		from, err := passSecrets(spec, cur.secretsFrom)
		if err != nil {
			return nil, badRegistry("%s: job %q: %v", cur.w.Path, seg, err)
		}
		prefix += seg + "/"
		cur = &calledJob{w: cw.w, key: segs[i+1], scope: withContext(sc, "inputs", inputs), secretsFrom: from}
	}
	if cur.w.Jobs[cur.key] == nil {
		return nil, unknownJob(cur.w, cur.key)
	}
	if skip != nil {
		return skip, nil
	}
	needs, err := jobNeeds(fsys, cur.w, cur.key, prefix, raw)
	if err != nil {
		return nil, err
	}
	cur.scope = withContext(cur.scope, "needs", needs)
	if github, ok := cur.scope.Contexts["github"].(map[string]any); ok {
		github = maps.Clone(github)
		github["job"] = cur.key
		cur.scope = withContext(cur.scope, "github", github)
	}
	return cur, nil
}

// jobNeeds is the needs context a job of w reads (A82): each job it needs, by
// the id it needs it by. A run carries its needs by planned key, so a job
// under a call finds its own under the call's prefix, and a job that called a
// workflow is answered from the jobs of that call.
func jobNeeds(fsys fs.FS, w *Workflow, key, prefix string, raw map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, n := range uniqueNeeds(w.Jobs[key].Needs) {
		full := prefix + n
		if v, ok := raw[full]; ok {
			// A job of this workflow — or a call a plan before 2.8 made one
			// job of, which failed and is reported as it was.
			out[n] = v
			continue
		}
		if nj := w.Jobs[n]; nj != nil && nj.Uses != "" {
			v, ok, err := callResult(fsys, w, n, full, raw)
			if err != nil {
				return nil, err
			}
			if ok {
				out[n] = v
			}
		}
	}
	return out, nil
}

// callResult is needs.<id> of a job that called a workflow, from the results
// of the call's jobs that the run carries: failure if one failed, else
// cancelled if one was, else skipped if every one was, else success — A55's
// rule for a matrix's legs — and outputs the called workflow's
// on.workflow_call.outputs, answered against them. ok is false when the run
// carries none of them.
func callResult(fsys fs.FS, w *Workflow, key, full string, raw map[string]any) (map[string]any, bool, error) {
	if strings.Count(full, "/") >= protocol.MaxCallDepth {
		return nil, false, nil
	}
	spec, err := readCall(w, key)
	if err != nil {
		return nil, false, badRegistry("%s: job %q: %v", w.Path, key, err)
	}
	cw, err := readCalled(fsys, spec.file)
	if err != nil {
		return nil, false, badRegistry("%s: job %q: %v", w.Path, key, err)
	}
	jobs := map[string]any{}
	var results []string
	for _, k := range cw.w.JobNames() {
		sub := full + "/" + k
		entry, ok := raw[sub].(map[string]any)
		if !ok && cw.w.Jobs[k].Uses != "" {
			if entry, ok, err = callResult(fsys, cw.w, k, sub, raw); err != nil {
				return nil, false, err
			}
		}
		if !ok {
			continue
		}
		jobs[k] = entry
		r, _ := entry["result"].(string)
		results = append(results, r)
	}
	if len(results) == 0 {
		return nil, false, nil
	}
	sc := expr.Scope{Contexts: map[string]expr.Value{"jobs": jobs}}
	outputs := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(cw.outputs)) {
		v, err := expr.EvalString(cw.outputs[name].Value, sc)
		if err != nil {
			return nil, false, badRegistry("%s: on.workflow_call.outputs.%s: %v", cw.w.Path, name, err)
		}
		outputs[name] = v
	}
	return map[string]any{"result": callOutcome(results), "outputs": outputs}, true, nil
}

// callOutcome is a call's result from its jobs'.
func callOutcome(results []string) string {
	switch {
	case slices.Contains(results, protocol.NeedFailure):
		return protocol.NeedFailure
	case slices.Contains(results, protocol.NeedCancelled):
		return protocol.NeedCancelled
	case !slices.ContainsFunc(results, func(r string) bool { return r != protocol.NeedSkipped }):
		return protocol.NeedSkipped
	}
	return protocol.NeedSuccess
}

// CompileJobPath compiles the job a workflow run's job path names (A82): one
// of w's own, or a job of a workflow w calls, read from fsys through each
// caller's uses:. sc is the run's scope, its needs keyed as the run carries
// them. The job's inputs to admission are w's dispatch's, which are the ones a
// run of it carries, and a called job's secrets are what its callers pass it.
// The workflow returned is the one that declares the job, and the id its own
// there.
func CompileJobPath(
	ctx context.Context, fsys fs.FS, w *Workflow, jobPath string, sc expr.Scope, opts Options,
) (*registry.Job, *Workflow, string, error) {
	called, err := resolveCall(fsys, w, jobPath, sc, opts)
	if err != nil {
		return nil, nil, "", err
	}
	if called.skipped {
		return &registry.Job{Skip: true, SkipReason: called.skipReason}, nil, "", nil
	}
	opts.WorkflowRun = true
	job, err := CompileWith(ctx, called.w, called.key, called.scope, opts)
	if err != nil {
		return nil, nil, "", err
	}
	if called.w != w {
		job.Inputs = dispatchDeclarations(w)
		job.SecretsFrom = called.secretsFrom
	}
	return job, called.w, called.key, nil
}
