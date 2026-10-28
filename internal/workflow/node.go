package workflow

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// NodeRun is the whole of what a JavaScript action's step runs (A51). It is
// latem's text and it is constant: the action's directory and entry file
// arrive as environment values, so nothing an action's metadata says can
// become command text (INV-3). GITHUB_ACTION_PATH is made absolute here
// because a local action's directory is relative to the workspace, which is
// where the step starts. Exported so the executors' tests can run it for real.
const NodeRun = `if ! command -v node >/dev/null 2>&1; then
  echo "latem: ${GITHUB_ACTION_REPOSITORY:-this action} is a JavaScript action, and there is no node on this step's PATH" >&2
  exit 127
fi
GITHUB_ACTION_PATH="$(cd "$GHA_ACTION_DIR" && pwd)"
export GITHUB_ACTION_PATH
exec node "$GITHUB_ACTION_PATH/$GHA_ACTION_ENTRY"`

// Environment keys of a JavaScript action's step that are latem's rather than
// the action's.
const (
	actionDirVar   = "GHA_ACTION_DIR"
	actionEntryVar = "GHA_ACTION_ENTRY"
)

// nodeOrigin is what a JavaScript action's steps say about where the action
// came from, for GITHUB_ACTION_REPOSITORY and GITHUB_ACTION_REF.
type nodeOrigin struct {
	repository string
	ref        string
}

// nodeSteps is what a JavaScript action becomes (A51): its main step, standing
// where the `uses:` stood with the same id, name and condition so that
// `steps.<id>.outputs` reads what the action wrote to $GITHUB_OUTPUT.
func (x *expander) nodeSteps(outer Step, action *Action, origin nodeOrigin) ([]Step, error) {
	inputs, err := x.nodeInputs(outer, action)
	if err != nil {
		return nil, err
	}

	dir := action.Path
	if action.Remote {
		// Only the executor knows where the action cache is reachable from
		// inside the step, so the directory is late (A42 phase 5).
		dir = fmt.Sprintf("${{ format('{0}/{1}', latem.action_dir, '%s') }}",
			strings.ReplaceAll(action.Path, "'", "''"))
	}

	env := mergeEnv(inputs, outer.Env)
	env[actionDirVar] = dir
	env[actionEntryVar] = action.Runs.Main
	if origin.repository != "" {
		env["GITHUB_ACTION_REPOSITORY"] = origin.repository
		env["GITHUB_ACTION_REF"] = origin.ref
	}
	if outer.ID != "" {
		env["GITHUB_ACTION"] = outer.ID
	}

	name := outer.Name
	if name == "" {
		name = "Run " + outer.Uses
	}
	x.synth++
	use := "_action" + strconv.Itoa(x.synth)
	main := Step{
		ID:              outer.ID,
		Name:            name,
		If:              outer.If,
		Run:             NodeRun,
		Shell:           "bash",
		Env:             env,
		TimeoutMins:     outer.TimeoutMins,
		ContinueOnError: outer.ContinueOnError,
		fromAction:      true,
		actionUse:       use,
	}

	hook := func(label, entry, cond string) Step {
		e := maps.Clone(env)
		e[actionEntryVar] = entry
		return Step{
			Name: label + " " + name, If: cond, Run: NodeRun, Shell: "bash", Env: e,
			fromAction: true, actionUse: use,
		}
	}

	if action.Runs.Pre != "" {
		// pre-if alone, default always(): pre runs before any step, so the
		// step's own condition — which may read steps that have not run yet —
		// cannot be asked.
		x.pre = append(x.pre, hook("Pre", action.Runs.Pre, orAlways(action.Runs.PreIf)))
	}
	if action.Runs.Post != "" {
		// post runs only if main ran, which is when GitHub registers it at
		// all. Main needs an id for the condition to name.
		if main.ID == "" {
			main.ID = use
		}
		cond, err := postCondition(action.Runs.PostIf, main.ID)
		if err != nil {
			return nil, fmt.Errorf("uses: %s: post-if: %w", outer.Uses, err)
		}
		x.post = append(x.post, hook("Post", action.Runs.Post, cond))
	}
	return []Step{main}, nil
}

// orAlways is a hook's condition, defaulting to always() as GitHub does.
func orAlways(cond string) string {
	if strings.TrimSpace(cond) == "" {
		return "always()"
	}
	return cond
}

// postCondition is post-if, default always(), and only if the main step with
// id ran — its outcome is anything but skipped.
func postCondition(postIf, id string) (string, error) {
	user, err := conditionProgram(orAlways(postIf))
	if err != nil {
		return "", err
	}
	ran, err := expr.Parse(fmt.Sprintf("steps['%s'].outcome != 'skipped'", strings.ReplaceAll(id, "'", "''")))
	if err != nil {
		return "", err
	}
	return expr.And(user, ran).Wrap(), nil
}

// nodeInputs is every input as the action reads it: INPUT_<NAME>, upper-cased
// with spaces made underscores and hyphens kept, which is what @actions/core
// looks up (A51). A declared input takes the caller's value, else its
// default; a required one with neither is refused. An undeclared `with:` key
// is passed as well, as GitHub passes it, and noted.
//
// Values are templates and are left as written: the compiler evaluates them
// like any env value, so a secret or a step output in one is late (A50).
func (x *expander) nodeInputs(outer Step, action *Action) (map[string]string, error) {
	with, err := withStrings(outer.With)
	if err != nil {
		return nil, err
	}
	given := make(map[string]string, len(with))
	for k, v := range with {
		given[strings.ToLower(k)] = v
	}

	env := map[string]string{}
	declared := map[string]bool{}
	for _, name := range sortedNames(action.Inputs) {
		in := action.Inputs[name]
		declared[strings.ToLower(name)] = true
		switch v, ok := given[strings.ToLower(name)]; {
		case ok:
			env[nodeInputVar(name)] = v
		case in.Default != nil:
			env[nodeInputVar(name)] = *in.Default
		case in.Required:
			return nil, fmt.Errorf("uses: %s requires the input %q", outer.Uses, name)
		}
	}

	var extra []string
	for k, v := range with {
		if !declared[strings.ToLower(k)] {
			env[nodeInputVar(k)] = v
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		x.notes = append(x.notes, fmt.Sprintf("uses: %s does not declare the input(s) %s; passed anyway, as GitHub does",
			outer.Uses, strings.Join(extra, ", ")))
	}
	return env, nil
}

// nodeInputVar is GitHub's spelling of an input as an environment variable.
func nodeInputVar(name string) string {
	return "INPUT_" + strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), " ", "_"))
}

// originOf is a fetched action's repository and ref, for the step's
// environment.
func originOf(r actions.Ref) nodeOrigin {
	return nodeOrigin{repository: r.Owner + "/" + r.Repo, ref: r.Ref}
}
