package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// maxActionDepth bounds how far a local action may use another. A repository
// is attacker-influenced content at the resolved commit, and an action that
// uses itself through three intermediaries is a loop the cycle check below
// catches; the cap is for the chain that is merely very long.
const maxActionDepth = 8

// containerSpec is a `uses: docker://…` step after expansion and before
// compilation. Its strings are still templates.
type containerSpec struct {
	Image      string
	Entrypoint string
	Args       []string
}

// expander replaces every local and docker:// `uses:` in a job with what it
// becomes (A44): a composite action's steps inlined, or one container step.
// Anything else is left for [Job.unsupported] to refuse by name.
//
// It is a rewrite of the workflow's own step model, done before compilation,
// so that the compiler proper — hoisting, late bindings, conditions — sees
// only steps of the kinds it already understands, and an inlined step is
// exactly as safe as one the author wrote there by hand.
type expander struct {
	loader  ActionLoader
	fetcher RemoteFetcher
	policy  actions.Policy
	// usedRemote records that some step named a repository that is not this
	// one, which is what makes the compiled job require an isolating
	// executor.
	usedRemote bool
	// scope is the admission-time scope, for the one shim that has to
	// evaluate something before any step runs: a cache's mount point.
	scope expr.Scope
	// os is runner.os as the job's labels settle it — "Linux", "macOS" — or
	// "" when they do not (A58).
	os string
	// caches are the declarations `uses: actions/cache` compiled into, in the
	// order first seen and once each (A49).
	caches    []registry.CacheDecl
	cacheSeen map[string]bool
	where     string
	// outputs holds, for each composite `uses:` step that had an id, the
	// program each declared output became. Ids are namespaced, so one map
	// serves every depth.
	outputs map[string]map[string]*expr.Program
	synth   int
	// notes are what expansion learned that the run log should say: the
	// commit a tag resolved to, an input an action did not declare (A51).
	notes []string
	// pre and post are JavaScript actions' hook steps, in the order their
	// actions appear: pre runs before the job's first step, post after its
	// last, in reverse (A51).
	pre, post []Step
}

func newExpander(loader ActionLoader, where string) *expander {
	return &expander{loader: loader, where: where, outputs: map[string]map[string]*expr.Program{}}
}

// expandSteps processes steps in order, because an action's outputs are only
// visible to the steps after it.
func (x *expander) expandSteps(ctx context.Context, steps []Step, stack []string) ([]Step, error) {
	var out []Step
	for i, st := range steps {
		st, err := x.rewriteStep(st, x.applyOutputs)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		expanded, err := x.expand(ctx, st, stack)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i+1, err)
		}
		out = append(out, expanded...)
	}
	return out, nil
}

