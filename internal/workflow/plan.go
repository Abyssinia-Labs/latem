package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// GitHub's public endpoints. A job compiled from a workflow gets these
// whatever host its repository is on: they are what github.server_url and the
// API clients a workflow calls default to, and latem has no GitHub Enterprise
// host to substitute (A50).
const (
	GitHubServerURL  = "https://github.com"
	GitHubAPIURL     = "https://api.github.com"
	GitHubGraphQLURL = "https://api.github.com/graphql"
)

// The events a plan is made for (A52). schedule, and every other event
// GitHub has, is refused by name rather than planned to nothing.
const (
	EventPush               = "push"
	EventPullRequest        = "pull_request"
	EventPullRequestTarget  = "pull_request_target"
	EventRelease            = "release"
	EventRepositoryDispatch = "repository_dispatch"
	EventWorkflowDispatch   = "workflow_dispatch"
)

// PlanOptions is what planning needs from the machine besides the commit.
type PlanOptions struct {
	// Repository is owner/name, for github.repository.
	Repository string
	// LabelAliases is the machine's [label_aliases] (A50).
	LabelAliases map[string][]string
	// Vars are the repository's (A50). An environment's are not read: the
	// plan names a job's environment, and nothing in it is evaluated inside
	// one.
	Vars map[string]string
	// NoCalls plans a job that calls a workflow as one job that cannot run,
	// saying why, for a control plane older than 2.8 (A81): it would refuse
	// a plan whose keys are job paths, and the whole plan with them.
	NoCalls bool
	// Changed lists the paths the event changed, computed in the mirror: a
	// pull request's three-dot diff, a push's two-dot one. It is called at
	// most once, and only for a workflow with a paths filter. Nil means
	// there is no diff, and then a paths filter is an error, not a guess.
	Changed func() ([]string, error)
}

// PlanEvent answers which workflows at a commit an event triggers, and what
// each one's jobs are (A52).
//
// fsys is the commit's tree, and only .github/workflows is read from it. The
// error is for a request that cannot be planned at all: an event latem does
// not plan, or a dispatch its workflow refuses — GitHub refuses those before
// anything starts, too. A workflow that cannot be read, parsed or planned is
// in the plan with its error.
func PlanEvent(fsys fs.FS, ev protocol.Event, opts PlanOptions) ([]protocol.PlannedWorkflow, error) {
	if err := checkEvent(ev); err != nil {
		return nil, err
	}
	payload := map[string]any{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return nil, fmt.Errorf("%s: the event's payload is not a JSON object: %w", ev.Name, err)
		}
	}
	files, err := workflowFiles(fsys)
	if err != nil {
		return nil, err
	}
	if ev.Name == EventWorkflowDispatch {
		if !slices.Contains(files, ev.Workflow) {
			return nil, fmt.Errorf("there is no workflow %s at %s", ev.Workflow, ev.SHA)
		}
		files = []string{ev.Workflow}
	}
	p := &planner{fsys: fsys, ev: ev, payload: payload, opts: opts}
	plan := []protocol.PlannedWorkflow{}
	for _, f := range files {
		pw, triggered, err := p.workflow(f)
		if err != nil {
			return nil, err
		}
		if triggered {
			plan = append(plan, pw)
		}
	}
	return plan, nil
}

