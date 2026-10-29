package expr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scope() Scope {
	return Scope{
		Contexts: map[string]Value{
			"github": map[string]any{
				"ref":        "refs/heads/main",
				"event_name": "push",
				"run_number": float64(42),
				"event": map[string]any{
					"pull_request": map[string]any{"number": float64(7)},
				},
			},
			"env":    map[string]any{"FOO": "bar", "EMPTY": ""},
			"matrix": map[string]any{"os": "macos", "go": float64(1.25)},
			"needs": map[string]any{
				"build": map[string]any{"outputs": map[string]any{"sha": "abc123"}},
			},
		},
		Status: StatusSuccess,
	}
}

func evalOK(t *testing.T, src string) Value {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	v, err := p.Eval(scope())
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return v
}

func TestLiteralsAndContexts(t *testing.T) {
	cases := []struct {
		src  string
		want Value
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"42", float64(42)},
		{"1.5", 1.5},
		{"0xff", float64(255)},
		{"1e3", float64(1000)},
		{"'hello'", "hello"},
		{"'it''s'", "it's"},
		{"github.ref", "refs/heads/main"},
		{"github.event.pull_request.number", float64(7)},
		{"github['ref']", "refs/heads/main"},
		{"needs.build.outputs.sha", "abc123"},
		// Missing is null, not an error: a workflow reading an event field
		// that this event does not carry is correct and common.
		{"github.event.issue.number", nil},
		{"nosuchcontext", nil},
		{"nosuchcontext.deeply.nested", nil},
	}
	for _, c := range cases {
		if got := evalOK(t, c.src); got != c.want {
			t.Errorf("%s = %#v, want %#v", c.src, got, c.want)
		}
	}
}

// The coercion rules are the part most likely to be "simplified" into
// something that disagrees with GitHub on inputs nobody tested.
func TestComparisonCoercion(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"1 == 1", true},
		{"'1' == 1", true},        // string coerces to number
		{"'abc' == 0", false},     // NaN never equals anything
		{"'' == 0", true},         // the empty string is zero, not NaN
		{"null == 0", true},       //
		{"false == 0", true},      //
		{"true == 1", true},       //
		{"'ABC' == 'abc'", true},  // strings compare case-insensitively
		{"'abc' != 'ABC'", false}, //
		{"1 < 2", true},
		{"'a' < 'b'", false},  // both are NaN, so every ordering is false
		{"'a' >= 'b'", false}, // including this one
		{"2 >= 2", true},
		{"!true", false},
		{"!''", true},
		{"!'x'", false},
		{"!0", true},
	}
	for _, c := range cases {
		got, ok := evalOK(t, c.src).(bool)
		if !ok || got != c.want {
			t.Errorf("%s = %#v, want %v", c.src, evalOK(t, c.src), c.want)
		}
	}
}

// && and || yield the operand, not a boolean. `${{ env.X || 'default' }}` is
// the whole reason this matters.
func TestLogicalOperatorsYieldOperands(t *testing.T) {
	if got := evalOK(t, "env.EMPTY || 'fallback'"); got != "fallback" {
		t.Errorf("|| = %#v, want the fallback operand", got)
	}
	if got := evalOK(t, "env.FOO || 'fallback'"); got != "bar" {
		t.Errorf("|| = %#v, want the truthy left operand", got)
	}
	if got := evalOK(t, "env.FOO && 'second'"); got != "second" {
		t.Errorf("&& = %#v, want the right operand", got)
	}
	if got := evalOK(t, "env.EMPTY && 'second'"); got != "" {
		t.Errorf("&& = %#v, want the falsy left operand", got)
	}
}

func TestPrecedence(t *testing.T) {
	// && binds tighter than ||; == tighter than both; ! tighter than all.
	if got := evalOK(t, "true || false && false"); got != true {
		t.Errorf("|| vs && = %#v", got)
	}
	if got := evalOK(t, "1 == 1 && 2 == 2"); got != true {
		t.Errorf("== vs && = %#v", got)
	}
	if got := evalOK(t, "!false && true"); got != true {
		t.Errorf("! vs && = %#v", got)
	}
	if got := evalOK(t, "(true || false) && false"); got != false {
		t.Errorf("parens = %#v", got)
	}
}

func TestFunctions(t *testing.T) {
	cases := []struct {
		src  string
		want Value
	}{
		{"contains('hello world', 'World')", true},
		{"contains('hello', 'xyz')", false},
		{"startsWith('refs/heads/main', 'refs/')", true},
		{"endsWith('a.tar.gz', '.GZ')", true},
		{"format('{0}-{1}', 'a', 2)", "a-2"},
		{"format('{{literal}}')", "{literal}"},
		{"join(fromJSON('[1,2,3]'), '-')", "1-2-3"},
		{"join(fromJSON('[\"a\",\"b\"]'))", "a,b"},
		{"toJSON('x')", `"x"`},
		{"fromJSON('{\"a\":1}').a", float64(1)},
		{"fromJSON('[10,20]')[1]", float64(20)},
		{"success()", true},
		{"failure()", false},
		{"always()", true},
		{"cancelled()", false},
		// A float holding an integer must not print as 1.000000; this lands in
		// tag names and artifact paths.
		{"format('v{0}', github.run_number)", "v42"},
	}
	for _, c := range cases {
		if got := evalOK(t, c.src); got != c.want {
			t.Errorf("%s = %#v, want %#v", c.src, got, c.want)
		}
	}
}

func TestContainsOnArrays(t *testing.T) {
	if got := evalOK(t, "contains(fromJSON('[\"a\",\"b\"]'), 'B')"); got != true {
		t.Errorf("array membership = %#v, want true", got)
	}
	if got := evalOK(t, "contains(fromJSON('[1,2]'), 3)"); got != false {
		t.Errorf("array membership = %#v, want false", got)
	}
}