func (x *expander) expand(ctx context.Context, st Step, stack []string) ([]Step, error) {
	switch {
	case st.Uses == "":
		return []Step{st}, nil

	case len(stack) == 0 && x.skippedOnThisPlatform(st):
		// A58: its if: is false on the platform this job runs on, so the
		// action is never fetched and never asks for isolation. The step
		// stays, with its id, name and if:, and is skipped when it is
		// reached, as GitHub reports it.
		name := st.Name
		if name == "" {
			name = "Run " + st.Uses
		}
		return []Step{{ID: st.ID, Name: name, If: st.If, Run: "true"}}, nil

	case strings.HasPrefix(st.Uses, dockerScheme):
		c, err := x.containerStep(st, strings.TrimPrefix(st.Uses, dockerScheme), nil, nil)
		if err != nil {
			return nil, err
		}
		return []Step{c}, nil

	case strings.HasPrefix(st.Uses, "./"):
		if x.loader == nil {
			return nil, fmt.Errorf("uses: %s is a local action, and there is no checkout here to read it from", st.Uses)
		}
		if len(stack) >= maxActionDepth {
			return nil, fmt.Errorf("uses: %s is nested more than %d actions deep", st.Uses, maxActionDepth)
		}
		action, err := x.loader.Load(st.Uses)
		if err != nil {
			return nil, err
		}
		for _, seen := range stack {
			if seen == action.Path {
				return nil, fmt.Errorf("uses: %s uses itself, through %s", st.Uses, strings.Join(append(stack, action.Path), " → "))
			}
		}
		kind, err := action.kind()
		if err != nil {
			return nil, err
		}
		if kind == actionNode {
			return x.nodeSteps(st, action, nodeOrigin{})
		}
		if kind == actionDocker {
			rewrite, err := x.actionRewrite(st, action, "")
			if err != nil {
				return nil, err
			}
			c, err := x.containerStep(st, strings.TrimPrefix(action.Runs.Image, dockerScheme), action, rewrite)
			if err != nil {
				return nil, err
			}
			return []Step{c}, nil
		}
		return x.inlineComposite(ctx, st, action, append(stack, action.Path))
	}
	// A checkout, which the run loop performs itself (A53). Asked before the
	// allowlist: nothing is fetched and nothing third-party runs, so there is
	// no decision for an operator to make, and requiring it to be allowlisted
	// would fail every workflow on a machine that opted into no actions at all.
	if spec, ok, err := checkoutShim(st, x.scope); err != nil {
		return nil, err
	} else if ok {
		if st.Name == "" {
			st.Name = "Run " + st.Uses
		}
		st.Uses, st.With, st.checkout = "", nil, spec
		return []Step{st}, nil
	}

	// The cache, as a mount (A49). Also before the allowlist, for the same
	// reason as checkout.
	if decls, outputs, ok, err := cacheShim(st, x.scope); err != nil {
		return nil, err
	} else if ok {
		if x.cacheSeen == nil {
			x.cacheSeen = map[string]bool{}
		}
		for _, d := range decls {
			if !x.cacheSeen[d.Name] {
				x.cacheSeen[d.Name] = true
				x.caches = append(x.caches, d)
			}
		}
		if st.ID != "" {
			x.outputs[st.ID] = outputs
		}
		return nil, nil
	}

	// Artifacts, as a step the run loop performs itself (A49). The step
	// survives, because its `if:` is what decides whether it happens.
	if spec, outputs, ok, err := artifactShim(st, x.scope); err != nil {
		return nil, err
	} else if ok {
		st.Uses, st.With, st.artifact = "", nil, spec
		if st.Name == "" {
			if spec.Download {
				st.Name = "download artifact " + spec.Name
			} else {
				st.Name = "upload artifact " + spec.Name
			}
			st.Name = strings.TrimSpace(st.Name)
		}
		if st.ID != "" {
			x.outputs[st.ID] = outputs
		}
		return []Step{st}, nil
	}

	// A remote action: another repository's code, which is the one kind of
	// `uses:` this machine has an opinion about.
	ref, err := actions.ParseRef(st.Uses)
	if err != nil {
		return nil, err
	}
	if err := x.policy.Permits(ref); err != nil {
		return nil, err
	}
	if x.fetcher == nil {
		return nil, fmt.Errorf(
			"uses: %s is allowed here, but this caller has no action cache to fetch it into", ref)
	}
	got, err := x.fetcher.Fetch(ctx, ref)
	if err != nil {
		return nil, err
	}
	x.usedRemote = true
	if !ref.Pinned() {
		// A tag can be moved by whoever owns it; the commit it named for this
		// run is what anyone reading the log later needs (A51).
		x.notes = append(x.notes, fmt.Sprintf("uses: %s resolved to %s", st.Uses, got.SHA))
	}

	action, err := dirLoader{dir: got.Dir, rel: got.Rel}.Load(st.Uses)
	if err != nil {
		return nil, err
	}
	kind, err := action.kind()
	if err != nil {
		return nil, err
	}
	if kind == actionNode {
		return x.nodeSteps(st, action, originOf(ref))
	}
	if kind == actionDocker {
		rewrite, err := x.actionRewrite(st, action, "")
		if err != nil {
			return nil, err
		}
		c, err := x.containerStep(st, strings.TrimPrefix(action.Runs.Image, dockerScheme), action, rewrite)
		if err != nil {
			return nil, err
		}
		return []Step{c}, nil
	}
	// A remote composite's own `uses: ./path` would be relative to *its*
	// repository, not to the workspace, and resolving that needs a loader
	// rooted at the fetched tree rather than at the checkout. Refused by name
	// until it is, rather than silently reading a path out of the wrong repo.
	for i, inner := range action.Runs.Steps {
		if strings.HasPrefix(inner.Uses, "./") || strings.HasPrefix(inner.Uses, "../") {
			return nil, fmt.Errorf(
				"uses: %s step %d uses %s, a path inside its own repository, which is not implemented yet",
				ref, i+1, inner.Uses)
		}
	}
	return x.inlineComposite(ctx, st, action, append(stack, got.Rel))
}

