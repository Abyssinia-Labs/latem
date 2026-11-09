package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// hoistPrefix names the variables [Compile] binds expressions to.
//
// Not LATEM_: that namespace is reserved to the agent and steps[].env rejects
// it, which is the right rule and not one to weaken for this. GHA_ says where
// the value came from, and a collision with a name the author chose is checked
// for rather than assumed away.
const hoistPrefix = "GHA_EXPR_"

// lateContexts are answered by the run loop as the step is about to run, so
// an expression reaching for one compiles to a [registry.LateValue] rather
// than to a value. The command is unchanged either way — see hoist.
var lateContexts = map[string]bool{
	"job": true, "runner": true, "steps": true,
	// secrets are late for a reason that is not timing: the value must never
	// be in the compiled registry, which is persisted. Compiled late, the
	// registry holds the name, and the value is resolved on the machine and
	// read as the step is about to run (A50).
	"secrets": true,
	// latem is this agent's own, and carries the one fact a compiled job
	// cannot know: where a fetched action's files are reachable from inside
	// the step. Only a chosen executor can answer that (A42 phase 5).
	"latem": true,
}

// refusedContexts cannot be compiled at all, each for its own reason — unless
// the scope answers them: a workflow run's job carries its needs' results and
// outputs, so for it needs is known when the run is admitted (A52).
var refusedContexts = map[string]string{
	"needs": "this run is not a workflow run's job, so there is nothing for needs to refer to (A52)",
}

// lateFuncs are the status functions. They answer against the job's status,
// which the run loop has and the compiler does not, so they make an
// expression late in exactly the way the `job` context does.
//
// always() is here although its answer is a constant. What `if: always()`
// means is "run this step even after a failure", and the run loop can only
// honour that if the step still carries a condition when it gets there — an
// admission-time true would drop the condition and leave an ordinary step,
// which stops at the first failure like any other. A test holds this.
var lateFuncs = map[string]bool{"success": true, "failure": true, "cancelled": true, "always": true}

// TokenSecret is the secret a workflow run's job token is read as —
// secrets.GITHUB_TOKEN, and github.token, which is the same value (A52).
const TokenSecret = "GITHUB_TOKEN" //nolint:gosec // a secret's name, not a secret

// secretsContext is read by name only: `secrets.X` or `secrets['X']`. A name
// computed at run time could be any secret on the machine, so it is refused
// rather than resolved (A50).
const secretsContext = "secrets"

// labelPattern mirrors the native registry's, because the labels compiled here
// are matched against the same runner labels.
var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Compile turns one workflow job into a registry job.
//
// The scope supplies the contexts known when a run is admitted — github, env,
// vars, inputs. Anything the job reaches for that is not knowable then is an
// error naming the construct, never a guess.
//
// The result is an ordinary [registry.Job]: it goes through the same admission,
// the same executors and the same log pipeline as one written in
// .latem/jobs.yaml, because a second execution path would be a second place for
// the invariants in §5 to be true or not.
func Compile(ctx context.Context, w *Workflow, jobName string, sc expr.Scope) (*registry.Job, error) {
	return CompileWith(ctx, w, jobName, sc, Options{})
}

// Options is what compilation needs beyond the workflow itself.
type Options struct {
	// Actions reads a local action's metadata. Nil compiles everything except
	// `uses: ./path`, which is refused by name — there being no checkout to
	// read the action out of.
	Actions ActionLoader
	// Policy decides which remote actions this machine will run. Its zero
	// value refuses all of them, which is the shipped default (A42 phase 5).
	Policy actions.Policy
	// Fetcher resolves and fetches an allowed remote action. Nil refuses one
	// by name rather than reaching for a cache that is not there.
	Fetcher RemoteFetcher
	// LabelAliases is the machine's [label_aliases] (A50): a runs-on label
	// found here is replaced by the labels it names, before GitHub's hosted
	// labels are mapped. See [compileLabels].
	LabelAliases map[string][]string
	// Vars answers the vars context for a job in an environment (empty for
	// none), from the repository's config (A50). Nil leaves vars empty.
	Vars func(environment string) map[string]string
	// WorkflowRun compiles a workflow run's job (A52): needs, a job's if:,
	// outputs: and continue-on-error are then its own to honour, and the
	// scope carries the needs context they read.
	WorkflowRun bool
	// Matrix is the leg of a matrix job a run builds, as run.matrix carried
	// it (A55). The job's matrix is expanded here, from the file, and this
	// must be exactly one of its legs.
	Matrix json.RawMessage
	// IgnoreJobIf compiles a workflow run's job without evaluating its if:,
	// for `latem try` (A76): the person named the job they want run, and
	// whether this event would have run it is the question they did not ask.
	IgnoreJobIf bool
}