// PlanFile plans one workflow file for an event whether or not its on:
// matches the event. It is what `latem try` asks (A76): the person names the
// job they want run, and whether this event would have run it is the question
// they are not asking. A dispatch is still a dispatch: the file must declare
// workflow_dispatch, and the inputs are validated against it. A file that
// cannot be read, parsed or planned is an error rather than a planned failure,
// because there is nobody downstream to report it to.
func PlanFile(fsys fs.FS, file string, ev protocol.Event, opts PlanOptions) (protocol.PlannedWorkflow, error) {
	if err := checkEvent(ev); err != nil {
		return protocol.PlannedWorkflow{}, err
	}
	if ev.Name == EventWorkflowDispatch && ev.Workflow != file {
		return protocol.PlannedWorkflow{}, fmt.Errorf("the dispatch names %s, not %s", ev.Workflow, file)
	}
	payload := map[string]any{}
	if len(ev.Payload) > 0 {
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return protocol.PlannedWorkflow{}, fmt.Errorf("%s: the event's payload is not a JSON object: %w", ev.Name, err)
		}
	}
	p := &planner{fsys: fsys, ev: ev, payload: payload, opts: opts, anyTrigger: true}
	pw, _, err := p.workflow(file)
	if err == nil && pw.Error != "" {
		err = errors.New(pw.Error)
	}
	return pw, err
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// checkEvent refuses an event the planner cannot read, before any file is.
func checkEvent(ev protocol.Event) error {
	switch ev.Name {
	case EventPush, EventPullRequest, EventPullRequestTarget, EventRelease,
		EventRepositoryDispatch, EventWorkflowDispatch:
	case "schedule":
		return errors.New("schedule events are not planned yet (A52)")
	default:
		return fmt.Errorf("latem does not plan %q events (A52)", ev.Name)
	}
	if !shaPattern.MatchString(ev.SHA) {
		return fmt.Errorf("%s: %q is not a commit id", ev.Name, ev.SHA)
	}
	switch ev.Name {
	case EventPush, EventWorkflowDispatch:
		name, kind := refNameAndType(ev.Ref)
		if kind == "" || name == "" {
			return fmt.Errorf("%s: %q is neither a branch nor a tag", ev.Name, ev.Ref)
		}
	case EventPullRequest, EventPullRequestTarget:
		if ev.BaseRef == "" || ev.Action == "" {
			return fmt.Errorf("%s: a pull request event needs its action and its base branch", ev.Name)
		}
	case EventRelease, EventRepositoryDispatch:
		if ev.Action == "" {
			return fmt.Errorf("%s: the event needs its action", ev.Name)
		}
	}
	if (ev.Name == EventWorkflowDispatch) != (ev.Workflow != "") {
		return fmt.Errorf("%s: only a workflow_dispatch names a workflow, and it has to", ev.Name)
	}
	if ev.Workflow != "" && !IsWorkflowPath(ev.Workflow) {
		return fmt.Errorf("%s: %q is not a file under %s", ev.Name, ev.Workflow, Dir)
	}
	if ev.Name != EventWorkflowDispatch && len(ev.Inputs) > 0 {
		return fmt.Errorf("%s: only a workflow_dispatch has inputs", ev.Name)
	}
	return nil
}

// refNameAndType is github.ref_name and github.ref_type for a ref. A pull
// request's merge ref is named as GitHub names it, "7/merge", and is a branch.
func refNameAndType(ref string) (name, kind string) {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/"), "branch"
	case strings.HasPrefix(ref, "refs/tags/"):
		return strings.TrimPrefix(ref, "refs/tags/"), "tag"
	case strings.HasPrefix(ref, "refs/pull/"):
		return strings.TrimPrefix(ref, "refs/pull/"), "branch"
	}
	return ref, ""
}

type planner struct {
	fsys    fs.FS
	ev      protocol.Event
	payload map[string]any
	opts    PlanOptions
	// anyTrigger plans a file whatever its on: says (PlanFile).
	anyTrigger bool

	diffDone bool
	diff     []string
	diffErr  error
}

