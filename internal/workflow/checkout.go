package workflow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// checkoutAction is the one action every workflow opens with.
const checkoutAction = "actions/checkout"

// checkoutSpec is a checkout's `with:`, kept as written until compileStep,
// which knows the step's scope and turns it into a [registry.CheckoutStep].
type checkoutSpec struct {
	ref, token                string
	persist, clean, fetchTags bool
	depth                     int
}

// checkoutInputs are the `with:` names latem's checkout answers (A53). Each
// returns why a value cannot be honoured, or "" when it can. Everything
// absent is refused by name rather than ignored, which is A48's point and
// still the point: a build that silently got something other than what it
// asked for fails later, somewhere nobody traces back to here.
var checkoutInputs = map[string]func(v string, sc expr.Scope) string{
	"ref":                 func(string, expr.Scope) string { return "" },
	"token":               func(string, expr.Scope) string { return "" },
	"persist-credentials": boolInput,
	"clean":               boolInput,
	"fetch-tags":          boolInput,
	"fetch-depth": func(v string, sc expr.Scope) string {
		s, why := staticInput(v, sc)
		if why != "" {
			return why
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n < 0 {
			return "is not a whole number of commits"
		}
		return ""
	},
	// §12 already initialises the run's own submodules and LFS objects, and a
	// checkout elsewhere in the job would need its own; neither is built.
	"submodules": falseInput,
	"lfs":        falseInput,
	// The workspace root is the only place a repository is checked out.
	"path": func(v string, sc expr.Scope) string {
		s, why := staticInput(v, sc)
		if why != "" {
			return why
		}
		if s := strings.TrimSpace(s); s != "" && s != "." {
			return "checks out somewhere other than the workspace root"
		}
		return ""
	},
	// The run's own repository, written out, is still the run's own.
	"repository": func(v string, sc expr.Scope) string {
		s, why := staticInput(v, sc)
		if why != "" {
			return why
		}
		gh, _ := sc.Contexts["github"].(map[string]any)
		own, _ := gh["repository"].(string)
		if s := strings.TrimSpace(s); s != "" && !strings.EqualFold(s, own) {
			return "is another repository; a run builds one repository"
		}
		return ""
	},
	"github-server-url": func(v string, sc expr.Scope) string {
		s, why := staticInput(v, sc)
		if why != "" {
			return why
		}
		if s := strings.TrimRight(strings.TrimSpace(s), "/"); s != "" && s != "https://github.com" {
			return "is not github.com"
		}
		return ""
	},
	"show-progress":      func(string, expr.Scope) string { return "" },
	"set-safe-directory": func(string, expr.Scope) string { return "" },
}

// staticInput evaluates a `with:` value that has to be known when the job is
// compiled.
func staticInput(v string, sc expr.Scope) (string, string) {
	s, err := expr.EvalString(v, sc)
	if err != nil {
		return "", "is only known once the job runs, and this input is needed before"
	}
	return s, ""
}

func boolInput(v string, sc expr.Scope) string {
	s, why := staticInput(v, sc)
	if why != "" {
		return why
	}
	if s := strings.TrimSpace(s); s != "" && !strings.EqualFold(s, "true") && !strings.EqualFold(s, "false") {
		return "is not true or false"
	}
	return ""
}

func falseInput(v string, sc expr.Scope) string {
	s, why := staticInput(v, sc)
	if why != "" {
		return why
	}
	if s := strings.TrimSpace(s); s != "" && !strings.EqualFold(s, "false") {
		return "is not built"
	}
	return ""
}

func truthy(v string) bool { return v == "" || strings.EqualFold(v, "true") }
func falsey(v string) bool { return v == "" || strings.EqualFold(v, "false") }

// checkoutShim recognises `uses: actions/checkout` and turns it into a step
// the run loop performs (A53), refusing by name what it cannot honour.
//
// It runs before the allowlist, and deliberately: nothing is fetched and
// nothing third-party executes, so there is no decision for an operator to
// make. The step moves only the working tree, to a commit the repository's
// policy already admits; the job itself was compiled from the pinned commit
// (INV-5) and stays that.
func checkoutShim(st Step, sc expr.Scope) (*checkoutSpec, bool, error) {
	ref, parseErr := actions.ParseRef(st.Uses)
	if parseErr != nil || !strings.EqualFold(ref.Slug(), checkoutAction) || ref.Path != "" {
		return nil, false, nil
	}

	with, err := withStrings(st.With)
	if err != nil {
		return nil, false, err
	}
	var refused []string
	for _, name := range sortedNames(with) {
		check, known := checkoutInputs[strings.ToLower(name)]
		if !known {
			refused = append(refused, fmt.Sprintf("%s (not an input latem's checkout answers)", name))
			continue
		}
		if why := check(with[name], sc); why != "" {
			refused = append(refused, fmt.Sprintf("%s: %s %s", name, with[name], why))
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, false, fmt.Errorf("uses: %s — latem performs this checkout itself (A53) and cannot honour %s",
			st.Uses, strings.Join(refused, "; "))
	}

	spec := &checkoutSpec{persist: true, clean: true, depth: 1}
	for name, v := range with {
		switch strings.ToLower(name) {
		case "ref":
			spec.ref = v
		case "token":
			spec.token = v
		case "persist-credentials":
			s, _ := staticInput(v, sc)
			spec.persist = truthy(strings.TrimSpace(s))
		case "clean":
			s, _ := staticInput(v, sc)
			spec.clean = truthy(strings.TrimSpace(s))
		case "fetch-tags":
			s, _ := staticInput(v, sc)
			spec.fetchTags = !falsey(strings.TrimSpace(s))
		case "fetch-depth":
			s, _ := staticInput(v, sc)
			spec.depth, _ = strconv.Atoi(strings.TrimSpace(s))
		}
	}
	return spec, true, nil
}

// compileCheckout turns a checkout's `with:` into what the run loop performs.
//
// `ref:` may be known now (`inputs`, `needs`, `github`) or only as the step
// runs (`steps.*`). `token:` must be an expression: a credential written
// into a workflow file is a credential published with the repository, and
// is refused rather than used.
func compileCheckout(spec *checkoutSpec, sc expr.Scope, where string) (*registry.CheckoutStep, error) {
	out := &registry.CheckoutStep{
		PersistCredentials: spec.persist,
		Clean:              spec.clean,
		FetchDepth:         spec.depth,
		FetchTags:          spec.fetchTags,
	}
	if strings.TrimSpace(spec.ref) != "" {
		late, err := withValue("ref", spec.ref, sc, where)
		if err != nil {
			return nil, err
		}
		if late != nil {
			out.RefLate = late
		} else {
			v, err := expr.EvalString(spec.ref, sc)
			if err != nil {
				return nil, badRegistry("%s: with.ref: %v", where, err)
			}
			out.Ref = strings.TrimSpace(v)
		}
	}
	if strings.TrimSpace(spec.token) != "" {
		if !expr.Has(spec.token) {
			return nil, badRegistry("%s: with.token is written into the workflow; a token belongs in a secret — "+
				"`token: ${{ secrets.NAME }}`, or leave it out for the job's own", where)
		}
		late, err := withValue("token", spec.token, sc, where)
		if err != nil {
			return nil, err
		}
		if late == nil {
			return nil, badRegistry("%s: with.token evaluates to a value written into the workflow; a token "+
				"belongs in a secret", where)
		}
		out.Token = late
	}
	return out, nil
}

// withValue is envValue for a checkout's inputs: nil for a value known now, a
// late value for one known as the step runs.
func withValue(name, v string, sc expr.Scope, where string) (registry.LateValue, error) {
	if !expr.Has(v) {
		return nil, nil
	}
	spans, err := expr.Spans(v)
	if err != nil {
		return nil, badRegistry("%s: with.%s: %v", where, name, err)
	}
	anyLate := false
	for _, sp := range spans {
		w, err := classify(sp.Prog, sc)
		if err != nil {
			return nil, badRegistry("%s: with.%s: %v", where, name, err)
		}
		if w == whenLate {
			anyLate = true
		}
	}
	if !anyLate {
		return nil, nil
	}
	if len(spans) != 1 || spans[0].Start != 0 || spans[0].End != len(v) {
		return newLateTemplate(v, spans, sc), nil
	}
	return newLate(spans[0].Prog, sc), nil
}