// CompileWith is [Compile] against a machine's actions and its policy.
func CompileWith(
	ctx context.Context, w *Workflow, jobName string, sc expr.Scope, opts Options,
) (*registry.Job, error) {
	declared, ok := w.Jobs[jobName]
	if !ok {
		return nil, &registry.Error{
			Code: protocol.ErrUnknownJob,
			Msg:  fmt.Sprintf("%s declares no job %q; it has %s", w.Path, jobName, strings.Join(w.JobNames(), ", ")),
		}
	}

	// A50: vars are the repository's, then the environment's over them —
	// so the environment is answered first, against the repository's.
	if opts.Vars != nil {
		sc = withContext(sc, "vars", stringsToAny(opts.Vars("")))
	}
	if opts.WorkflowRun {
		// A dispatch's inputs arrive as strings; the inputs context holds
		// them as the workflow declares them, so `inputs.dry == true` is
		// true for a boolean input set to "true", as on GitHub.
		var err error
		if sc, err = typedInputs(w, sc); err != nil {
			return nil, badRegistry("%s: %v", w.Path, err)
		}
		// The job's if:, against its needs and the event, before anything
		// else about the job is read: a skipped job's steps are not
		// compiled, because none of them runs (A52).
		run := true
		if !opts.IgnoreJobIf {
			if run, err = jobCondition(declared.If, sc); err != nil {
				return nil, badRegistry("%s: job %q: if: %v", w.Path, jobName, err)
			}
		}
		if !run {
			return &registry.Job{Description: declared.Name, Skip: true,
				SkipReason: strings.TrimSpace(declared.If)}, nil
		}
	}
	// A55: the matrix leg, after the job's if: — which cannot read it — and
	// before everything that can.
	sc, err := withLeg(w, declared, jobName, sc, opts.Matrix)
	if err != nil {
		return nil, err
	}
	environment, err := environmentName(declared.Environment, sc)
	if err != nil {
		return nil, badRegistry("%s: job %q: environment: %v", w.Path, jobName, err)
	}
	if opts.Vars != nil && environment != "" {
		sc = withContext(sc, "vars", stringsToAny(opts.Vars(environment)))
	}

	// A50: the env context holds the workflow's and the job's env:, which is
	// what GitHub evaluates ${{ env.X }} against; a step's own is added per
	// step. Before expansion, so an action's with: sees it too.
	jobEnv := envContext(sc, nil, w.Env, declared.Env)
	sc = withEnv(sc, jobEnv)

	// A44: local and docker:// actions become steps before anything else
	// looks at the job, so that the refusals, the hoisting and the late
	// bindings below all see only kinds of step they already understand. The
	// job is copied, not edited: a Workflow is parsed once and may be
	// compiled for more than one run.
	x := newExpander(opts.Actions, w.Path)
	x.policy, x.fetcher, x.scope = opts.Policy, opts.Fetcher, sc
	// A58: the platform the job's labels settle, for a step whose if: asks
	// nothing else. An error here is reported where labels are compiled.
	if labels, err := compileLabels(declared.RunsOn, w.Path, jobName, opts.LabelAliases, sc); err == nil {
		x.os = osOfLabels(labels)
	}
	expanded, err := x.expandSteps(ctx, declared.Steps, nil)
	if err != nil {
		return nil, badRegistry("%s: job %q: %v", w.Path, jobName, err)
	}
	// A51: JavaScript actions' pre steps first, their post steps last and in
	// reverse, as GitHub orders them.
	if len(x.pre) > 0 || len(x.post) > 0 {
		all := make([]Step, 0, len(x.pre)+len(expanded)+len(x.post))
		all = append(all, x.pre...)
		all = append(all, expanded...)
		for i := len(x.post) - 1; i >= 0; i-- {
			all = append(all, x.post[i])
		}
		expanded = all
	}
	copied := *declared
	copied.Steps = expanded
	j := &copied

	if err := j.unsupported(w.Path, jobName, opts.WorkflowRun); err != nil {
		return nil, err
	}

	// Workflow-level env is the base; the job's overrides it. Step env is
	// applied per step below.
	baseEnv := mergeEnv(w.Env, j.Env)

	labels, err := compileLabels(j.RunsOn, w.Path, jobName, opts.LabelAliases, sc)
	if err != nil {
		return nil, err
	}

	out := &registry.Job{
		Description: j.Name,
		RunsOn:      labels,
		// A remote action is code the operator did not write, and by this
		// point it has been inlined into ordinary steps — so unless the job
		// says so here, nothing downstream can tell. The operator's opt-out
		// was applied where the policy lives, in the expander, so a job that
		// still carries this has not been excused from it (A47).
		RequiresIsolation: x.usedRemote && !opts.Policy.AllowUnisolated,
		CacheDecls:        x.caches,
	}
	for _, d := range x.caches {
		out.Caches = append(out.Caches, d.Name)
	}
	// GitHub's default rather than §9's 30 minutes (A50): a workflow that
	// relies on its jobs being allowed six hours never had to say so.
	out.Timeout = protocol.Duration(DefaultJobTimeout)
	if j.TimeoutMins != nil {
		out.Timeout = protocol.Duration(float64(*j.TimeoutMins) * float64(minute))
	}

	d := jobDefaults{
		env:        baseEnv,
		envContext: jobEnv,
		shell:      defaultShell(w.Defaults, j.Defaults),
		workingDir: defaultWorkingDirectory(w.Defaults, j.Defaults),
	}

	for i, st := range j.Steps {
		where := fmt.Sprintf("%s: job %q step %d", w.Path, jobName, i+1)
		if st.Name != "" {
			where = fmt.Sprintf("%s: job %q step %q", w.Path, jobName, st.Name)
		}
		compiled, skip, err := compileStep(st, where, d, sc)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		out.Steps = append(out.Steps, compiled)
	}

	if len(out.Steps) == 0 {
		return nil, badRegistry("%s: job %q has no steps to run", w.Path, jobName)
	}
	if opts.WorkflowRun {
		if out.Outputs, err = compileOutputs(j.Outputs, sc); err != nil {
			return nil, badRegistry("%s: job %q: outputs: %v", w.Path, jobName, err)
		}
		out.Inputs = dispatchDeclarations(w)
	}
	// A50: the names, never the values. The run resolves them on the
	// machine, scoped by repository and environment, and registers every
	// value with the redactor before the first step.
	out.Secrets = secretNames(out.Steps)
	out.Secrets = mergeNames(out.Secrets, outputSecrets(out.Outputs))
	out.Environment = environment
	out.Notes = append([]string(nil), x.notes...)
	if !opts.WorkflowRun {
		out.Notes = append(out.Notes, ignoredNotes(w, j)...)
	}
	return out, nil
}