// workflow plans one file, reporting whether the event triggers it.
func (p *planner) workflow(file string) (protocol.PlannedWorkflow, bool, error) {
	pw := protocol.PlannedWorkflow{File: file, Name: file}
	dispatch := p.ev.Name == EventWorkflowDispatch
	broken := func(err error) (protocol.PlannedWorkflow, bool, error) {
		if dispatch {
			return pw, false, err // whoever dispatched it hears why, now
		}
		pw.Error = err.Error()
		return pw, true, nil
	}

	data, err := readCapped(p.fsys, file, MaxWorkflowBytes)
	if err != nil {
		return broken(err)
	}
	w, err := Parse(data, file)
	if err != nil {
		return broken(err)
	}
	if w.Name != "" {
		pw.Name = w.Name
	}
	on, err := w.triggers()
	if err != nil {
		return broken(err)
	}
	raw, ok := on[p.ev.Name]
	if !ok {
		if dispatch {
			return pw, false, fmt.Errorf("%s has no workflow_dispatch trigger", file)
		}
		if !p.anyTrigger {
			return pw, false, nil
		}
	}
	t := &trigger{}
	if ok {
		if t, err = decodeTrigger(p.ev.Name, raw); err != nil {
			return broken(fmt.Errorf("%s: on.%s: %w", file, p.ev.Name, err))
		}
	}
	if !p.anyTrigger {
		match, err := p.matches(t)
		if err != nil {
			return broken(fmt.Errorf("%s: on.%s: %w", file, p.ev.Name, err))
		}
		if !match {
			return pw, false, nil
		}
	}

	inputs := map[string]any{}
	if dispatch {
		pw.Inputs, inputs, err = dispatchInputs(t.Inputs, p.ev.Inputs)
		if err != nil {
			return pw, false, fmt.Errorf("%s: %w", file, err)
		}
	}
	sc := p.scope(pw.Name, inputs, pw.Inputs)

	if pw.RunName, err = planString(w.RunName, sc); err != nil {
		return broken(fmt.Errorf("%s: run-name: %w", file, err))
	}
	if pw.Concurrency, err = planConcurrency(w.Concurrency, sc); err != nil {
		return broken(fmt.Errorf("%s: concurrency: %w", file, err))
	}
	perms, declared, err := planPermissions(w.Permissions)
	if err != nil {
		return broken(fmt.Errorf("%s: permissions: %w", file, err))
	}
	if !declared {
		perms = maps.Clone(restrictedPermissions)
	}
	order, err := jobOrder(w)
	if err != nil {
		return broken(err)
	}
	pw.Jobs = p.workflowJobs(w, order, sc, perms, &callSite{files: []string{file}})
	return pw, true, nil
}

// scope is what a plan's expressions read: github, as far as the event
// answers it, inputs and vars.
func (p *planner) scope(workflow string, inputs map[string]any, given map[string]string) expr.Scope {
	refName, refType := refNameAndType(p.ev.Ref)
	owner, _, _ := strings.Cut(p.opts.Repository, "/")
	event := maps.Clone(p.payload)
	if p.ev.Name == EventWorkflowDispatch {
		// github.event.inputs holds them as strings, the inputs context typed.
		event["inputs"] = stringsToAny(given)
	}
	baseRef, headRef := "", ""
	if p.ev.Name == EventPullRequest || p.ev.Name == EventPullRequestTarget {
		baseRef, headRef = p.ev.BaseRef, p.ev.HeadRef
	}
	return expr.Scope{
		Contexts: map[string]expr.Value{
			"github": map[string]any{
				"event_name":       p.ev.Name,
				"event":            event,
				"ref":              p.ev.Ref,
				"ref_name":         refName,
				"ref_type":         refType,
				"sha":              p.ev.SHA,
				"base_ref":         baseRef,
				"head_ref":         headRef,
				"repository":       p.opts.Repository,
				"repository_owner": owner,
				"workflow":         workflow,
				"actor":            p.ev.Actor,
				"triggering_actor": p.ev.Actor,
				"server_url":       GitHubServerURL,
				"api_url":          GitHubAPIURL,
				"graphql_url":      GitHubGraphQLURL,
			},
			"inputs": inputs,
			"vars":   stringsToAny(p.opts.Vars),
		},
		Status: expr.StatusSuccess,
	}
}

// jobs plans one job: one planned job, or one per leg of its matrix (A55).
// A matrix that cannot be expanded is one planned job carrying the error.
func (p *planner) jobs(w *Workflow, key string, sc expr.Scope, perms map[string]string) []protocol.PlannedJob {
	st, err := expandStrategy(w.Jobs[key], sc)
	if err != nil {
		pj := p.job(w, key, legScope(sc, nil, 0), perms, nil, 0)
		pj.Labels, pj.Error = nil, err.Error()
		return []protocol.PlannedJob{pj}
	}
	if st == nil {
		return []protocol.PlannedJob{p.job(w, key, legScope(sc, nil, 0), perms, nil, 0)}
	}
	out := make([]protocol.PlannedJob, 0, len(st.legs))
	for i := range st.legs {
		out = append(out, p.job(w, key, legScope(sc, st, i), perms, st, i))
	}
	return out
}

