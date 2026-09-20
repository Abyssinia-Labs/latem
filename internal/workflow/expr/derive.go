package expr

import (
	"fmt"
	"regexp"
	"strings"
)

// This file derives programs from programs. It exists for A44: a composite
// action is inlined into the job that uses it, and the two things that have to
// be true for that to be correct — the action's `inputs.x` means whatever the
// caller wrote in `with: x:`, and its step ids cannot collide with the job's —
// are both rewrites of an expression's tree rather than of its value.
//
// Rewriting the tree is what makes late-ness fall out instead of needing a
// mechanism. If the caller wrote `with: sha: ${{ steps.prev.outputs.sha }}`,
// then after substitution the action's `${{ inputs.sha }}` *is*
// `steps.prev.outputs.sha`, and the compiler's existing classifier sees a
// steps reference and makes it late. Substituting the value instead would
// need the value, which does not exist until the run is under way.

// Substitute replaces every `<context>.<name>` with the program lookup returns
// for that name.
//
// Only a literal name can be substituted, because the replacement is chosen at
// compile time. `inputs[someExpr]`, or the bare context handed to a function,
// is refused rather than left to evaluate to null — a composite action whose
// input silently reads as empty is the failure this is here to prevent.
func (p *Program) Substitute(context string, lookup func(name string) (*Program, error)) (*Program, error) {
	ctx := lowerASCII(context)
	var walk func(*node) (*node, error)
	walk = func(n *node) (*node, error) {
		if n == nil {
			return nil, nil
		}
		if n.kind == nIndex && n.left != nil && n.left.kind == nContext && lowerASCII(n.left.name) == ctx {
			name, ok := n.right.lit.(string)
			if n.right.kind != nLiteral || !ok {
				return nil, fmt.Errorf("%s[…] with a computed name cannot be used here; write %s.<name>", context, context)
			}
			repl, err := lookup(name)
			if err != nil {
				return nil, err
			}
			return repl.root, nil
		}
		if n.kind == nContext && lowerASCII(n.name) == ctx {
			return nil, fmt.Errorf("the %s context can only be read as %s.<name> here", context, context)
		}
		out := *n
		var err error
		if out.left, err = walk(n.left); err != nil {
			return nil, err
		}
		if out.right, err = walk(n.right); err != nil {
			return nil, err
		}
		if len(n.args) > 0 {
			out.args = make([]*node, len(n.args))
			for i, a := range n.args {
				if out.args[i], err = walk(a); err != nil {
					return nil, err
				}
			}
		}
		return &out, nil
	}
	root, err := walk(p.root)
	if err != nil {
		return nil, err
	}
	return derived(root), nil
}

// RenameSteps rewrites every literal `steps.<id>` through rename. It is how an
// inlined action's step ids are moved into the namespace of the step that
// used it, so that `build` inside an action and `build` in the job are two
// ids and the registry's ordering check sees both.
func (p *Program) RenameSteps(rename func(id string) string) *Program {
	var walk func(*node) *node
	walk = func(n *node) *node {
		if n == nil {
			return nil
		}
		out := *n
		if n.kind == nIndex && n.left != nil && n.left.kind == nContext &&
			lowerASCII(n.left.name) == "steps" && n.right != nil && n.right.kind == nLiteral {
			if id, ok := n.right.lit.(string); ok {
				out.right = &node{kind: nLiteral, lit: rename(id)}
				out.left = n.left
				return &out
			}
		}
		out.left, out.right = walk(n.left), walk(n.right)
		if len(n.args) > 0 {
			out.args = make([]*node, len(n.args))
			for i, a := range n.args {
				out.args[i] = walk(a)
			}
		}
		return &out
	}
	return derived(walk(p.root))
}

// And is `p && q` as one program: the condition on a `uses:` step applied to
// every step inlined from it.
func And(p, q *Program) *Program {
	switch {
	case p == nil:
		return q
	case q == nil:
		return p
	}
	return derived(&node{kind: nBinary, op: "&&", left: p.root, right: q.root})
}

// Literal is a program that evaluates to a fixed string — an input's default,
// or a `with:` value that contained no expression.
func Literal(s string) *Program { return derived(&node{kind: nLiteral, lit: s}) }

// derived wraps a tree, rendering its source so that messages quoting a
// derived expression show what it actually is after the rewrite.
func derived(root *node) *Program { return &Program{root: root, src: render(root)} }

var plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// render prints a tree back to expression source. Binary operands are
// parenthesised whenever they are themselves binary, which is more brackets
// than a person would write and never fewer than the tree needs.
func render(n *node) string {
	if n == nil {
		return ""
	}
	switch n.kind {
	case nLiteral:
		switch v := n.lit.(type) {
		case nil:
			return "null"
		case string:
			return "'" + strings.ReplaceAll(v, "'", "''") + "'"
		default:
			return toString(v)
		}
	case nContext:
		return n.name
	case nIndex:
		if n.right != nil && n.right.kind == nLiteral {
			if s, ok := n.right.lit.(string); ok && plainName.MatchString(s) {
				return render(n.left) + "." + s
			}
		}
		return render(n.left) + "[" + render(n.right) + "]"
	case nStar:
		return render(n.left) + ".*"
	case nCall:
		args := make([]string, len(n.args))
		for i, a := range n.args {
			args[i] = render(a)
		}
		return n.name + "(" + strings.Join(args, ", ") + ")"
	case nUnary:
		return n.op + renderOperand(n.left)
	case nBinary:
		return renderOperand(n.left) + " " + n.op + " " + renderOperand(n.right)
	}
	return ""
}