// ignoredNotes says what the job's workflow asked for that latem parsed and
// does not do yet, so a run does not appear to honour it (A50).
func ignoredNotes(w *Workflow, j *Job) []string {
	var notes []string
	if len(w.Permissions) > 0 || len(j.Permissions) > 0 {
		notes = append(notes, "permissions: is not enforced — there is no GITHUB_TOKEN for it to scope yet")
	}
	if len(w.Concurrency) > 0 || len(j.Concurrency) > 0 {
		notes = append(notes, "concurrency: is not honoured — this run neither waits for nor cancels another "+
			"(a jobs.yaml concurrency_group does serialise)")
	}
	return notes
}

// secretNames lists every secret a compiled job's late values and conditions
// read, sorted.
func secretNames(steps []registry.Step) []string {
	set := map[string]bool{}
	note := func(p *expr.Program) {
		names, _ := p.ContextKeys(secretsContext)
		for _, n := range names {
			set[n] = true
		}
		if keys, _ := p.ContextKeys("github"); slices.Contains(keys, "token") {
			set[TokenSecret] = true
		}
	}
	visit := func(v any) {
		switch l := v.(type) {
		case *lateExpr:
			note(l.prog)
		case *lateTemplate:
			for _, p := range l.progs {
				note(p)
			}
		}
	}
	for _, st := range steps {
		for _, v := range st.Late {
			visit(v)
		}
		if st.If != nil {
			visit(st.If)
		}
		if c := st.Checkout; c != nil {
			visit(c.RefLate)
			visit(c.Token)
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// environmentName reads `environment:` — a name, or {name, url} — and answers
// any expression in the name now (A50). Its url is GitHub's deployment UI and
// has nothing to scope here.
func environmentName(raw json.RawMessage, sc expr.Scope) (string, error) {
	name, err := environmentField(raw)
	if err != nil || name == "" {
		return "", err
	}
	if !answerableNow(name, sc) {
		return "", fmt.Errorf("%q is only known when the job runs, and an environment decides which secrets it gets before it starts", name)
	}
	out, err := expr.EvalString(name, sc)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// environmentField is the name an `environment:` gives, unevaluated.
func environmentField(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		var obj struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", fmt.Errorf("expected a name or {name, url}, got %s", string(raw))
		}
		name = obj.Name
	}
	return name, nil
}

const minute = 60 * 1_000_000_000 // protocol.Duration is nanoseconds

// DefaultJobTimeout is GitHub's timeout-minutes default, 360 (A50).
const DefaultJobTimeout = 360 * minute

// runsOnValues evaluates an expression in runs-on to its labels: a string, or
// a list of strings.
func runsOnValues(raw string, sc expr.Scope) ([]string, error) {
	spans, err := expr.Spans(raw)
	if err != nil {
		return nil, err
	}
	for _, sp := range spans {
		if err := planReads(sp.Prog, sc); err != nil {
			return nil, err
		}
	}
	v, err := expr.EvalValue(raw, sc)
	if err != nil {
		return nil, err
	}
	var labels []string
	switch t := v.(type) {
	case string:
		labels = []string{t}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s yields %s, which is not a label", raw, describe(e))
			}
			labels = append(labels, s)
		}
	default:
		return nil, fmt.Errorf("%s yields %s, not a label or a list of labels", raw, describe(v))
	}
	for _, l := range labels {
		if strings.TrimSpace(l) == "" {
			return nil, fmt.Errorf("%s yields an empty label", raw)
		}
	}
	return labels, nil
}