// job plans one job, or one leg of a matrix job when st is set; a job that
// calls a workflow is [planner.call]'s. What is wrong
// with it is collected rather than stopped at, for the reason
// [Job.unsupported] gives.
func (p *planner) job(
	w *Workflow, key string, sc expr.Scope, perms map[string]string, st *matrixStrategy, leg int,
) (pj protocol.PlannedJob) {
	j := w.Jobs[key]
	pj = protocol.PlannedJob{
		Key:            key,
		Name:           key,
		Needs:          uniqueNeeds(j.Needs),
		Permissions:    perms,
		TimeoutMinutes: float64(DefaultJobTimeout / minute),
	}
	var problems []string
	fail := func(what string, err error) { problems = append(problems, what+": "+err.Error()) }
	defer func() {
		if len(problems) > 0 {
			pj.Error = strings.Join(problems, "; ")
		}
	}()

	if j.Name != "" {
		name, err := planString(j.Name, sc)
		if err != nil {
			fail("name", err)
		} else if name = strings.TrimSpace(name); name != "" {
			pj.Name = name
		}
	}
	if st != nil {
		// GitHub names a leg by its values unless the name already reads
		// them, and the check run, and so branch protection, goes by it.
		if !nameReadsMatrix(j.Name) {
			pj.Name += legSuffix(st.legs[leg])
		}
		m, err := legMatrix(st.legs[leg])
		if err != nil {
			fail("strategy.matrix", err)
		}
		pj.Matrix = m
		pj.Strategy = &protocol.Strategy{
			JobIndex: leg, JobTotal: len(st.legs), FailFast: st.failFast, MaxParallel: st.maxParallel,
		}
	}
	pj.ReadsVars = placeVarReads(w, j)
	if labels, err := compileLabels(j.RunsOn, w.Path, key, p.opts.LabelAliases, sc); err != nil {
		problems = append(problems, err.Error())
	} else {
		pj.Labels = labels
		// compileLabels answered the same expressions, so this cannot fail.
		written, _ := answeredRunsOn(j.RunsOn, w.Path, key, sc)
		pj.RunsOn = plannedRunsOn(written)
	}
	if name, err := environmentField(j.Environment); err != nil {
		fail("environment", err)
	} else if name, err = planString(name, sc); err != nil {
		fail("environment", err)
	} else {
		pj.Environment = strings.TrimSpace(name)
	}
	if own, declared, err := planPermissions(j.Permissions); err != nil {
		fail("permissions", err)
	} else if declared {
		pj.Permissions = own
	}
	var err error
	if pj.Concurrency, err = planConcurrency(j.Concurrency, sc); err != nil {
		fail("concurrency", err)
	}
	if pj.ContinueOnError, err = planBool(j.ContinueOnError, sc); err != nil {
		fail("continue-on-error", err)
	}
	if j.TimeoutMins != nil {
		if *j.TimeoutMins <= 0 {
			fail("timeout-minutes", fmt.Errorf("%v is not a number of minutes greater than zero", *j.TimeoutMins))
		} else {
			pj.TimeoutMinutes = *j.TimeoutMins
		}
	}
	if j.If != "" {
		prog, err := conditionProgram(j.If)
		if err != nil {
			fail("if", err)
		} else {
			_, funcs := prog.References()
			pj.IfChecksStatus = slices.ContainsFunc(funcs, func(f string) bool { return lateFuncs[f] })
		}
	}
	return pj
}

// maxVarReads is as many names as a planned job carries. More than that is
// as good as all of them, and a plan is not refused for it.
const maxVarReads = 64