func TestStarFilter(t *testing.T) {
	v := evalOK(t, "fromJSON('[{\"n\":1},{\"n\":2}]').*.n")
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 || arr[0] != float64(1) || arr[1] != float64(2) {
		t.Errorf("star filter = %#v", v)
	}
}

func TestStatusFunctionsFollowScope(t *testing.T) {
	p, err := Parse("failure()")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Eval(Scope{Status: StatusFailure})
	if err != nil || v != true {
		t.Errorf("failure() under a failed status = %#v, %v", v, err)
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, src := range []string{
		"1 +",
		"(1",
		"'unterminated",
		`"double quoted"`,
		"github.",
		"nosuchfn(1)",
		"1 @ 2",
	} {
		p, err := Parse(src)
		if err != nil {
			continue // a parse error is a fine outcome
		}
		if _, err := p.Eval(scope()); err == nil {
			t.Errorf("%q parsed and evaluated without error", src)
		}
	}
}

func TestTemplateSpans(t *testing.T) {
	spans, err := Spans("a ${{ github.ref }} b ${{ 1 }} c")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if spans[0].Start != 2 || spans[0].End != 19 {
		t.Errorf("first span = %d..%d", spans[0].Start, spans[0].End)
	}
}

// A `}}` inside a string literal must not terminate the expression.
func TestBracesInsideStringLiteral(t *testing.T) {
	got, err := EvalString("${{ contains('a}}b', '}}') }}", scope())
	if err != nil {
		t.Fatalf("EvalString: %v", err)
	}
	if got != "true" {
		t.Errorf("= %q, want \"true\"", got)
	}
}

func TestEvalStringAndValue(t *testing.T) {
	got, err := EvalString("ref is ${{ github.ref }}", scope())
	if err != nil || got != "ref is refs/heads/main" {
		t.Errorf("EvalString = %q, %v", got, err)
	}
	// A lone expression keeps its type; surrounded by text it becomes a string.
	v, err := EvalValue("${{ fromJSON('[1,2]') }}", scope())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.([]any); !ok {
		t.Errorf("EvalValue kept %T, want []any", v)
	}
	v2, err := EvalValue("x${{ fromJSON('[1,2]') }}", scope())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v2.(string); !ok {
		t.Errorf("EvalValue with surrounding text kept %T, want string", v2)
	}
}

// `if:` is written both with and without the braces.
func TestTruthyAcceptsBareConditions(t *testing.T) {
	for _, src := range []string{
		"github.event_name == 'push'",
		"${{ github.event_name == 'push' }}",
	} {
		ok, err := Truthy(src, scope())
		if err != nil || !ok {
			t.Errorf("Truthy(%q) = %v, %v", src, ok, err)
		}
	}
	if ok, err := Truthy("", scope()); err != nil || !ok {
		t.Errorf("an empty condition should be true: %v, %v", ok, err)
	}
}

func TestHashFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := scope()
	sc.WorkspaceDir = dir

	p, _ := Parse("hashFiles('go.sum')")
	v, err := p.Eval(sc)
	if err != nil {
		t.Fatalf("hashFiles: %v", err)
	}
	if s, ok := v.(string); !ok || len(s) != 64 {
		t.Errorf("hashFiles = %#v, want a 64-char hex digest", v)
	}

	// No match is the empty string, which is what callers compare against.
	p2, _ := Parse("hashFiles('nothing-here.lock')")
	v2, err := p2.Eval(sc)
	if err != nil || v2 != "" {
		t.Errorf("unmatched hashFiles = %#v, %v", v2, err)
	}

	// Without a workspace it refuses rather than hashing the agent's cwd.
	p3, _ := Parse("hashFiles('go.sum')")
	if _, err := p3.Eval(scope()); err == nil {
		t.Error("hashFiles with no workspace should error, not hash the process's cwd")
	}
}

// A pattern is repository content, and repository content does not get to read
// outside the workspace. Both routes out are closed: traversal in the pattern,
// and a symlink pointing out of the tree.
func TestHashFilesStaysInTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sc := scope()
	sc.WorkspaceDir = dir

	p, _ := Parse("hashFiles('../*/secret')")
	if _, err := p.Eval(sc); err == nil {
		t.Error("a pattern escaping the workspace should be refused")
	}

	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p2, _ := Parse("hashFiles('link')")
	v, err := p2.Eval(sc)
	if err != nil {
		t.Fatalf("hashFiles over a symlink: %v", err)
	}
	if v != "" {
		t.Errorf("a symlink out of the workspace was followed: %#v", v)
	}
}

func TestContextKeys(t *testing.T) {
	p, err := Parse(`secrets.A != '' && secrets['B'] && format('{0}', secrets.a) && steps.x.outputs.y`)
	if err != nil {
		t.Fatal(err)
	}
	keys, computed := p.ContextKeys("secrets")
	if strings.Join(keys, ",") != "A,B,a" || computed {
		t.Errorf("ContextKeys(secrets) = %v, %v", keys, computed)
	}
	if ids := p.StepReferences(); strings.Join(ids, ",") != "x" {
		t.Errorf("StepReferences = %v", ids)
	}
	p2, err := Parse(`secrets[format('KEY_{0}', env.STAGE)]`)
	if err != nil {
		t.Fatal(err)
	}
	if keys, computed := p2.ContextKeys("secrets"); len(keys) != 0 || !computed {
		t.Errorf("a computed key should be reported as computed: %v, %v", keys, computed)
	}
}