// unsupported refuses every construct this phase does not implement, by name.
//
// One error listing all of them, rather than the first: a workflow being
// adopted usually trips several at once, and making the author discover them
// one build at a time is the slow version of the same conversation.
func (j *Job) unsupported(file, name string, workflowRun bool) error {
	var found []string
	add := func(what, why string) { found = append(found, fmt.Sprintf("  %s — %s", what, why)) }

	switch {
	case j.Uses != "" && workflowRun:
		// A workflow run plans a call as the called workflow's jobs, and a
		// run names one of those by its job path (A82).
		add("uses: (a called workflow)", "the job runs as the called workflow's jobs; name one as <job>/<called job>")
	case j.Uses != "":
		add("uses: (a called workflow)", "a call is planned into a workflow run, and this run is not a workflow run's job (A82)")
	case len(j.With) > 0 || len(j.Secrets) > 0:
		add("with: or secrets: on a job", "only a job that calls a workflow has them, as on GitHub")
	}
	if len(j.Needs) > 0 && !workflowRun {
		add("needs:", refusedContexts["needs"])
	}
	if len(j.Services) > 0 {
		add("services:", "service containers are not implemented")
	}
	if len(j.Container) > 0 {
		add("container:", "set the executor's image in config.toml instead")
	}
	// A workflow run's job honours these (A52): its if: was answered before
	// this, its outputs are reported as it ends, and continue-on-error
	// decides the workflow run's conclusion, which is the control plane's.
	if !workflowRun {
		if len(j.Outputs) > 0 {
			add("outputs:", "job outputs are read by the jobs that need this one, and this run is not a workflow run's job (A52)")
		}
		if len(j.ContinueOnError) > 0 {
			add("continue-on-error: on a job", "it decides a workflow run's result, and this run is not a workflow run's job (A52)")
		}
		if j.If != "" {
			add("if: on a job", "it decides the job's place in a workflow run, and this run is not a workflow run's job (A52)")
		}
	}
	for i, st := range j.Steps {
		if st.Uses != "" {
			// Unreachable since phase 5 step 2: the expander resolves every
			// `uses:` or refuses it by name. Kept because a step that reaches
			// execution with an unexpanded `uses:` would otherwise run as an
			// empty command, which is the silent version of this.
			found = append(found, fmt.Sprintf("  step %d uses: %s — unresolved", i+1, st.Uses))
		}
		if st.Run == "" && st.Uses == "" && st.container == nil && st.artifact == nil && st.checkout == nil {
			found = append(found, fmt.Sprintf("  step %d — has neither run: nor uses:", i+1))
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return badRegistry("%s: job %q uses constructs latem does not implement yet:\n%s",
		file, name, strings.Join(found, "\n"))
}

// compileLabels maps runs-on onto runner labels.
//
// Lowercased, because GitHub's own labels are written `macOS` and `ubuntu-latest`
// interchangeably and a runner's labels are lowercase by §8's pattern. A label
// that still does not fit the pattern is refused here rather than failing to
// match anything later, which would present as "no runner" and send the
// operator looking at their config.
//
// Since A50 a label is first looked up in the machine's aliases and then in
// GitHub's hosted labels, and replaced by what it stands for: `ubuntu-latest`
// is `linux` on a machine you own, and a vendor's label is whatever the
// operator says it is. A Windows label is refused by name, because no latem
// runner can be one (A47).
func compileLabels(runsOn StringList, file, job string, aliases map[string][]string, sc expr.Scope) ([]string, error) {
	if len(runsOn) == 0 {
		return nil, badRegistry("%s: job %q has no runs-on", file, job)
	}
	runsOn, err := answeredRunsOn(runsOn, file, job, sc)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(runsOn))
	seen := map[string]bool{}
	add := func(l string) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	for _, raw := range runsOn {
		l := strings.ToLower(strings.TrimSpace(raw))
		// self-hosted is GitHub's marker for "not a hosted runner". Every
		// latem runner is self-hosted, so it carries no information and is
		// dropped rather than demanded of the operator's label set.
		if l == "self-hosted" {
			continue
		}
		if mapped, ok := aliases[l]; ok {
			for _, m := range mapped {
				add(m)
			}
			continue
		}
		if strings.HasPrefix(l, "windows") {
			return nil, badRegistry(
				"%s: job %q runs on %q, and no latem runner can be Windows (A47)", file, job, raw)
		}
		if mapped := hostedLabel(l); mapped != nil {
			for _, m := range mapped {
				add(m)
			}
			continue
		}
		if !labelPattern.MatchString(l) {
			return nil, badRegistry("%s: job %q: runs-on label %q must match %s", file, job, raw, labelPattern)
		}
		add(l)
	}
	if len(out) == 0 {
		return nil, badRegistry(
			"%s: job %q has only `self-hosted` in runs-on, which every latem runner is; "+
				"name the labels that distinguish the machine you want", file, job)
	}
	return out, nil
}

// answeredRunsOn is runs-on with its expressions answered, as a plan answers
// them (A55): from the event, the inputs, the vars and the matrix leg, all
// known before a machine is chosen. Each label an expression yields is checked
// as a written one is.
func answeredRunsOn(runsOn StringList, file, job string, sc expr.Scope) (StringList, error) {
	var written StringList
	for _, raw := range runsOn {
		if !expr.Has(raw) {
			written = append(written, raw)
			continue
		}
		labels, err := runsOnValues(raw, sc)
		if err != nil {
			return nil, badRegistry("%s: job %q: runs-on: %v", file, job, err)
		}
		written = append(written, labels...)
	}
	return written, nil
}

// plannedRunsOn is what [compileLabels] reads before it looks at any aliases
// (A68), for a control plane to finish on each machine with that machine's
// own: the same rule, alias first, then the hosted label, then the label.
func plannedRunsOn(written StringList) []protocol.RunsOnLabel {
	var out []protocol.RunsOnLabel
	seen := map[string]bool{}
	for _, raw := range written {
		l := strings.ToLower(strings.TrimSpace(raw))
		if l == "self-hosted" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, protocol.RunsOnLabel{Label: l, Hosted: hostedLabel(l)})
	}
	return out
}