// placeVarReads names the variables that decide whether a job runs, where, and
// as what (A68): its if:, runs-on, environment and strategy, which the machine
// that takes it answers again for itself, and its name, concurrency and
// continue-on-error and its workflow's concurrency, which only a plan answers.
// Read from the file, so the list is the commit's and no machine's. A step's
// reads are not here: they change what a job does, not whether it does it.
func placeVarReads(w *Workflow, j *Job) []string {
	names := map[string]bool{}
	note := func(prog *expr.Program) {
		keys, computed := prog.ContextKeys("vars")
		contexts, _ := prog.References()
		if computed || (len(keys) == 0 && slices.Contains(contexts, "vars")) {
			names[protocol.AllVars] = true
		}
		for _, k := range keys {
			names[k] = true
		}
	}
	var visit func(v any)
	visit = func(v any) {
		switch t := v.(type) {
		case string:
			if spans, err := expr.Spans(t); err == nil {
				for _, sp := range spans {
					note(sp.Prog)
				}
			}
		case []any:
			for _, e := range t {
				visit(e)
			}
		case map[string]any:
			for _, e := range t {
				visit(e)
			}
		}
	}
	if j.If != "" {
		if prog, err := conditionProgram(j.If); err == nil {
			note(prog)
		}
	}
	visit(j.Name)
	for _, label := range j.RunsOn {
		visit(label)
	}
	for _, raw := range []json.RawMessage{
		// A call's with: is its called jobs' inputs, which may place them (A81).
		j.Environment, j.Strategy, j.Concurrency, j.ContinueOnError, j.With, w.Concurrency,
	} {
		var v any
		if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
			visit(v)
		}
	}
	if len(names) > maxVarReads {
		return []string{protocol.AllVars}
	}
	return slices.Sorted(maps.Keys(names))
}

func uniqueNeeds(needs StringList) []string {
	var out []string
	for _, n := range needs {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// jobKeyPattern is GitHub's rule for a job's id.
var jobKeyPattern = regexp.MustCompile(`^[_A-Za-z][A-Za-z0-9_-]*$`)

// jobOrder sorts a workflow's jobs so each comes after every job it needs —
// by key where needs leave the order open — and refuses a need that names no
// job, or a cycle, as GitHub refuses the file.
func jobOrder(w *Workflow) ([]string, error) {
	keys := w.JobNames()
	waiting := map[string]int{}
	dependents := map[string][]string{}
	for _, key := range keys {
		if !jobKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("%s: job id %q must start with a letter or _ and contain only "+
				"letters, digits, - and _", w.Path, key)
		}
		for _, need := range uniqueNeeds(w.Jobs[key].Needs) {
			if _, ok := w.Jobs[need]; !ok {
				return nil, fmt.Errorf("%s: job %q needs %q, which is not a job in this workflow", w.Path, key, need)
			}
			waiting[key]++
			dependents[need] = append(dependents[need], key)
		}
	}
	var ready, order []string
	for _, key := range keys {
		if waiting[key] == 0 {
			ready = append(ready, key)
		}
	}
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		order = append(order, key)
		for _, d := range dependents[key] {
			if waiting[d]--; waiting[d] == 0 {
				i, _ := slices.BinarySearch(ready, d)
				ready = slices.Insert(ready, i, d)
			}
		}
	}
	if len(order) < len(keys) {
		var stuck []string
		for _, key := range keys {
			if waiting[key] > 0 {
				stuck = append(stuck, key)
			}
		}
		return nil, fmt.Errorf("%s: jobs %s need each other in a cycle", w.Path, strings.Join(stuck, ", "))
	}
	return order, nil
}

// triggers reads `on:` as each event it names and that event's configuration,
// which is null for an event named bare.
func (w *Workflow) triggers() (map[string]json.RawMessage, error) {
	raw := w.On
	if len(raw) == 0 {
		raw = w.OnYAML11
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s has no on:, so nothing triggers it", w.Path)
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return map[string]json.RawMessage{one: nil}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		out := make(map[string]json.RawMessage, len(many))
		for _, e := range many {
			out[e] = nil
		}
		return out, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: on: expected an event, a list of events or a map of them, got %s", w.Path, raw)
	}
	return m, nil
}

// trigger is one planned event's configuration under `on:`.
type trigger struct {
	Types          StringList               `json:"types"`
	Branches       StringList               `json:"branches"`
	BranchesIgnore StringList               `json:"branches-ignore"`
	Tags           StringList               `json:"tags"`
	TagsIgnore     StringList               `json:"tags-ignore"`
	Paths          StringList               `json:"paths"`
	PathsIgnore    StringList               `json:"paths-ignore"`
	Inputs         map[string]dispatchInput `json:"inputs"`
}