// RemoteFetcher resolves and fetches an action from another repository.
// *actions.Cache is the implementation; the interface is here so a compiler
// test needs no network.
type RemoteFetcher interface {
	Fetch(ctx context.Context, r actions.Ref) (actions.Resolved, error)
}

// dirLoader reads a fetched action's metadata from an absolute directory.
//
// Unlike [rootLoader] it is not scoped to the workspace, because a fetched
// action is not in the workspace. It does not need to be: the path comes from
// the cache's own layout, every element of which this agent built from values
// [actions.ParseRef] had already constrained.
type dirLoader struct {
	dir string
	// rel is the action's path relative to the cache root, which is what
	// `github.action_path` resolves against at run time.
	rel string
}

func (l dirLoader) Load(uses string) (*Action, error) {
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(l.dir, name)) //#nosec G304 -- a path this agent built under its own cache root from a validated reference
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, badRegistry("reading %s of %s: %v", name, uses, err)
		}
		a, err := ParseAction(data, l.rel)
		if err != nil {
			return nil, err
		}
		a.Remote = true
		return a, nil
	}
	return nil, badRegistry("uses: %s has no action.yml or action.yaml at the commit it resolved to", uses)
}

// inlineComposite returns the action's steps, rewritten to stand where the
// `uses:` step stood.
func (x *expander) inlineComposite(ctx context.Context, outer Step, action *Action, stack []string) ([]Step, error) {
	ns := outer.ID
	if ns == "" {
		// Inner steps still refer to each other, so they still need a
		// namespace; one nobody can name from outside is the right one for a
		// step nobody gave a name to.
		x.synth++
		ns = "_uses" + strconv.Itoa(x.synth)
	}
	rewrite, err := x.actionRewrite(outer, action, ns)
	if err != nil {
		return nil, err
	}

	var outerCond *expr.Program
	if outer.If != "" {
		if outerCond, err = conditionProgram(outer.If); err != nil {
			return nil, fmt.Errorf("if: %w", err)
		}
	}

	inner := make([]Step, 0, len(action.Runs.Steps))
	for i, st := range action.Runs.Steps {
		at := fmt.Sprintf("%s step %d", action.Path, i+1)
		if st.Run != "" && st.Shell == "" {
			return nil, fmt.Errorf("%s: a composite action's run step must name its shell", at)
		}
		if st, err = x.rewriteStep(st, rewrite); err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		if st.ID != "" {
			st.ID = ns + "/" + st.ID
		}
		st.Name = inlinedName(outer, st, i)
		st.fromAction = true
		st.Env = mergeEnv(outer.Env, st.Env)
		if st.WorkingDirectory == "" {
			st.WorkingDirectory = outer.WorkingDirectory
		}
		if st.TimeoutMins == nil {
			st.TimeoutMins = outer.TimeoutMins
		}
		// The condition on the `uses:` step governs every step it became.
		if outerCond != nil {
			cond := outerCond
			if st.If != "" {
				own, err := conditionProgram(st.If)
				if err != nil {
					return nil, fmt.Errorf("%s: if: %w", at, err)
				}
				cond = expr.And(outerCond, own)
			}
			st.If = cond.Wrap()
		}
		inner = append(inner, st)
	}

	// Nested actions, and the outputs of nested actions, in order.
	expanded, err := x.expandSteps(ctx, inner, stack)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action.Path, err)
	}

	// The action's own outputs, in the caller's terms: inner ids namespaced,
	// inputs replaced, and any nested action's outputs already resolved — so
	// what is stored needs no further work when a later step refers to it.
	declared := map[string]*expr.Program{}
	for _, name := range sortedNames(action.Outputs) {
		prog, err := expr.Template(action.Outputs[name].Value)
		if err != nil {
			return nil, fmt.Errorf("%s: outputs.%s: %w", action.Path, name, err)
		}
		if prog, err = rewrite(prog); err != nil {
			return nil, fmt.Errorf("%s: outputs.%s: %w", action.Path, name, err)
		}
		if prog, err = x.applyOutputs(prog); err != nil {
			return nil, fmt.Errorf("%s: outputs.%s: %w", action.Path, name, err)
		}
		declared[name] = prog
	}
	if outer.ID != "" {
		x.outputs[outer.ID] = declared
	}
	return expanded, nil
}