var (
	ubuntuLabel    = regexp.MustCompile(`^ubuntu-(latest|[0-9]+\.[0-9]+)$`)
	ubuntuArmLabel = regexp.MustCompile(`^ubuntu-(latest|[0-9]+\.[0-9]+)-arm$`)
	macosLabel     = regexp.MustCompile(`^macos-(latest|[0-9]+)(-x?large)?$`)
)

// hostedLabel maps one of GitHub's hosted-runner labels onto the labels a
// machine you own carries for the same platform, or nil when l is not one
// (A50). The version in the label is not kept: a machine is whatever release
// it runs, and that is the operator's to say, through an alias, if a job has
// to care.
func hostedLabel(l string) []string {
	switch {
	case ubuntuLabel.MatchString(l):
		return []string{"linux"}
	case ubuntuArmLabel.MatchString(l):
		return []string{"linux", "arm64"}
	case macosLabel.MatchString(l):
		return []string{"macos"}
	}
	return nil
}

// jobDefaults is what every step of a job starts from.
type jobDefaults struct {
	env        map[string]string // the workflow's env: under the job's
	envContext map[string]any    // the same, answered, for ${{ env.X }}
	shell      string
	workingDir string
}

func defaultWorkingDirectory(wf, job *Defaults) string {
	dir := ""
	if wf != nil && wf.Run != nil && wf.Run.WorkingDirectory != "" {
		dir = wf.Run.WorkingDirectory
	}
	if job != nil && job.Run != nil && job.Run.WorkingDirectory != "" {
		dir = job.Run.WorkingDirectory
	}
	return dir
}

func defaultShell(wf, job *Defaults) string {
	shell := ""
	if wf != nil && wf.Run != nil && wf.Run.Shell != "" {
		shell = wf.Run.Shell
	}
	if job != nil && job.Run != nil && job.Run.Shell != "" {
		shell = job.Run.Shell
	}
	return shell
}

