package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v2"

	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// MaxMatrixLegs is GitHub's limit on the jobs one matrix may produce.
const MaxMatrixLegs = 256

// matrixOrder is the order a matrix's keys are written in, and each include
// entry's (A55). A leg's name lists its values in this order, and the cross
// product varies the first key slowest, so a leg's name and position both
// depend on what decoding through JSON — which sorts keys — throws away.
type matrixOrder struct {
	keys    []string
	include [][]string
}

// readMatrixOrders reads, for each job with a strategy.matrix mapping, the
// order of its keys, from the file itself. A file this cannot read is one
// [Parse] has already refused or accepted on its own terms, so a failure here
// leaves the order to the fallback: sorted.
func readMatrixOrders(data []byte) map[string]*matrixOrder {
	var doc yaml.MapSlice
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	jobs, _ := mapItem(doc, "jobs").(yaml.MapSlice)
	out := map[string]*matrixOrder{}
	for _, job := range jobs {
		def, _ := job.Value.(yaml.MapSlice)
		strategy, _ := mapItem(def, "strategy").(yaml.MapSlice)
		matrix, ok := mapItem(strategy, "matrix").(yaml.MapSlice)
		if !ok {
			continue
		}
		order := &matrixOrder{}
		for _, item := range matrix {
			switch k := fmt.Sprint(item.Key); k {
			case "include":
				entries, _ := item.Value.([]any)
				for _, e := range entries {
					var keys []string
					if m, ok := e.(yaml.MapSlice); ok {
						for _, it := range m {
							keys = append(keys, fmt.Sprint(it.Key))
						}
					}
					order.include = append(order.include, keys)
				}
			case "exclude":
			default:
				order.keys = append(order.keys, k)
			}
		}
		out[fmt.Sprint(job.Key)] = order
	}
	return out
}

func mapItem(m yaml.MapSlice, key string) any {
	for _, item := range m {
		if fmt.Sprint(item.Key) == key {
			return item.Value
		}
	}
	return nil
}

// matrixLeg is one combination of a matrix.
type matrixLeg struct {
	values map[string]any
	// order is the leg's keys in the order its name lists their values: the
	// matrix's own keys as written, then those include added.
	order []string
}

// matrixStrategy is a job's strategy, expanded.
type matrixStrategy struct {
	legs        []matrixLeg
	failFast    bool
	maxParallel int
}

// expandStrategy expands a job's strategy.matrix by GitHub's rules (A55):
// the cross product of its keys, first key outermost; then exclude, which
// removes every combination matching all of an entry's keys; then include,
// in order, which extends each combination whose original values an entry
// matches — never overwriting an original value — or, matching none, is a
// combination of its own. Nil for a job with no matrix.
//
// Values may be expressions the plan's scope answers. A matrix that reads
// needs is computed by another job, and is refused: neither the plan nor the
// machine compiling a leg could expand it the same way twice.
func expandStrategy(j *Job, sc expr.Scope) (*matrixStrategy, error) {
	if len(j.Strategy) == 0 || string(j.Strategy) == "null" {
		return nil, nil
	}
	var raw struct {
		Matrix      json.RawMessage `json:"matrix"`
		FailFast    json.RawMessage `json:"fail-fast"`
		MaxParallel json.RawMessage `json:"max-parallel"`
	}
	dec := json.NewDecoder(bytes.NewReader(j.Strategy))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("strategy: expected matrix, fail-fast and max-parallel: %w", err)
	}
	if len(raw.Matrix) == 0 || string(raw.Matrix) == "null" {
		return nil, nil
	}

	st := &matrixStrategy{failFast: true}
	if len(raw.FailFast) > 0 {
		v, err := planBool(raw.FailFast, sc)
		if err != nil {
			return nil, fmt.Errorf("strategy.fail-fast: %w", err)
		}
		st.failFast = v
	}
	if len(raw.MaxParallel) > 0 {
		v, err := matrixValue(raw.MaxParallel, sc)
		if err != nil {
			return nil, fmt.Errorf("strategy.max-parallel: %w", err)
		}
		if s, ok := v.(string); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return nil, fmt.Errorf("strategy.max-parallel: %q is not a number", s)
			}
			v = n
		}
		n, ok := v.(float64)
		if !ok || n < 1 || n != float64(int(n)) {
			return nil, fmt.Errorf("strategy.max-parallel: %v is not a whole number of jobs", v)
		}
		st.maxParallel = int(n)
	}

	m, err := matrixValue(raw.Matrix, sc)
	if err != nil {
		return nil, fmt.Errorf("strategy.matrix: %w", err)
	}
	matrix, ok := m.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("strategy.matrix: expected a mapping, got %s", describe(m))
	}
	order := j.matrixOrder
	if order == nil {
		order = &matrixOrder{}
	}
	if st.legs, err = expandMatrix(matrix, order); err != nil {
		return nil, fmt.Errorf("strategy.matrix: %w", err)
	}
	return st, nil
}

