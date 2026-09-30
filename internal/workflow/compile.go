package workflow

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

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
	// latem is this agent's own, and carries the one fact a compiled job
	// cannot know: where a fetched action's files are reachable from inside
	// the step. Only a chosen executor can answer that (A42 phase 5).
	"latem": true,
}

// refusedContexts cannot be compiled at all, each for its own reason.
var refusedContexts = map[string]string{
	"needs": "latem runs one job per run, so there is nothing for needs to refer to (N2)",
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

// secretsContext is refused separately from the run-time contexts, because the
// reason is not timing.
//
// INV-4 and §15: a secret reaches a step as an environment variable of its own
// name, because the job declared it in secrets: — it is never substituted. If
// this compiled, the value would be evaluated at admission and written into the
// registry's env map, which is persisted and logged; "secrets never leave the
// machine" would still hold and "secrets are not in the registry" would not.
// Answering null instead would be worse still: the step would run with an empty
// credential and fail somewhere that does not mention secrets.
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

	// A44: local and docker:// actions become steps before anything else
	// looks at the job, so that the refusals, the hoisting and the late
	// bindings below all see only kinds of step they already understand. The
	// job is copied, not edited: a Workflow is parsed once and may be
	// compiled for more than one run.
	x := newExpander(opts.Actions, w.Path)
	x.policy, x.fetcher, x.scope = opts.Policy, opts.Fetcher, sc
	expanded, err := x.expandSteps(ctx, declared.Steps, nil)
	if err != nil {
		return nil, badRegistry("%s: job %q: %v", w.Path, jobName, err)
	}
	copied := *declared
	copied.Steps = expanded
	j := &copied

	if err := j.unsupported(w.Path, jobName); err != nil {
		return nil, err
	}

	// Workflow-level env is the base; the job's overrides it. Step env is
	// applied per step below.
	baseEnv := mergeEnv(w.Env, j.Env)

	labels, err := compileLabels(j.RunsOn, w.Path, jobName)
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
	if j.TimeoutMins != nil {
		out.Timeout = protocol.Duration(float64(*j.TimeoutMins) * float64(minute))
	}

	shell := defaultShell(w.Defaults, j.Defaults)

	for i, st := range j.Steps {
		where := fmt.Sprintf("%s: job %q step %d", w.Path, jobName, i+1)
		if st.Name != "" {
			where = fmt.Sprintf("%s: job %q step %q", w.Path, jobName, st.Name)
		}
		compiled, skip, err := compileStep(st, where, baseEnv, shell, sc)
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
	return out, nil
}

const minute = 60 * 1_000_000_000 // protocol.Duration is nanoseconds

// unsupported refuses every construct this phase does not implement, by name.
//
// One error listing all of them, rather than the first: a workflow being
// adopted usually trips several at once, and making the author discover them
// one build at a time is the slow version of the same conversation.
func (j *Job) unsupported(file, name string) error {
	var found []string
	add := func(what, why string) { found = append(found, fmt.Sprintf("  %s — %s", what, why)) }

	if j.Uses != "" {
		add("uses: (reusable workflow)", "calling another workflow is not implemented")
	}
	if len(j.Strategy) > 0 {
		add("strategy:", "a matrix expands one job into several; latem runs the job you named")
	}
	if len(j.Needs) > 0 {
		add("needs:", refusedContexts["needs"])
	}
	if len(j.Services) > 0 {
		add("services:", "service containers are not implemented")
	}
	if len(j.Container) > 0 {
		add("container:", "set the executor's image in config.toml instead")
	}
	if len(j.Outputs) > 0 {
		add("outputs:", "job outputs are consumed by needs:, which latem has no equivalent for")
	}
	if len(j.ContinueOnError) > 0 {
		add("continue-on-error:", "a step either fails the run or it does not")
	}
	if j.If != "" {
		add("if: on a job", "a run targets one job; gate it with the hook's ref pattern instead")
	}
	for i, st := range j.Steps {
		if st.Uses != "" {
			// Unreachable since phase 5 step 2: the expander resolves every
			// `uses:` or refuses it by name. Kept because a step that reaches
			// execution with an unexpanded `uses:` would otherwise run as an
			// empty command, which is the silent version of this.
			found = append(found, fmt.Sprintf("  step %d uses: %s — unresolved", i+1, st.Uses))
		}
		if st.Run == "" && st.Uses == "" && st.container == nil && st.artifact == nil {
			found = append(found, fmt.Sprintf("  step %d — has neither run: nor uses:", i+1))
		}
		if len(st.ContinueOnError) > 0 {
			found = append(found, fmt.Sprintf("  step %d continue-on-error: — not implemented", i+1))
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
func compileLabels(runsOn StringList, file, job string) ([]string, error) {
	if len(runsOn) == 0 {
		return nil, badRegistry("%s: job %q has no runs-on", file, job)
	}
	out := make([]string, 0, len(runsOn))
	for _, raw := range runsOn {
		l := strings.ToLower(strings.TrimSpace(raw))
		if expr.Has(l) {
			return nil, badRegistry(
				"%s: job %q has an expression in runs-on; a run is matched to a machine before "+
					"anything is evaluated, so the label has to be written out", file, job)
		}
		// self-hosted is GitHub's marker for "not a hosted runner". Every
		// latem runner is self-hosted, so it carries no information and is
		// dropped rather than demanded of the operator's label set.
		if l == "self-hosted" {
			continue
		}
		if !labelPattern.MatchString(l) {
			return nil, badRegistry("%s: job %q: runs-on label %q must match %s", file, job, raw, labelPattern)
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, badRegistry(
			"%s: job %q has only `self-hosted` in runs-on, which every latem runner is; "+
				"name the labels that distinguish the machine you want", file, job)
	}
	return out, nil
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
func compileStep(st Step, where string, baseEnv map[string]string, shell string, sc expr.Scope) (registry.Step, bool, error) {
	var cond registry.Condition
	if st.If != "" {
		prog, err := conditionProgram(st.If)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
		}
		w, err := classify(prog)
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
	if st.artifact != nil {
		out := registry.Step{ID: st.ID, Name: st.Name, If: cond, Artifact: st.artifact}
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
		out := registry.Step{ID: st.ID, Name: st.Name, Env: env, If: cond, Container: c}
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

	if st.WorkingDirectory != "" {
		dir, err := expr.EvalString(st.WorkingDirectory, sc)
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

	out := registry.Step{ID: st.ID, Name: st.Name, Run: run, Env: env, If: cond}
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

// envValue decides whether an env: value is late. It is late when it is
// exactly one late expression. A value that mixes text with a late expression
// is refused: a late binding is one program, and the honest answer is to ask
// for the expression in a variable of its own rather than to silently split
// the value in two.
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
		w, err := classify(sp.Prog)
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
		return nil, badRegistry(
			"%s: env.%s mixes text with an expression that is only known when the step runs; "+
				"put that expression in a variable of its own and reference it", where, k)
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
		w, err := classify(sp.Prog)
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
func classify(p *expr.Program) (when, error) {
	contexts, funcs := p.References()
	result := whenNow
	for _, c := range contexts {
		if c == secretsContext {
			return whenNow, errSecretsInExpression
		}
		if why, refused := refusedContexts[c]; refused {
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

// errSecretsInExpression is a sentinel so the two callers report it identically.
var errSecretsInExpression = fmt.Errorf(
	"${{ secrets.* }} is not substituted. Declare the secret in the job's secrets: list and " +
		"read it from the environment by its own name — substituting it would write the value " +
		"into the compiled registry, which is persisted (INV-4, RUNNER_SPEC §15)")

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
			w, err := classify(sp.Prog)
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