func mergeEnv(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// compileStep produces one registry step, or reports that the step is skipped
// because its `if:` evaluated false at admission.
//
// A condition that can be answered now is answered now, and a false one drops
// the step from the compiled job — there is nothing to gain from carrying a
// step that will not run. A condition that needs the run loop becomes the
// step's If, evaluated then.
func compileStep(st Step, where string, d jobDefaults, sc expr.Scope) (registry.Step, bool, error) {
	baseEnv, shell := d.env, d.shell
	// The step's own env: joins the env context for its own expressions (A50).
	sc = withEnv(sc, envContext(sc, d.envContext, st.Env))

	continueOnError, err := compileContinueOnError(st.ContinueOnError, sc)
	if err != nil {
		return registry.Step{}, false, badRegistry("%s: continue-on-error: %v", where, err)
	}

	var cond registry.Condition
	if st.If != "" {
		prog, err := conditionProgram(st.If)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
		}
		w, err := classify(prog, sc)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
		}
		switch w {
		case whenLate:
			cond = newLate(prog, sc)
		default:
			v, err := prog.Eval(sc)
			if err != nil {
				return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
			}
			if !expr.IsTruthy(v) {
				return registry.Step{}, true, nil
			}
		}
	}

	// A step the run loop performs itself has no process, so it has no shell,
	// environment or working directory — only its condition and its budget.
	if st.checkout != nil {
		c, err := compileCheckout(st.checkout, sc, where)
		if err != nil {
			return registry.Step{}, false, err
		}
		out := registry.Step{ID: st.ID, Name: st.Name, If: cond, Checkout: c, ContinueOnError: continueOnError}
		if st.TimeoutMins != nil {
			out.Timeout = protocol.Duration(float64(*st.TimeoutMins) * float64(minute))
		}
		return out, false, nil
	}
	if st.artifact != nil {
		out := registry.Step{ID: st.ID, Name: st.Name, If: cond, Artifact: st.artifact, ContinueOnError: continueOnError}
		if st.TimeoutMins != nil {
			out.Timeout = protocol.Duration(float64(*st.TimeoutMins) * float64(minute))
		}
		return out, false, nil
	}

	if st.Shell != "" {
		shell = st.Shell
	}
	if st.container == nil && shell != "" && !isSupportedShell(shell) {
		return registry.Step{}, false, badRegistry(
			"%s: shell: %q is not one latem runs; a step is a process, and the shells it may be are bash and sh", where, shell)
	}

	env := map[string]string{}
	late := map[string]registry.LateValue{}
	bind := func(k, v string) error {
		lv, err := envValue(k, v, sc, where)
		if err != nil {
			return err
		}
		if lv != nil {
			late[k] = lv
			delete(env, k)
			return nil
		}
		resolved, err := expr.EvalString(v, sc)
		if err != nil {
			return badRegistry("%s: env.%s: %v", where, k, err)
		}
		delete(late, k)
		env[k] = resolved
		return nil
	}
	for _, k := range sortedKeys(baseEnv) {
		if err := bind(k, baseEnv[k]); err != nil {
			return registry.Step{}, false, err
		}
	}
	for _, k := range sortedKeys(st.Env) {
		if err := bind(k, st.Env[k]); err != nil {
			return registry.Step{}, false, err
		}
	}

	if st.container != nil {
		c, err := compileContainer(st.container, sc, where)
		if err != nil {
			return registry.Step{}, false, err
		}
		for k, v := range env {
			if err := envValueIsLiteral(v); err != nil {
				return registry.Step{}, false, badRegistry("%s: env.%s: %v", where, k, err)
			}
		}
		out := registry.Step{ID: st.ID, Name: st.Name, Env: env, If: cond, Container: c, ContinueOnError: continueOnError}
		if st.TimeoutMins != nil {
			out.Timeout = protocol.Duration(float64(*st.TimeoutMins) * float64(minute))
		}
		if len(env) == 0 {
			out.Env = nil
		}
		if len(late) > 0 {
			out.Late = late
		}
		return out, false, nil
	}

	taken := map[string]string{}
	for k, v := range env {
		taken[k] = v
	}
	for k := range late {
		taken[k] = ""
	}
	run, hoisted, hoistedLate, err := hoist(st.Run, taken, sc, where)
	if err != nil {
		return registry.Step{}, false, err
	}
	for k, v := range hoisted {
		env[k] = v
	}
	for k, v := range hoistedLate {
		late[k] = v
	}

	// A job's defaults.run.working-directory, unless the step names its own
	// or came from an action, which GitHub's defaults do not reach (A50).
	workingDir := st.WorkingDirectory
	if workingDir == "" && !st.fromAction {
		workingDir = d.workingDir
	}
	if workingDir != "" {
		dir, err := expr.EvalString(workingDir, sc)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: working-directory: %v", where, err)
		}
		// A step is a new process, so a `cd` in one does not survive into the
		// next — which is exactly what working-directory means here, and why
		// prefixing the command is the whole implementation.
		run = fmt.Sprintf("cd %s\n%s", shellQuote(dir), run)
	}

	for k, v := range env {
		if err := envValueIsLiteral(v); err != nil {
			return registry.Step{}, false, badRegistry("%s: env.%s: %v", where, k, err)
		}
	}

	out := registry.Step{
		ID: st.ID, Name: st.Name, Run: run, Shell: string(githubShell(shell)), Env: env, If: cond,
		ContinueOnError: continueOnError, ActionUse: st.actionUse,
	}
	if st.TimeoutMins != nil {
		out.Timeout = protocol.Duration(float64(*st.TimeoutMins) * float64(minute))
	}
	if len(env) == 0 {
		out.Env = nil
	}
	if len(late) > 0 {
		out.Late = late
	}
	return out, false, nil
}

