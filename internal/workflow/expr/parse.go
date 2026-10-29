package expr

import (
	"fmt"
	"sort"
)

// node is one AST node. The tree is small — six shapes — so a single struct
// with a kind discriminator stays easier to read than six types and a visitor.
type node struct {
	kind  nodeKind
	op    string
	lit   Value
	name  string
	left  *node
	right *node
	args  []*node
}

type nodeKind int

const (
	nLiteral nodeKind = iota
	nContext          // a bare identifier: github, env, matrix, …
	nIndex            // left[right] and left.name, which are the same operation
	nStar             // left.* — the array filter
	nCall
	nUnary
	nBinary
)

// Parse compiles expression source into a reusable program.
//
// Parsing is separated from evaluation because a workflow's expressions are
// compiled once at load and evaluated once per step, and because a syntax error
// should be a registry error at admission rather than a failure eight minutes
// into a build.
func Parse(src string) (*Program, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("unexpected %q at offset %d", p.peek().text, p.peek().pos)
	}
	return &Program{root: n, src: src}, nil
}

// Program is a parsed expression.
type Program struct {
	root *node
	src  string
}

// Source returns the text the program was compiled from, for error messages
// that quote the expression rather than describing it.
func (p *Program) Source() string { return p.src }

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token         { return p.toks[p.i] }
func (p *parser) next() token         { t := p.toks[p.i]; p.i++; return t }
func (p *parser) at(k tokenKind) bool { return p.toks[p.i].kind == k }

// precedence of the binary operators, loosest first. Unary ! binds tighter
// than all of them and is handled in parseUnary.
var precedence = map[string]int{
	"||": 1,
	"&&": 2,
	"==": 3, "!=": 3,
	"<": 4, "<=": 4, ">": 4, ">=": 4,
}

func (p *parser) parseExpr(minPrec int) (*node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokOp {
			return left, nil
		}
		prec, ok := precedence[t.text]
		if !ok || prec < minPrec {
			return left, nil
		}
		p.next()
		// Every binary operator here is left-associative, so the right side is
		// parsed at one level tighter.
		right, err := p.parseExpr(prec + 1)
		if err != nil {
			return nil, err
		}
		left = &node{kind: nBinary, op: t.text, left: left, right: right}
	}
}

func (p *parser) parseUnary() (*node, error) {
	if t := p.peek(); t.kind == tokOp && t.text == "!" {
		p.next()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &node{kind: nUnary, op: "!", left: operand}, nil
	}
	return p.parsePostfix()
}

// parsePostfix reads a primary and then every access that follows it, so
// `github.event.pull_request.number` and `needs.build.outputs['x']` and
// `fromJSON(x)[0].name` all parse without special cases.
func (p *parser) parsePostfix() (*node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.at(tokDot):
			p.next()
			if p.at(tokStar) {
				p.next()
				n = &node{kind: nStar, left: n}
				continue
			}
			if !p.at(tokIdent) {
				return nil, fmt.Errorf("expected a property name after '.' at offset %d", p.peek().pos)
			}
			name := p.next()
			n = &node{kind: nIndex, left: n, right: &node{kind: nLiteral, lit: name.text}}

		case p.at(tokLBracket):
			p.next()
			if p.at(tokStar) {
				p.next()
				if !p.at(tokRBracket) {
					return nil, fmt.Errorf("expected ']' after '*' at offset %d", p.peek().pos)
				}
				p.next()
				n = &node{kind: nStar, left: n}
				continue
			}
			idx, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			if !p.at(tokRBracket) {
				return nil, fmt.Errorf("expected ']' at offset %d", p.peek().pos)
			}
			p.next()
			n = &node{kind: nIndex, left: n, right: idx}

		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (*node, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		return &node{kind: nLiteral, lit: t.num}, nil

	case tokString:
		return &node{kind: nLiteral, lit: t.text}, nil

	case tokLParen:
		n, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if !p.at(tokRParen) {
			return nil, fmt.Errorf("expected ')' at offset %d", p.peek().pos)
		}
		p.next()
		return n, nil

	case tokIdent:
		switch lowerASCII(t.text) {
		case "true":
			return &node{kind: nLiteral, lit: true}, nil
		case "false":
			return &node{kind: nLiteral, lit: false}, nil
		case "null":
			return &node{kind: nLiteral, lit: nil}, nil
		}
		if p.at(tokLParen) {
			p.next()
			var args []*node
			if !p.at(tokRParen) {
				for {
					a, err := p.parseExpr(0)
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.at(tokComma) {
						p.next()
						continue
					}
					break
				}
			}
			if !p.at(tokRParen) {
				return nil, fmt.Errorf("expected ')' closing %s( at offset %d", t.text, t.pos)
			}
			p.next()
			return &node{kind: nCall, name: t.text, args: args}, nil
		}
		return &node{kind: nContext, name: t.text}, nil

	case tokEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	}
	return nil, fmt.Errorf("unexpected %q at offset %d", t.text, t.pos)
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// References reports the context names and function names an expression
// reaches for, without evaluating it.
//
// The caller is the workflow compiler, which has to tell an expression it can
// resolve now from one that cannot be resolved until the step runs.
// `${{ github.ref }}` is known when a run is admitted; `${{ steps.x.outputs.y }}`
// and `${{ failure() }}` are not, and compiling them to a value would bake in
// an answer that happens to be whatever was true at admission.
func (p *Program) References() (contexts, funcs []string) {
	seenCtx, seenFn := map[string]bool{}, map[string]bool{}
	var walk func(*node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		switch n.kind {
		case nContext:
			seenCtx[lowerASCII(n.name)] = true
		case nCall:
			seenFn[lowerASCII(n.name)] = true
		}
		walk(n.left)
		walk(n.right)
		for _, a := range n.args {
			walk(a)
		}
	}
	walk(p.root)
	return sortedSet(seenCtx), sortedSet(seenFn)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// StepReferences lists the step ids an expression reads as `steps.<id>` or
// `steps['<id>']` with a literal id.
//
// Only a literal counts. `steps[someExpr]` cannot be checked at admission and
// is not reported, so it resolves to null at run time — which is what GitHub
// does with it as well. The registry uses this to refuse a reference to a step
// that is not an earlier one, before anything runs.
func (p *Program) StepReferences() []string {
	ids, _ := p.ContextKeys("steps")
	return ids
}

// ContextKeys lists the literal keys an expression reads directly under one
// context — `secrets.NAME` and `secrets['NAME']` for "secrets" — and reports
// whether it also indexes that context with a key computed at run time, which
// no list of names can describe.
func (p *Program) ContextKeys(context string) (keys []string, computed bool) {
	context = lowerASCII(context)
	seen := map[string]bool{}
	var walk func(*node)
	walk = func(n *node) {
		if n == nil {
			return
		}
		if n.kind == nIndex && n.left != nil && n.left.kind == nContext && lowerASCII(n.left.name) == context {
			if n.right != nil && n.right.kind == nLiteral {
				if k, ok := n.right.lit.(string); ok && k != "" {
					seen[k] = true
				}
			} else {
				computed = true
			}
		}
		walk(n.left)
		walk(n.right)
		for _, a := range n.args {
			walk(a)
		}
	}
	walk(p.root)
	return sortedSet(seen), computed
}