func expandMatrix(matrix map[string]any, order *matrixOrder) ([]matrixLeg, error) {
	dims := dimensionOrder(matrix, order.keys)
	combos := []matrixLeg{{values: map[string]any{}}}
	for _, k := range dims {
		values, ok := matrix[k].([]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a list of values, got %s", k, describe(matrix[k]))
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("%s has no values", k)
		}
		// Refused before the round is built, not after: a list long enough to
		// pass the cap on its own would otherwise be crossed with every leg so
		// far first. Each factor is at most MaxMatrixLegs once past the first
		// two tests, so the product cannot overflow.
		if len(values) > MaxMatrixLegs || len(combos) > MaxMatrixLegs || len(combos)*len(values) > MaxMatrixLegs {
			return nil, fmt.Errorf("it expands to more than %d jobs", MaxMatrixLegs)
		}
		next := make([]matrixLeg, 0, len(combos)*len(values))
		for _, c := range combos {
			for _, v := range values {
				leg := matrixLeg{values: cloneValues(c.values), order: append(slices.Clone(c.order), k)}
				leg.values[k] = v
				next = append(next, leg)
			}
		}
		combos = next
	}
	if len(dims) == 0 {
		combos = nil
	}

	excludes, err := matrixEntries(matrix["exclude"], "exclude")
	if err != nil {
		return nil, err
	}
	for i, ex := range excludes {
		for k := range ex {
			if !slices.Contains(dims, k) {
				return nil, fmt.Errorf("exclude[%d]: %q is not one of the matrix's keys (%s)", i, k, strings.Join(dims, ", "))
			}
		}
	}
	combos = slices.DeleteFunc(combos, func(c matrixLeg) bool {
		return slices.ContainsFunc(excludes, func(ex map[string]any) bool { return matches(c.values, ex, nil) })
	})

	includes, err := matrixEntries(matrix["include"], "include")
	if err != nil {
		return nil, err
	}
	original := len(combos)
	for i, in := range includes {
		keys := entryOrder(in, order.include, i)
		added := false
		for c := range combos[:original] {
			if !matches(combos[c].values, in, dims) {
				continue
			}
			added = true
			for _, k := range keys {
				if !slices.Contains(combos[c].order, k) {
					combos[c].order = append(combos[c].order, k)
				}
				combos[c].values[k] = in[k]
			}
		}
		if !added {
			combos = append(combos, matrixLeg{values: cloneValues(in), order: keys})
		}
	}

	switch {
	case len(combos) == 0:
		return nil, fmt.Errorf("it has no combinations once exclude is applied")
	case len(combos) > MaxMatrixLegs:
		return nil, fmt.Errorf("it expands to %d jobs, more than %d", len(combos), MaxMatrixLegs)
	}
	return combos, nil
}