// envValue decides whether an env: value is late: it is when any expression in
// it is. One expression is a [lateExpr]; text around it makes a
// [lateTemplate], rendered whole when the step runs (A50).
func envValue(k, v string, sc expr.Scope, where string) (registry.LateValue, error) {
	if !expr.Has(v) {
		return nil, nil
	}
	spans, err := expr.Spans(v)
	if err != nil {
		return nil, badRegistry("%s: env.%s: %v", where, k, err)
	}
	anyLate := false
	for _, sp := range spans {
		w, err := classify(sp.Prog, sc)
		if err != nil {
			return nil, badRegistry("%s: env.%s: %v", where, k, err)
		}
		if w == whenLate {
			anyLate = true
		}
	}
	if !anyLate {
		return nil, nil
	}
	if len(spans) != 1 || spans[0].Start != 0 || spans[0].End != len(v) {
		// Text around a late expression — `Bearer ${{ secrets.TOKEN }}` — is
		// rendered whole as the step is about to run (A50). It is still an
		// environment value, so nothing about A42's rule changes.
		return newLateTemplate(v, spans, sc), nil
	}
	return newLate(spans[0].Prog, sc), nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hoist rewrites a run: so that no evaluated value is ever shell text.
//
// This is A42's normative rule and the reason this package exists in the shape
// it does. `run: echo ${{ github.event.pull_request.title }}` becomes
//
//	env:  GHA_EXPR_1: <the title>
//	run:  echo "$GHA_EXPR_1"
//
// which is the same program for every title and is not a different program for
// a title containing `; rm -rf /`. Substituting the value into the command is
// what GitHub does and is the injection class it issues advisories for.
//
// The variable is referenced double-quoted so that whitespace and globbing
// characters in the value do not word-split either.
func hoist(run string, existing map[string]string, sc expr.Scope, where string) (string, map[string]string, map[string]registry.LateValue, error) {
	if !expr.Has(run) {
		return run, nil, nil, nil
	}
	spans, err := expr.Spans(run)
	if err != nil {
		return "", nil, nil, badRegistry("%s: run: %v", where, err)
	}

	taken := map[string]bool{}
	for k := range existing {
		taken[k] = true
	}

	hoisted := map[string]string{}
	late := map[string]registry.LateValue{}
	var b strings.Builder
	last, n := 0, 0
	for _, sp := range spans {
		w, err := classify(sp.Prog, sc)
		if err != nil {
			return "", nil, nil, badRegistry("%s: run: %v", where, err)
		}

		name := ""
		for {
			n++
			name = fmt.Sprintf("%s%d", hoistPrefix, n)
			if !taken[name] {
				break
			}
		}
		taken[name] = true

		// Either way the command is the same: it references a variable. The
		// only thing that differs is whether the variable's value is known
		// now or handed to the run loop to compute. That is the whole of A42's
		// rule at this level — a late value changes an env entry, never the
		// command.
		switch w {
		case whenLate:
			late[name] = newLate(sp.Prog, sc)
		default:
			v, err := sp.Prog.Eval(sc)
			if err != nil {
				return "", nil, nil, badRegistry("%s: run: ${{ %s }}: %v", where, strings.TrimSpace(sp.Prog.Source()), err)
			}
			hoisted[name] = expr.Stringify(v)
		}

		b.WriteString(run[last:sp.Start])
		b.WriteString(`"$` + name + `"`)
		last = sp.End
	}
	b.WriteString(run[last:])
	return b.String(), hoisted, late, nil
}

// when says at what point an expression can be answered.
type when int

const (
	whenNow  when = iota // every context is in the admission scope
	whenLate             // needs the run loop's StepScope
)

// classify decides whether an expression compiles to a value now, to a late
// binding, or not at all — in which case the error names why.
//
// secrets is checked first and separately: it is neither now nor late, and the
// reason is not timing (INV-4, §15).
func classify(p *expr.Program, sc expr.Scope) (when, error) {
	contexts, funcs := p.References()
	result := whenNow
	// github.token is a secret by another name (A52): never compiled into the
	// registry, answered as the step runs from the job's token.
	if keys, _ := p.ContextKeys("github"); slices.Contains(keys, "token") {
		result = whenLate
	}
	for _, c := range contexts {
		if c == secretsContext {
			if _, computed := p.ContextKeys(secretsContext); computed {
				return whenNow, fmt.Errorf("${{ %s }}: a secret has to be named, as secrets.NAME — "+
					"one chosen at run time could be any secret on the machine", strings.TrimSpace(p.Source()))
			}
		}
		if why, refused := refusedContexts[c]; refused && sc.Contexts[c] == nil {
			return whenNow, fmt.Errorf("${{ %s }}: the %s context cannot be used here — %s",
				strings.TrimSpace(p.Source()), c, why)
		}
		if lateContexts[c] {
			result = whenLate
		}
	}
	for _, f := range funcs {
		if lateFuncs[f] {
			result = whenLate
		}
	}
	return result, nil
}

// conditionProgram compiles an `if:`, which may be written bare or wrapped in
// ${{ }}. A condition is one expression; text around the braces has no meaning
// as a condition and is refused rather than coerced to a string that is always
// truthy.
func conditionProgram(cond string) (*expr.Program, error) {
	trimmed := strings.TrimSpace(cond)
	if !expr.Has(trimmed) {
		return expr.Parse(trimmed)
	}
	spans, err := expr.Spans(trimmed)
	if err != nil {
		return nil, err
	}
	if len(spans) != 1 || spans[0].Start != 0 || spans[0].End != len(trimmed) {
		return nil, fmt.Errorf("a condition is a single expression, not text with expressions in it")
	}
	return spans[0].Prog, nil
}

// envValueIsLiteral rejects a compiled value that would be read as this
// system's own interpolation syntax when the registry validates it.
//
// It is rare — it takes a branch or a commit message containing "${" — and
// failing here with the reason beats failing in [registry.Validate] with a
// message about an expansion the author never wrote.
func envValueIsLiteral(v string) error {
	if strings.Contains(v, "${") {
		return fmt.Errorf(
			"the value evaluates to %q, which contains ${ and would be read as an interpolation "+
				"by the registry validator; this is a limitation, not something the workflow did wrong", v)
	}
	return nil
}

// githubShell maps a workflow's shell onto the executor's name for GitHub's
// contract (A50). shell has already passed [isSupportedShell]; empty is
// GitHub's default for a step that names none, `bash -e`, which is not the
// same as `shell: bash`.
func githubShell(shell string) executor.Shell {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "bash":
		return executor.ShellGitHubBash
	case "sh":
		return executor.ShellGitHubSh
	}
	return executor.ShellGitHub
}