// actionRewrite builds the expression rewrite for one use of one action.
//
// Order matters and is the subtle part. The action's own `steps.x` references
// are namespaced *first*, and only then is `inputs.x` replaced by what the
// caller wrote — because the caller's expression may itself mention
// `steps.prev`, which is the caller's step and must not be moved into the
// action's namespace. Substitution does not descend into what it inserts, so
// doing it second is what keeps the two apart.
func (x *expander) actionRewrite(outer Step, action *Action, ns string) (func(*expr.Program) (*expr.Program, error), error) {
	with, err := withStrings(outer.With)
	if err != nil {
		return nil, err
	}
	inputs := map[string]*expr.Program{}
	for _, name := range sortedNames(action.Inputs) {
		in := action.Inputs[name]
		var src string
		switch v, given := with[name]; {
		case given:
			src = v
		case in.Default != nil:
			src = string(*in.Default)
		case in.Required:
			return nil, fmt.Errorf("uses: %s requires the input %q", outer.Uses, name)
		}
		prog, err := expr.Template(src)
		if err != nil {
			return nil, fmt.Errorf("with.%s: %w", name, err)
		}
		inputs[strings.ToLower(name)] = prog
	}
	lookup := func(name string) (*expr.Program, error) {
		if p, ok := inputs[strings.ToLower(name)]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("%s declares no input %q", action.Path, name)
	}
	// A local action's path is relative to the workspace, which is where a
	// step starts, so it is a literal. A fetched action's is relative to a
	// cache whose location only the executor knows, so it is resolved at step
	// time against `latem.action_dir` — which makes every expression carrying
	// it late, through the classifier that already exists.
	actionPath := expr.Literal(action.Path)
	if action.Remote {
		p, err := expr.Parse(fmt.Sprintf("format('{0}/{1}', latem.action_dir, '%s')",
			strings.ReplaceAll(action.Path, "'", "''")))
		if err != nil {
			return nil, err
		}
		actionPath = p
	}

	return func(p *expr.Program) (*expr.Program, error) {
		if ns != "" {
			p = p.RenameSteps(func(id string) string { return ns + "/" + id })
		}
		p, err := p.Substitute("inputs", lookup)
		if err != nil {
			return nil, err
		}
		return p.SubstituteField("github", "action_path", actionPath), nil
	}, nil
}

