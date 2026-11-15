package workflow

import (
	"encoding/json"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// JobVarReads names the variables a job reads, as written (A97): any
// expression in its declaration, steps and conditions included, and in its
// workflow's env, defaults, concurrency and run-name. For a called job, a job
// path, it is the called job's, and the outermost caller job's own, whose
// with: and if: are answered first. computed reports a read no list can name, such as
// vars[matrix.name]. A composite action's own file is not read.
//
// It is what `latem setup` says a job lacks: a variable no store here holds
// reads as empty, which is GitHub's rule and seldom what a job meant.
func JobVarReads(fsys fs.FS, w *Workflow, jobPath string) (names []string, computed bool) {
	r := varReads{names: map[string]bool{}}
	root, _, called := strings.Cut(jobPath, "/")
	j := w.Jobs[root]
	switch {
	case j == nil:
	case !called:
		r.job(w, j)
	default:
		// The caller's with: and if:, and not its workflow's env, which a
		// called workflow does not inherit.
		r.value(j, false)
		if in, key, err := CalledJob(fsys, w, jobPath); err == nil && in.Jobs[key] != nil {
			r.job(in, in.Jobs[key])
		}
	}
	return slices.Sorted(maps.Keys(r.names)), r.computed
}

type varReads struct {
	names    map[string]bool
	computed bool
}

func (r *varReads) job(w *Workflow, j *Job) {
	r.value(w.RunName, false)
	r.value(w.Env, false)
	r.value(w.Defaults, false)
	r.value(w.Concurrency, false)
	r.value(j, false)
}

// value walks v as JSON, reading each string for expressions; an if: is a
// condition, which needs no ${{ }}.
func (r *varReads) value(v any, condition bool) {
	switch t := v.(type) {
	case nil:
		return
	case string:
		r.text(t, condition)
		return
	case map[string]any:
		for k, e := range t {
			r.value(e, k == "if")
		}
		return
	case []any:
		for _, e := range t {
			r.value(e, false)
		}
		return
	case json.RawMessage:
		if len(t) == 0 {
			return
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	var generic any
	if json.Unmarshal(b, &generic) != nil {
		return
	}
	if _, isString := generic.(string); isString && condition {
		r.value(generic, true)
		return
	}
	switch generic.(type) {
	case map[string]any, []any, string:
		r.value(generic, false)
	}
}

func (r *varReads) text(s string, condition bool) {
	if condition && strings.TrimSpace(s) != "" {
		if prog, err := conditionProgram(s); err == nil {
			r.note(prog)
		}
		return
	}
	if !expr.Has(s) {
		return
	}
	spans, err := expr.Spans(s)
	if err != nil {
		return
	}
	for _, sp := range spans {
		r.note(sp.Prog)
	}
}

func (r *varReads) note(prog *expr.Program) {
	keys, computed := prog.ContextKeys("vars")
	contexts, _ := prog.References()
	if computed || (len(keys) == 0 && slices.Contains(contexts, "vars")) {
		r.computed = true
	}
	for _, k := range keys {
		r.names[k] = true
	}
}