func isSupportedShell(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bash", "sh":
		return true
	}
	return false
}

// shellQuote wraps a value in single quotes for the one place a compiled value
// becomes part of a command: the `cd` that implements working-directory.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// compileContainer resolves a container step's image, entrypoint and argv.
//
// All three must be answerable at admission. An argv element is not an
// environment entry: there is no variable for a late value to arrive in, and
// the honest answer to `args: [${{ steps.x.outputs.y }}]` is to say so and
// name the channel that does work, rather than to pass an empty argument.
func compileContainer(c *containerSpec, sc expr.Scope, where string) (*registry.ContainerStep, error) {
	static := func(field, v string) (string, error) {
		if !expr.Has(v) {
			return v, nil
		}
		spans, err := expr.Spans(v)
		if err != nil {
			return "", badRegistry("%s: %s: %v", where, field, err)
		}
		for _, sp := range spans {
			w, err := classify(sp.Prog, sc)
			if err != nil {
				return "", badRegistry("%s: %s: %v", where, field, err)
			}
			if w == whenLate {
				return "", badRegistry(
					"%s: %s: ${{ %s }} is only known when the step runs, and a container's %s is fixed "+
						"at admission; pass the value through env, which can be late", where, field, sp.Prog.Source(), field)
			}
		}
		out, err := expr.EvalString(v, sc)
		if err != nil {
			return "", badRegistry("%s: %s: %v", where, field, err)
		}
		return out, nil
	}

	out := &registry.ContainerStep{}
	var err error
	if out.Image, err = static("image", c.Image); err != nil {
		return nil, err
	}
	if out.Entrypoint, err = static("entrypoint", c.Entrypoint); err != nil {
		return nil, err
	}
	for _, a := range c.Args {
		v, err := static("args", a)
		if err != nil {
			return nil, err
		}
		out.Args = append(out.Args, v)
	}
	return out, nil
}

// RegistryFor wraps one compiled job in a registry, lifting the caches it
// declared to where a registry declares them. It is the one place that happens,
// so the resolver and anything else building a registry from a compiled job
// cannot disagree about it.
func RegistryFor(jobName string, job *registry.Job) *registry.Registry {
	return &registry.Registry{
		Version: registry.SchemaVersion,
		Caches:  append([]registry.CacheDecl(nil), job.CacheDecls...),
		Jobs:    map[string]*registry.Job{jobName: job},
	}
}
