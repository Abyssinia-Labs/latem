package expr

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Program {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	return p
}

func with(t *testing.T, m map[string]string) func(string) (*Program, error) {
	return func(name string) (*Program, error) {
		src, ok := m[name]
		if !ok {
			return nil, fmt.Errorf("input %q is not declared", name)
		}
		return mustParse(t, src), nil
	}
}

func TestSubstituteReplacesTheTreeNotTheValue(t *testing.T) {
	p := mustParse(t, "format('{0}-{1}', inputs.name, inputs.n)")
	got, err := p.Substitute("inputs", with(t, map[string]string{"name": "'app'", "n": "github.run_number"}))
	if err != nil {
		t.Fatal(err)
	}
	v, err := got.Eval(scope())
	if err != nil || v != "app-42" {
		t.Errorf("= %#v, %v", v, err)
	}
	if !strings.Contains(got.Source(), "github.run_number") {
		t.Errorf("the derived source should show what it became: %q", got.Source())
	}
}

// The reason it is a tree rewrite: a caller's late expression makes the
// action's own reference late, with no mechanism of its own.
func TestSubstituteCarriesLatenessThrough(t *testing.T) {
	p := mustParse(t, "inputs.sha")
	got, err := p.Substitute("inputs", with(t, map[string]string{"sha": "steps.prev.outputs.sha"}))
	if err != nil {
		t.Fatal(err)
	}
	contexts, _ := got.References()
	if !slices.Contains(contexts, "steps") {
		t.Errorf("contexts = %v, want steps to be visible to the classifier", contexts)
	}
	if refs := got.StepReferences(); !slices.Equal(refs, []string{"prev"}) {
		t.Errorf("step refs = %v", refs)
	}
}

func TestSubstituteRefusesWhatItCannotResolve(t *testing.T) {
	for _, src := range []string{
		"inputs.undeclared",  // lookup refuses
		"inputs[github.ref]", // computed name
		"toJSON(inputs)",     // the bare context
	} {
		p := mustParse(t, src)
		if _, err := p.Substitute("inputs", with(t, map[string]string{"x": "'1'"})); err == nil {
			t.Errorf("%q should be refused, not left to read as null", src)
		}
	}
}

func TestRenameStepsMovesIdsIntoANamespace(t *testing.T) {
	p := mustParse(t, "steps.build.outputs.sha == steps['test'].outputs.sha && github.ref")
	got := p.RenameSteps(func(id string) string { return "act/" + id })
	if refs := got.StepReferences(); !slices.Equal(refs, []string{"act/build", "act/test"}) {
		t.Errorf("refs = %v", refs)
	}
	// An id with a slash is not a plain name, so it renders bracketed — and
	// the rendering has to parse back to the same thing.
	back := mustParse(t, got.Source())
	if refs := back.StepReferences(); !slices.Equal(refs, []string{"act/build", "act/test"}) {
		t.Errorf("rendered source %q did not round-trip: %v", got.Source(), refs)
	}
}

func TestAnd(t *testing.T) {
	yes, no := mustParse(t, "github.event_name == 'push'"), mustParse(t, "github.event_name == 'schedule'")
	for _, c := range []struct {
		p    *Program
		want bool
	}{{And(yes, yes), true}, {And(yes, no), false}, {And(nil, yes), true}, {And(no, nil), false}} {
		v, err := c.p.Eval(scope())
		if err != nil || truthy(v) != c.want {
			t.Errorf("%s = %#v, %v; want %v", c.p.Source(), v, err, c.want)
		}
	}
	// failure() || x, And-ed, must keep its grouping.
	late := And(mustParse(t, "failure() || always()"), yes)
	if !strings.Contains(late.Source(), "(failure() || always())") {
		t.Errorf("an || operand lost its brackets: %q", late.Source())
	}
}

// Whatever render prints has to mean what the tree means.
func TestRenderRoundTrips(t *testing.T) {
	for _, src := range []string{
		"!contains(github.ref, 'it''s') && (1 < 2 || null == false)",
		"fromJSON('[1,2]')[1]",
		"needs.*.outputs",
		"format('{0}', env.FOO) != ''",
	} {
		p := mustParse(t, src)
		back := mustParse(t, derived(p.root).Source())
		a, errA := p.Eval(scope())
		b, errB := back.Eval(scope())
		if errA != nil || errB != nil || toJSONString(a) != toJSONString(b) {
			t.Errorf("%q → %q: %#v (%v) vs %#v (%v)", src, back.Source(), a, errA, b, errB)
		}
	}
	if got := Literal("it's").Source(); got != "'it''s'" {
		t.Errorf("Literal source = %q", got)
	}
}