func renderOperand(n *node) string {
	if n != nil && n.kind == nBinary {
		return "(" + render(n) + ")"
	}
	return render(n)
}

// Template turns a template string — literal text, `${{ … }}` spans, or a mix —
// into one program that evaluates to what the string would render as.
//
// It is what lets a `with:` value be substituted for `inputs.x` whatever shape
// the caller wrote it in. No spans is a literal; exactly one span and nothing
// else is that span's program, so its type and its late-ness survive; a mix
// becomes a `format` call, with the text's own braces escaped so they are not
// read as placeholders.
func Template(s string) (*Program, error) {
	spans, err := Spans(s)
	if err != nil {
		return nil, err
	}
	switch {
	case len(spans) == 0:
		return Literal(s), nil
	case len(spans) == 1 && spans[0].Start == 0 && spans[0].End == len(s):
		return spans[0].Prog, nil
	}
	escape := strings.NewReplacer("{", "{{", "}", "}}")
	var f strings.Builder
	args := []*node{nil} // args[0] is the format string, filled in below
	last := 0
	for i, sp := range spans {
		f.WriteString(escape.Replace(s[last:sp.Start]))
		fmt.Fprintf(&f, "{%d}", i)
		args = append(args, sp.Prog.root)
		last = sp.End
	}
	f.WriteString(escape.Replace(s[last:]))
	args[0] = &node{kind: nLiteral, lit: f.String()}
	return derived(&node{kind: nCall, name: "format", args: args}), nil
}

// SubstituteField replaces exactly `<context>.<name>` and leaves every other
// use of the context alone. It is for the one field an inlined action adds to
// a context that otherwise belongs to the job: `github.action_path`.
func (p *Program) SubstituteField(context, name string, repl *Program) *Program {
	ctx := lowerASCII(context)
	return p.mapNodes(func(n *node) *node {
		if n.kind == nIndex && n.left != nil && n.left.kind == nContext && lowerASCII(n.left.name) == ctx &&
			n.right != nil && n.right.kind == nLiteral {
			if s, ok := n.right.lit.(string); ok && strings.EqualFold(s, name) {
				return repl.root
			}
		}
		return nil
	})
}

// SubstituteOutputs replaces `steps.<id>.outputs.<name>` with the program
// lookup returns for name. It is how a composite action's declared outputs
// reach the job that used it: the reference becomes the action's own
// `outputs.<name>.value` expression, which reads an inner step's real output.
// A name the action does not declare is an error, for the same reason an
// undeclared input is — the alternative is an empty string nobody asked for.
func (p *Program) SubstituteOutputs(id string, lookup func(name string) (*Program, error)) (*Program, error) {
	var failed error
	out := p.mapNodes(func(n *node) *node {
		name, ok := stepOutputRef(n, id)
		if !ok || failed != nil {
			return nil
		}
		repl, err := lookup(name)
		if err != nil {
			failed = err
			return nil
		}
		return repl.root
	})
	if failed != nil {
		return nil, failed
	}
	return out, nil
}

// stepOutputRef matches steps.<id>.outputs.<name> with literal id and name.
func stepOutputRef(n *node, id string) (string, bool) {
	lit := func(x *node) (string, bool) {
		if x == nil || x.kind != nLiteral {
			return "", false
		}
		s, ok := x.lit.(string)
		return s, ok
	}
	if n.kind != nIndex {
		return "", false
	}
	name, ok := lit(n.right)
	if !ok {
		return "", false
	}
	outputs := n.left
	if outputs == nil || outputs.kind != nIndex {
		return "", false
	}
	if s, ok := lit(outputs.right); !ok || !strings.EqualFold(s, "outputs") {
		return "", false
	}
	step := outputs.left
	if step == nil || step.kind != nIndex || step.left == nil || step.left.kind != nContext ||
		lowerASCII(step.left.name) != "steps" {
		return "", false
	}
	if s, ok := lit(step.right); !ok || s != id {
		return "", false
	}
	return name, true
}

// mapNodes rebuilds the tree top-down. Where f returns a node, that node
// replaces the subtree and is not descended into; nil means "keep walking".
func (p *Program) mapNodes(f func(*node) *node) *Program {
	var walk func(*node) *node
	walk = func(n *node) *node {
		if n == nil {
			return nil
		}
		if repl := f(n); repl != nil {
			return repl
		}
		out := *n
		out.left, out.right = walk(n.left), walk(n.right)
		if len(n.args) > 0 {
			out.args = make([]*node, len(n.args))
			for i, a := range n.args {
				out.args[i] = walk(a)
			}
		}
		return &out
	}
	return derived(walk(p.root))
}

// Wrap renders a program back into template form, `${{ … }}`, so that a
// rewritten expression can be put back where a template string is expected.
func (p *Program) Wrap() string { return "${{ " + p.Source() + " }}" }

// RewriteTemplate applies f to every expression in a template string and
// renders the result back as a template. Text between expressions is kept
// byte for byte.
func RewriteTemplate(s string, f func(*Program) (*Program, error)) (string, error) {
	spans, err := Spans(s)
	if err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return s, nil
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp.Start])
		prog, err := f(sp.Prog)
		if err != nil {
			return "", err
		}
		b.WriteString(prog.Wrap())
		last = sp.End
	}
	b.WriteString(s[last:])
	return b.String(), nil
}