// dimensionOrder is the matrix's keys other than include and exclude, in the
// file's order; a key the file's order does not know — a matrix computed by
// an expression — follows, sorted.
func dimensionOrder(matrix map[string]any, written []string) []string {
	var dims []string
	for _, k := range written {
		if _, ok := matrix[k]; ok && k != "include" && k != "exclude" {
			dims = append(dims, k)
		}
	}
	var rest []string
	for k := range matrix {
		if k != "include" && k != "exclude" && !slices.Contains(dims, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(dims, rest...)
}

// entryOrder is an include entry's keys in the file's order, then any the
// file's order does not know, sorted.
func entryOrder(entry map[string]any, written [][]string, i int) []string {
	var keys []string
	if i < len(written) {
		for _, k := range written[i] {
			if _, ok := entry[k]; ok {
				keys = append(keys, k)
			}
		}
	}
	var rest []string
	for k := range entry {
		if !slices.Contains(keys, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// matches reports whether a combination agrees with an entry. Only the keys
// in among count when among is non-nil: include matches on the matrix's own
// keys and adds the rest, exclude on every key it names.
func matches(combo, entry map[string]any, among []string) bool {
	for k, v := range entry {
		if among != nil && !slices.Contains(among, k) {
			continue
		}
		have, ok := combo[k]
		if !ok || !reflect.DeepEqual(have, v) {
			return false
		}
	}
	return true
}

func matrixEntries(v any, what string) ([]map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected a list of mappings, got %s", what, describe(v))
	}
	out := make([]map[string]any, 0, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok || len(m) == 0 {
			return nil, fmt.Errorf("%s[%d]: expected a mapping, got %s", what, i, describe(e))
		}
		out = append(out, m)
	}
	return out, nil
}

// matrixValue decodes a part of a matrix and evaluates every expression in
// it against the plan's scope. A string that is exactly one expression keeps
// the expression's type, so `${{ fromJSON(inputs.targets) }}` is a list.
func matrixValue(raw json.RawMessage, sc expr.Scope) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return evalMatrix(v, sc)
}

func evalMatrix(v any, sc expr.Scope) (any, error) {
	switch t := v.(type) {
	case string:
		if !expr.Has(t) {
			return t, nil
		}
		spans, err := expr.Spans(t)
		if err != nil {
			return nil, err
		}
		for _, sp := range spans {
			if contexts, _ := sp.Prog.References(); slices.Contains(contexts, "needs") {
				return nil, fmt.Errorf("${{ %s }} reads needs: a matrix computed by another job is not "+
					"planned yet (A55)", strings.TrimSpace(sp.Prog.Source()))
			}
			if err := planReads(sp.Prog, sc); err != nil {
				return nil, err
			}
		}
		return expr.EvalValue(t, sc)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			var err error
			if out[i], err = evalMatrix(e, sc); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			var err error
			if out[k], err = evalMatrix(e, sc); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

func cloneValues(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// describe names a value's kind for an error.
func describe(v any) string {
	switch v.(type) {
	case nil:
		return "nothing"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprintf("%T", v)
}

// legSuffix is what GitHub appends to a matrix leg's name: its values, in
// key order, in parentheses.
func legSuffix(leg matrixLeg) string {
	parts := make([]string, 0, len(leg.order))
	for _, k := range leg.order {
		parts = append(parts, legValueString(leg.values[k]))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func legValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// nameReadsMatrix reports whether a job's name: is written in terms of the
// matrix, in which case GitHub uses it as it evaluates, without the suffix.
func nameReadsMatrix(name string) bool {
	spans, err := expr.Spans(name)
	if err != nil {
		return false
	}
	for _, sp := range spans {
		if contexts, _ := sp.Prog.References(); slices.Contains(contexts, "matrix") {
			return true
		}
	}
	return false
}

// legScope is sc with a leg's matrix and strategy contexts. Without a leg —
// a job with no matrix — they are what GitHub gives one: an empty matrix and
// a strategy of one job.
func legScope(sc expr.Scope, st *matrixStrategy, index int) expr.Scope {
	matrix := map[string]any{}
	strategy := map[string]any{"fail-fast": true, "job-index": float64(0), "job-total": float64(1), "max-parallel": float64(1)}
	if st != nil {
		matrix = cloneValues(st.legs[index].values)
		parallel := len(st.legs)
		if st.maxParallel > 0 && st.maxParallel < parallel {
			parallel = st.maxParallel
		}
		strategy = map[string]any{
			"fail-fast": st.failFast, "job-index": float64(index), "job-total": float64(len(st.legs)),
			"max-parallel": float64(parallel),
		}
	}
	return withContext(withContext(sc, "matrix", matrix), "strategy", strategy)
}

// legMatrix is a leg's values as the plan reports them and run.matrix
// returns them: a JSON object, keys sorted.
func legMatrix(leg matrixLeg) (json.RawMessage, error) {
	return json.Marshal(leg.values)
}

// findLeg is the index of the leg whose values are exactly given's, or -1.
func findLeg(st *matrixStrategy, given map[string]any) int {
	for i, leg := range st.legs {
		if reflect.DeepEqual(leg.values, given) {
			return i
		}
	}
	return -1
}

// withLeg is sc with the matrix leg a run builds (A55).
//
// The leg is not taken on trust. The job's matrix is expanded here, from the
// pinned commit's file, and the run's values must be exactly one of its legs:
// a control plane cannot invent a leg the file does not declare, nor use one
// to reach a runner the file does not name. The index the leg has in that
// expansion is strategy.job-index.
func withLeg(w *Workflow, j *Job, key string, sc expr.Scope, given json.RawMessage) (expr.Scope, error) {
	st, err := expandStrategy(j, sc)
	if err != nil {
		return sc, badRegistry("%s: job %q: %v", w.Path, key, err)
	}
	switch {
	case st == nil && len(given) == 0:
		return legScope(sc, nil, 0), nil
	case st == nil:
		return sc, badRegistry("%s: job %q has no matrix, and the run names a leg of one (A55)", w.Path, key)
	case len(given) == 0:
		return sc, badRegistry("%s: job %q is a matrix of %d jobs, and a run builds one of them; this one "+
			"names none (A55)", w.Path, key, len(st.legs))
	}
	var leg map[string]any
	if err := json.Unmarshal(given, &leg); err != nil || leg == nil {
		return sc, badRegistry("%s: job %q: the run's matrix is not a JSON object", w.Path, key)
	}
	i := findLeg(st, leg)
	if i < 0 {
		return sc, badRegistry("%s: job %q: the run's matrix %s is not one of the %d legs the file declares at "+
			"this commit (A55)", w.Path, key, given, len(st.legs))
	}
	return legScope(sc, st, i), nil
}

// MatrixLegs is a job's matrix legs, each as run.matrix carries it, for a
// caller that compiles every leg a plan would — `latem workflows check`, or a
// test (A55). Nil for a job without a matrix. sc is the plan's scope: github,
// inputs and vars.
func MatrixLegs(w *Workflow, job string, sc expr.Scope) ([]json.RawMessage, error) {
	j, ok := w.Jobs[job]
	if !ok {
		return nil, fmt.Errorf("%s declares no job %q", w.Path, job)
	}
	st, err := expandStrategy(j, sc)
	if err != nil || st == nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(st.legs))
	for _, leg := range st.legs {
		m, err := legMatrix(leg)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