// applyOutputs replaces references to an earlier composite action's outputs.
func (x *expander) applyOutputs(p *expr.Program) (*expr.Program, error) {
	ids := make([]string, 0, len(x.outputs))
	for id := range x.outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		declared := x.outputs[id]
		var err error
		p, err = p.SubstituteOutputs(id, func(name string) (*expr.Program, error) {
			if prog, ok := declared[name]; ok {
				return prog, nil
			}
			return nil, fmt.Errorf("steps.%s.outputs.%s: the action declares no output %q", id, name, name)
		})
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// rewriteStep applies f to every expression a step carries.
func (x *expander) rewriteStep(st Step, f func(*expr.Program) (*expr.Program, error)) (Step, error) {
	var err error
	tmpl := func(field string, s *string) {
		if err != nil || *s == "" {
			return
		}
		var out string
		if out, err = expr.RewriteTemplate(*s, f); err != nil {
			err = fmt.Errorf("%s: %w", field, err)
			return
		}
		*s = out
	}
	tmpl("run", &st.Run)
	tmpl("working-directory", &st.WorkingDirectory)

	if err == nil && st.If != "" {
		var prog *expr.Program
		if prog, err = conditionProgram(st.If); err == nil {
			if prog, err = f(prog); err == nil {
				st.If = prog.Wrap()
			}
		}
		if err != nil {
			err = fmt.Errorf("if: %w", err)
		}
	}

	if len(st.Env) > 0 {
		env := make(map[string]string, len(st.Env))
		for k, v := range st.Env {
			tmpl("env."+k, &v)
			env[k] = v
		}
		st.Env = env
	}

	if err == nil && len(st.With) > 0 {
		var with map[string]any
		if err = json.Unmarshal(st.With, &with); err == nil {
			for k, v := range with {
				switch t := v.(type) {
				case string:
					tmpl("with."+k, &t)
					with[k] = t
				case []any:
					for i, e := range t {
						if s, ok := e.(string); ok {
							tmpl("with."+k, &s)
							t[i] = s
						}
					}
				}
			}
			if err == nil {
				st.With, err = json.Marshal(with)
			}
		}
	}

	if st.container != nil && err == nil {
		c := *st.container
		tmpl("image", &c.Image)
		tmpl("entrypoint", &c.Entrypoint)
		c.Args = append([]string(nil), c.Args...)
		for i := range c.Args {
			tmpl("args", &c.Args[i])
		}
		st.container = &c
	}
	return st, err
}

var inputEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// containerStep turns a docker:// use into one container step. action and
// rewrite are nil for a bare `uses: docker://image`.
func (x *expander) containerStep(outer Step, image string, action *Action, rewrite func(*expr.Program) (*expr.Program, error)) (Step, error) {
	with, err := withValues(outer.With)
	if err != nil {
		return Step{}, err
	}
	if outer.WorkingDirectory != "" || outer.Shell != "" {
		return Step{}, fmt.Errorf("uses: %s — a container step has no shell and no working-directory of its own", outer.Uses)
	}

	c := &containerSpec{Image: image}
	env := map[string]string{}
	if action != nil {
		c.Entrypoint = action.Runs.Entrypoint
		c.Args = append([]string(nil), action.Runs.Args...)
		for k, v := range action.Runs.Env {
			env[k] = v
		}
	}

	// `with.args` is a list, or GitHub's single string, which is split the way
	// a shell would split it and then never seen by a shell: each word is one
	// argv element from here on.
	switch args := with["args"].(type) {
	case nil:
	case string:
		if c.Args, err = splitArgs(args); err != nil {
			return Step{}, fmt.Errorf("with.args: %w", err)
		}
	case []any:
		c.Args = c.Args[:0]
		for _, a := range args {
			c.Args = append(c.Args, scalarString(a))
		}
	default:
		return Step{}, fmt.Errorf("with.args must be a string or a list")
	}
	if ep, ok := with["entrypoint"].(string); ok && ep != "" {
		c.Entrypoint = ep
	}

	// Inputs reach a container as INPUT_<NAME>, as they do on GitHub.
	inputs := map[string]string{}
	if action != nil {
		for name, in := range action.Inputs {
			if in.Default != nil {
				inputs[name] = string(*in.Default)
			}
		}
	}
	for k, v := range with {
		if k == "args" || k == "entrypoint" {
			continue
		}
		inputs[k] = scalarString(v)
	}
	if action != nil {
		for name, in := range action.Inputs {
			if _, ok := inputs[name]; !ok && in.Required {
				return Step{}, fmt.Errorf("uses: %s requires the input %q", outer.Uses, name)
			}
		}
	}
	for name, v := range inputs {
		key := "INPUT_" + strings.ToUpper(strings.ReplaceAll(name, " ", "_"))
		if !inputEnvName.MatchString(key) {
			// GitHub would export INPUT_REPO-TOKEN. That is not a name an
			// environment map validated by §9's grammar can hold, and
			// exporting it under a different one would hand the action an
			// empty input it has no way to notice.
			return Step{}, fmt.Errorf(
				"with.%s would be exported as %s, which is not a valid environment name here; "+
					"pass it through args or env instead", name, key)
		}
		env[key] = v
	}

	st := outer
	st.Uses, st.With, st.Run = "", nil, ""
	st.Env = mergeEnv(env, outer.Env)
	st.container = c
	if st.Name == "" {
		st.Name = outer.Uses
	}
	if rewrite != nil {
		// Only what came from the action is in the action's terms; the outer
		// step's own fields were the caller's and have been rewritten already.
		inner := Step{Env: env, container: c}
		if inner, err = x.rewriteStep(inner, rewrite); err != nil {
			return Step{}, err
		}
		st.Env = mergeEnv(inner.Env, outer.Env)
		st.container = inner.container
	}
	return st, nil
}

func inlinedName(outer, inner Step, i int) string {
	base := outer.Name
	if base == "" {
		base = outer.Uses
	}
	if inner.Name != "" {
		return base + " › " + inner.Name
	}
	return fmt.Sprintf("%s › step %d", base, i+1)
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// withValues decodes a `with:` block.
func withValues(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("with: %w", err)
	}
	return m, nil
}

// withStrings is withValues with every scalar rendered as the string GitHub
// would pass an action.
func withStrings(raw json.RawMessage) (map[string]string, error) {
	m, err := withValues(raw)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = scalarString(v)
	}
	return out, nil
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// splitArgs splits a string into words: whitespace separates, single and
// double quotes group, and a backslash escapes inside double quotes or bare.
// Nothing is expanded — no variables, no globs, no substitution — because the
// result is an argv and is never handed to a shell.
//
// An expression is atomic: `${{ … }}` is copied into the current word whole,
// wherever it appears, and is evaluated later, inside that word. So whatever an
// expression evaluates to is part of exactly one argument. GitHub substitutes
// first and splits afterwards, which lets a value containing a space become
// several arguments — and a pull request title of `x --config=/etc/passwd`
// become a flag. Splitting first is stricter than GitHub and deliberately so:
// it is INV-3's argv discipline applied to the one place an argv is written as
// a string.
func splitArgs(s string) ([]string, error) {
	spans, err := expr.Spans(s)
	if err != nil {
		return nil, err
	}
	spanAt := make(map[int]int, len(spans))
	for _, sp := range spans {
		spanAt[sp.Start] = sp.End
	}

	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		if end, ok := spanAt[i]; ok && quote != '\'' {
			cur.WriteString(s[i:end])
			inWord = true
			i = end - 1
			continue
		}
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// pureFunctions are the expression functions whose answer depends on their
// arguments alone. A status function, or hashFiles, cannot be answered before
// the step is reached.
var pureFunctions = map[string]bool{
	"contains": true, "startswith": true, "endswith": true, "format": true,
	"join": true, "tojson": true, "fromjson": true,
}

// skippedOnThisPlatform reports whether st's if: is false wherever this job
// can run (A58). It answers only for a condition that reads runner.os and
// the matrix leg, both settled before the job is placed, through functions
// that depend on their arguments alone; anything else is left to the run.
//
// GitHub evaluates the condition when the step is reached. Its answer cannot
// differ: a job's runner, and so its runner.os, does not change mid-job, and
// a condition with no status function is success() && it, which a false it
// makes false.
func (x *expander) skippedOnThisPlatform(st Step) bool {
	if x.os == "" || strings.TrimSpace(st.If) == "" {
		return false
	}
	prog, err := conditionProgram(st.If)
	if err != nil {
		return false
	}
	contexts, funcs := prog.References()
	for _, c := range contexts {
		if c != "runner" && c != "matrix" {
			return false
		}
	}
	for _, f := range funcs {
		if !pureFunctions[f] {
			return false
		}
	}
	keys, computed := prog.ContextKeys("runner")
	if computed {
		return false
	}
	for _, k := range keys {
		if !strings.EqualFold(k, "os") {
			return false
		}
	}
	v, err := prog.Eval(expr.Scope{Contexts: map[string]expr.Value{
		"runner": map[string]any{"os": x.os},
		"matrix": x.scope.Contexts["matrix"],
	}})
	return err == nil && !expr.IsTruthy(v)
}

// osOfLabels is runner.os for a job whose labels say which platform it needs:
// macos or linux, which the compiler itself produces for GitHub's hosted
// labels, and an executor carries only on its own platform.
func osOfLabels(labels []string) string {
	mac, linux := slices.Contains(labels, "macos"), slices.Contains(labels, "linux")
	switch {
	case mac && !linux:
		return "macOS"
	case linux && !mac:
		return "Linux"
	}
	return ""
}