// triggerFields are the keys each planned event's configuration may have.
// Strict, as everything else here is: `branch:` for `branches:` would
// otherwise be a filter that silently filters nothing.
var triggerFields = map[string][]string{
	EventPush:               {"branches", "branches-ignore", "tags", "tags-ignore", "paths", "paths-ignore"},
	EventPullRequest:        {"types", "branches", "branches-ignore", "paths", "paths-ignore"},
	EventPullRequestTarget:  {"types", "branches", "branches-ignore", "paths", "paths-ignore"},
	EventRelease:            {"types"},
	EventRepositoryDispatch: {"types"},
	EventWorkflowDispatch:   {"inputs"},
}

// defaultPullRequestTypes are the activity types a pull_request trigger
// without types: answers, as on GitHub.
var defaultPullRequestTypes = []string{"opened", "synchronize", "reopened"}

func decodeTrigger(event string, raw json.RawMessage) (*trigger, error) {
	t := &trigger{}
	if len(raw) == 0 || string(raw) == "null" {
		return t, nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("expected a map, got %s", raw)
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Contains(triggerFields[event], k) {
			return nil, fmt.Errorf("%q is not something %s filters on; it has %s",
				k, event, strings.Join(triggerFields[event], ", "))
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(t); err != nil {
		return nil, err
	}
	for _, pair := range []struct {
		name     string
		pos, neg StringList
	}{
		{"branches", t.Branches, t.BranchesIgnore},
		{"tags", t.Tags, t.TagsIgnore},
		{"paths", t.Paths, t.PathsIgnore},
	} {
		if pair.pos != nil && pair.neg != nil {
			return nil, fmt.Errorf("%s and %s-ignore cannot both be set; write the exclusions as ! patterns in %s",
				pair.name, pair.name, pair.name)
		}
	}
	return t, nil
}

// matches decides whether the event triggers the workflow, as GitHub decides.
func (p *planner) matches(t *trigger) (bool, error) {
	ev := p.ev
	switch ev.Name {
	case EventPush:
		hasBranches := t.Branches != nil || t.BranchesIgnore != nil
		hasTags := t.Tags != nil || t.TagsIgnore != nil
		if branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/"); ok {
			if hasBranches {
				if ok, err := refFilter(t.Branches, t.BranchesIgnore, branch); err != nil || !ok {
					return false, err
				}
			} else if hasTags {
				// Filtering only tags is not wanting branches at all.
				return false, nil
			}
			return p.pathsMatch(t)
		}
		tag := strings.TrimPrefix(ev.Ref, "refs/tags/")
		if hasTags {
			// GitHub does not evaluate paths for a tag.
			return refFilter(t.Tags, t.TagsIgnore, tag)
		}
		return !hasBranches, nil
	case EventPullRequest, EventPullRequestTarget:
		types := t.Types
		if types == nil {
			types = defaultPullRequestTypes
		}
		if !slices.Contains(types, ev.Action) {
			return false, nil
		}
		if t.Branches != nil || t.BranchesIgnore != nil {
			if ok, err := refFilter(t.Branches, t.BranchesIgnore, ev.BaseRef); err != nil || !ok {
				return false, err
			}
		}
		return p.pathsMatch(t)
	case EventRelease, EventRepositoryDispatch:
		return t.Types == nil || slices.Contains(t.Types, ev.Action), nil
	case EventWorkflowDispatch:
		return true, nil
	}
	return false, nil
}

// refFilter is a branches or tags filter, or its -ignore twin.
func refFilter(include, ignore StringList, name string) (bool, error) {
	if include != nil {
		l, err := compileFilters(include)
		if err != nil {
			return false, err
		}
		return l.matches(name), nil
	}
	l, err := compileFilters(ignore)
	if err != nil {
		return false, err
	}
	return !l.matches(name), nil
}

// pathsMatch is a paths filter: any changed file it matches triggers the
// workflow. paths-ignore: any changed file it does not match does. GitHub
// stops looking after 300 files; this does not, so a large change that GitHub
// would not have triggered on can trigger here.
func (p *planner) pathsMatch(t *trigger) (bool, error) {
	if t.Paths == nil && t.PathsIgnore == nil {
		return true, nil
	}
	patterns, ignore := t.Paths, false
	if patterns == nil {
		patterns, ignore = t.PathsIgnore, true
	}
	l, err := compileFilters(patterns)
	if err != nil {
		return false, err
	}
	changed, err := p.changed()
	if err != nil {
		return false, err
	}
	for _, f := range changed {
		if l.matches(f) != ignore {
			return true, nil
		}
	}
	return false, nil
}

func (p *planner) changed() ([]string, error) {
	if !p.diffDone {
		p.diffDone = true
		if p.opts.Changed == nil {
			p.diffErr = errors.New("a paths filter needs the files the event changed, and there is no diff to read")
		} else {
			p.diff, p.diffErr = p.opts.Changed()
		}
	}
	return p.diff, p.diffErr
}

// dispatchInput is one workflow_dispatch input's declaration.
type dispatchInput struct {
	Description string        `json:"description"`
	Required    bool          `json:"required"`
	Default     *InputDefault `json:"default"`
	Type        string        `json:"type"`
	Options     []string      `json:"options"`
}

// dispatchInputs checks a dispatch's inputs against the workflow's
// declarations, as GitHub does before it starts anything, and fills in the
// defaults. It returns them as strings — what a run carries, and what
// github.event.inputs holds — and typed, for the inputs context, where a
// boolean input is a boolean.
func dispatchInputs(decl map[string]dispatchInput, given map[string]string) (map[string]string, map[string]any, error) {
	var unexpected []string
	for k := range given {
		if _, ok := decl[k]; !ok {
			unexpected = append(unexpected, k)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return nil, nil, fmt.Errorf("the workflow declares no input %s", strings.Join(unexpected, ", "))
	}
	resolved := make(map[string]string, len(decl))
	typed := make(map[string]any, len(decl))
	for _, name := range slices.Sorted(maps.Keys(decl)) {
		in := decl[name]
		v, ok := given[name]
		if !ok && in.Default != nil {
			v, ok = string(*in.Default), true
		}
		if in.Required && v == "" {
			return nil, nil, fmt.Errorf("input %q is required", name)
		}
		switch in.Type {
		case "", "string", "environment":
			typed[name] = v
		case "boolean":
			if !ok {
				v = "false"
			}
			if v != "true" && v != "false" {
				return nil, nil, fmt.Errorf("input %q is a boolean, and %q is not true or false", name, v)
			}
			typed[name] = v == "true"
		case "number":
			if v == "" {
				typed[name] = nil
				break
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("input %q is a number, and %q is not one", name, v)
			}
			typed[name] = f
		case "choice":
			if len(in.Options) == 0 {
				return nil, nil, fmt.Errorf("input %q is a choice with no options", name)
			}
			if v != "" && !slices.Contains(in.Options, v) {
				return nil, nil, fmt.Errorf("input %q is one of %s, and %q is not",
					name, strings.Join(in.Options, ", "), v)
			}
			typed[name] = v
		default:
			return nil, nil, fmt.Errorf("input %q has type %q; GitHub's are string, boolean, number, choice "+
				"and environment", name, in.Type)
		}
		resolved[name] = v
	}
	return resolved, typed, nil
}

// permissionScopes are the scopes a permissions: block may name.
var permissionScopes = []string{
	"actions", "attestations", "checks", "contents", "deployments", "discussions", "id-token",
	"issues", "models", "packages", "pages", "pull-requests", "repository-projects",
	"security-events", "statuses",
}

// restrictedPermissions is GitHub's restricted default: what a token may do
// when neither a job nor its workflow says (A52).
var restrictedPermissions = map[string]string{"contents": "read", "packages": "read"}

// planPermissions reads a permissions: block — read-all, write-all, or a map
// of scopes — and reports whether there was one. {} is one: it grants
// nothing.
func planPermissions(raw json.RawMessage) (map[string]string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var all string
	if json.Unmarshal(raw, &all) == nil {
		if all != "read-all" && all != "write-all" {
			return nil, false, fmt.Errorf("expected read-all, write-all or a map of scopes, got %q", all)
		}
		out := make(map[string]string, len(permissionScopes))
		for _, s := range permissionScopes {
			out[s] = strings.TrimSuffix(all, "-all")
		}
		return out, true, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, fmt.Errorf("expected read-all, write-all or a map of scopes, got %s", raw)
	}
	out := map[string]string{}
	for _, scope := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(permissionScopes, scope) {
			return nil, false, fmt.Errorf("%q is not a permission scope; they are %s",
				scope, strings.Join(permissionScopes, ", "))
		}
		switch level := m[scope]; level {
		case "read", "write":
			out[scope] = level
		case "none":
		default:
			return nil, false, fmt.Errorf("%s: %q is not read, write or none", scope, level)
		}
	}
	return out, true, nil
}

// planConcurrency reads a concurrency: block — a group, or {group,
// cancel-in-progress} — and evaluates it.
func planConcurrency(raw json.RawMessage, sc expr.Scope) (*protocol.Concurrency, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var group string
	var cancel json.RawMessage
	if err := json.Unmarshal(raw, &group); err != nil {
		var obj struct {
			Group            string          `json:"group"`
			CancelInProgress json.RawMessage `json:"cancel-in-progress"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("expected a group or {group, cancel-in-progress}: %w", err)
		}
		group, cancel = obj.Group, obj.CancelInProgress
	}
	g, err := planString(group, sc)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(g) == "" {
		return nil, fmt.Errorf("the group %q is empty once evaluated", group)
	}
	c, err := planBool(cancel, sc)
	if err != nil {
		return nil, fmt.Errorf("cancel-in-progress: %w", err)
	}
	return &protocol.Concurrency{Group: g, CancelInProgress: c}, nil
}

// planContexts are what a plan's expressions may read. needs, steps, job,
// runner, secrets and the status functions are only answered as a job runs,
// and a plan is made before any does.
var planContexts = map[string]bool{"github": true, "inputs": true, "vars": true, "matrix": true, "strategy": true}

// planString evaluates a value — run-name, a concurrency group, a job's name
// or environment — against the plan's scope.
func planString(s string, sc expr.Scope) (string, error) {
	spans, err := expr.Spans(s)
	if err != nil {
		return "", err
	}
	for _, sp := range spans {
		if err := planReads(sp.Prog, sc); err != nil {
			return "", err
		}
	}
	return expr.EvalString(s, sc)
}

// planBool reads true, false, or one expression answered by the plan's scope.
func planBool(raw json.RawMessage, sc expr.Scope) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, fmt.Errorf("expected true, false or an expression, got %s", raw)
	}
	if !expr.Has(s) {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("expected true, false or an expression, got %q", s)
	}
	prog, err := conditionProgram(s)
	if err != nil {
		return false, err
	}
	if err := planReads(prog, sc); err != nil {
		return false, err
	}
	v, err := prog.Eval(sc)
	if err != nil {
		return false, err
	}
	return expr.IsTruthy(v), nil
}

// planReads refuses an expression that reads what a plan cannot know. A
// github property the plan's scope does not hold — run_id, run_number, job —
// is refused too, rather than read as null: a concurrency group built on
// github.run_id that evaluated to "" would put every run in one group.
func planReads(prog *expr.Program, sc expr.Scope) error {
	src := strings.TrimSpace(prog.Source())
	contexts, funcs := prog.References()
	for _, c := range contexts {
		if !planContexts[c] {
			return fmt.Errorf("${{ %s }} reads %s, which is only known once the job runs", src, c)
		}
	}
	for _, f := range funcs {
		if lateFuncs[f] || f == "hashfiles" {
			return fmt.Errorf("${{ %s }} calls %s(), which is only answered once the job runs", src, f)
		}
	}
	keys, _ := prog.ContextKeys("github")
	github, _ := sc.Contexts["github"].(map[string]any)
	for _, k := range keys {
		if _, ok := github[strings.ToLower(k)]; !ok {
			return fmt.Errorf("${{ %s }}: github.%s is not known when a run is planned", src, k)
		}
	}
	// A called workflow's input its caller gives from needs (A81).
	if name, li, ok := readsLateInput(prog, sc); ok {
		return fmt.Errorf("${{ %s }} reads inputs.%s, which its caller gives as %s, and that is only known "+
			"once the job runs", src, name, strings.TrimSpace(li.from))
	}
	return nil
}
