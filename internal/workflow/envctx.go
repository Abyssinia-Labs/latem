package workflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// envContext is what ${{ env.X }} reads (A50): base, then each layer of env:
// over it — the workflow's, the job's, a step's — each value answered where it
// can be now.
//
// A value that can only be answered when the step runs (a step output, the
// job status) or not at all here (a secret) is left out rather than guessed:
// GitHub would give it a value, latem cannot yet, and an empty string in its
// place would be a wrong answer that looks like a right one. The variable
// itself still reaches the step; only the expression reading it is affected.
func envContext(sc expr.Scope, base map[string]any, layers ...map[string]string) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for _, layer := range layers {
		for _, k := range sortedKeys(layer) {
			v := layer[k]
			if !expr.Has(v) {
				out[k] = v
				continue
			}
			if !answerableNow(v, sc) {
				delete(out, k)
				continue
			}
			s, err := expr.EvalString(v, withEnv(sc, out))
			if err != nil {
				delete(out, k)
				continue
			}
			out[k] = s
		}
	}
	return out
}

// answerableNow reports whether every expression in v can be evaluated at
// admission.
func answerableNow(v string, sc expr.Scope) bool {
	spans, err := expr.Spans(v)
	if err != nil {
		return false
	}
	for _, sp := range spans {
		w, err := classify(sp.Prog, sc)
		if err != nil || w == whenLate {
			return false
		}
	}
	return true
}

// withEnv is sc with its env context replaced.
func withEnv(sc expr.Scope, env map[string]any) expr.Scope {
	return withContext(sc, "env", maps.Clone(env))
}

// withContext is sc with one context replaced. The scope is copied, not
// edited: one scope serves every job and step compiled from it.
func withContext(sc expr.Scope, name string, value map[string]any) expr.Scope {
	contexts := make(map[string]expr.Value, len(sc.Contexts)+1)
	maps.Copy(contexts, sc.Contexts)
	contexts[name] = value
	out := sc
	out.Contexts = contexts
	return out
}

func stringsToAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// compileContinueOnError reads a step's continue-on-error: a boolean, the
// strings "true" and "false", or one expression answered now (A50).
//
// An expression that can only be answered when the step runs is refused: the
// run loop decides what a failure means the moment the step ends, and a
// condition it has to evaluate then is a second `if:` nobody asked for.
func compileContinueOnError(raw json.RawMessage, sc expr.Scope) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return false, fmt.Errorf("expected true, false or an expression, got %s", string(raw))
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
	w, err := classify(prog, sc)
	if err != nil {
		return false, err
	}
	if w == whenLate {
		return false, fmt.Errorf("${{ %s }} is only known when the step runs; continue-on-error has to be "+
			"decided before it does", strings.TrimSpace(prog.Source()))
	}
	v, err := prog.Eval(sc)
	if err != nil {
		return false, err
	}
	return expr.IsTruthy(v), nil
}
