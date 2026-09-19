package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
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

// runtimeContexts are the contexts whose values do not exist until the step
// runs. An expression reaching for one cannot be compiled to a value.
var runtimeContexts = map[string]string{
	"steps":  "a step's outputs exist only once that step has run",
	"job":    "job.status is not known until the job is under way",
	"runner": "the runner context is supplied by the executor at step time",
	"needs":  "latem runs one job per run, so there is nothing for needs to refer to",
}

// runtimeFuncs are the status functions, which answer against a status the
// compiler does not have.
var runtimeFuncs = map[string]bool{"success": true, "failure": true, "cancelled": true}

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
func Compile(w *Workflow, jobName string, sc expr.Scope) (*registry.Job, error) {
	j, ok := w.Jobs[jobName]
	if !ok {
		return nil, &registry.Error{
			Code: protocol.ErrUnknownJob,
			Msg:  fmt.Sprintf("%s declares no job %q; it has %s", w.Path, jobName, strings.Join(w.JobNames(), ", ")),
		}
	}
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
		add("needs:", runtimeContexts["needs"])
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
			found = append(found, fmt.Sprintf(
				"  step %d uses: %s — a local or docker action is A42 phase 4, the marketplace is phase 5; "+
					"vendor the action's command as a run: step for now",
				i+1, st.Uses))
		}
		if st.Run == "" && st.Uses == "" {
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
// because its `if:` evaluated false.
func compileStep(st Step, where string, baseEnv map[string]string, shell string, sc expr.Scope) (registry.Step, bool, error) {
	if st.If != "" {
		runnable, err := conditionIsCompilable(st.If)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
		}
		if !runnable {
			return registry.Step{}, false, badRegistry(
				"%s: if: %s depends on state that does not exist until the run is under way. "+
					"A registry step has no condition, so this cannot be compiled to one — "+
					"move the test into the script the step calls", where, st.If)
		}
		ok, err := expr.Truthy(st.If, sc)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: if: %v", where, err)
		}
		if !ok {
			return registry.Step{}, true, nil
		}
	}

	if st.Shell != "" {
		shell = st.Shell
	}
	if shell != "" && !isSupportedShell(shell) {
		return registry.Step{}, false, badRegistry(
			"%s: shell: %q is not one latem runs; a step is a process, and the shells it may be are bash and sh", where, shell)
	}

	env := map[string]string{}
	for k, v := range baseEnv {
		resolved, err := expr.EvalString(v, sc)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: env.%s: %v", where, k, err)
		}
		env[k] = resolved
	}
	for k, v := range st.Env {
		resolved, err := expr.EvalString(v, sc)
		if err != nil {
			return registry.Step{}, false, badRegistry("%s: env.%s: %v", where, k, err)
		}
		env[k] = resolved
	}

	run, hoisted, err := hoist(st.Run, env, sc, where)
	if err != nil {
		return registry.Step{}, false, err
	}
	for k, v := range hoisted {
		env[k] = v
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

	out := registry.Step{Name: st.Name, Run: run, Env: env}
	if st.TimeoutMins != nil {
		out.Timeout = protocol.Duration(float64(*st.TimeoutMins) * float64(minute))
	}
	if len(env) == 0 {
		out.Env = nil
	}
	return out, false, nil
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
func hoist(run string, existing map[string]string, sc expr.Scope, where string) (string, map[string]string, error) {
	if !expr.Has(run) {
		return run, nil, nil
	}
	spans, err := expr.Spans(run)
	if err != nil {
		return "", nil, badRegistry("%s: run: %v", where, err)
	}

	taken := map[string]bool{}
	for k := range existing {
		taken[k] = true
	}

	hoisted := map[string]string{}
	var b strings.Builder
	last, n := 0, 0
	for _, sp := range spans {
		ok, err := programIsCompilable(sp.Prog)
		if err != nil {
			return "", nil, badRegistry("%s: run: %v", where, err)
		}
		if !ok {
			return "", nil, badRegistry(
				"%s: run: ${{ %s }} depends on state that does not exist until the run is under way; "+
					"write the value into $LATEM_ENV from an earlier step and read the variable instead",
				where, strings.TrimSpace(sp.Prog.Source()))
		}
		v, err := sp.Prog.Eval(sc)
		if err != nil {
			return "", nil, badRegistry("%s: run: ${{ %s }}: %v", where, strings.TrimSpace(sp.Prog.Source()), err)
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
		hoisted[name] = expr.Stringify(v)

		b.WriteString(run[last:sp.Start])
		b.WriteString(`"$` + name + `"`)
		last = sp.End
	}
	b.WriteString(run[last:])
	return b.String(), hoisted, nil
}

// programIsCompilable reports whether every context and function an expression
// reaches for can be answered at admission.
func programIsCompilable(p *expr.Program) (bool, error) {
	contexts, funcs := p.References()
	for _, c := range contexts {
		if c == secretsContext {
			return false, errSecretsInExpression
		}
		if _, runtime := runtimeContexts[c]; runtime {
			return false, nil
		}
	}
	for _, f := range funcs {
		if runtimeFuncs[f] {
			return false, nil
		}
	}
	return true, nil
}

// errSecretsInExpression is a sentinel so the two callers report it identically.
var errSecretsInExpression = fmt.Errorf(
	"${{ secrets.* }} is not substituted. Declare the secret in the job's secrets: list and " +
		"read it from the environment by its own name — substituting it would write the value " +
		"into the compiled registry, which is persisted (INV-4, RUNNER_SPEC §15)")

func conditionIsCompilable(cond string) (bool, error) {
	trimmed := strings.TrimSpace(cond)
	if !expr.Has(trimmed) {
		p, err := expr.Parse(trimmed)
		if err != nil {
			return false, err
		}
		return programIsCompilable(p)
	}
	spans, err := expr.Spans(trimmed)
	if err != nil {
		return false, err
	}
	for _, sp := range spans {
		ok, err := programIsCompilable(sp.Prog)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
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
