package expr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Status is the run status the status functions answer against.
type Status int

const (
	StatusSuccess Status = iota
	StatusFailure
	StatusCancelled
)

// Scope is everything an expression may read.
//
// Contexts is the named data — github, env, matrix and the rest. Missing is not
// an error: GitHub answers a nonexistent context property with null, and a
// workflow that reads `github.event.pull_request.number` on a push event relies
// on that. Erroring instead would reject correct workflows.
type Scope struct {
	Contexts map[string]Value
	Status   Status
	// WorkspaceDir roots hashFiles' globs. Empty disables hashFiles, which
	// answers with an error rather than a wrong hash — an empty string here
	// would otherwise hash the process's working directory, and on an agent
	// that is not the workspace.
	WorkspaceDir string
}

// Eval evaluates the program against a scope.
func (p *Program) Eval(sc Scope) (Value, error) {
	if p == nil || p.root == nil {
		return nil, fmt.Errorf("empty expression")
	}
	v, err := evalNode(p.root, sc)
	if err != nil {
		return nil, err
	}
	// filtered is an implementation detail of `.*`; the value handed back is
	// always one of the JSON types.
	if f, ok := v.(filtered); ok {
		return f.plain(), nil
	}
	return v, nil
}

func evalNode(n *node, sc Scope) (Value, error) {
	switch n.kind {
	case nLiteral:
		return n.lit, nil

	case nContext:
		// A bare identifier is a context name. An unknown one is null rather
		// than an error, matching the property rule above.
		if v, ok := sc.Contexts[n.name]; ok {
			return v, nil
		}
		if v, ok := sc.Contexts[lowerASCII(n.name)]; ok {
			return v, nil
		}
		return nil, nil

	case nIndex:
		recv, err := evalNode(n.left, sc)
		if err != nil {
			return nil, err
		}
		key, err := evalNode(n.right, sc)
		if err != nil {
			return nil, err
		}
		return index(recv, key), nil

	case nStar:
		recv, err := evalNode(n.left, sc)
		if err != nil {
			return nil, err
		}
		return starFilter(recv), nil

	case nUnary:
		v, err := evalNode(n.left, sc)
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil

	case nBinary:
		return evalBinary(n, sc)

	case nCall:
		return evalCall(n, sc)
	}
	return nil, fmt.Errorf("unevaluatable node")
}

func evalBinary(n *node, sc Scope) (Value, error) {
	// && and || short-circuit and yield the operand rather than a boolean,
	// which is what makes `${{ env.X || 'default' }}` the idiom it is. Folding
	// them to true/false would be a subtle and very common breakage.
	switch n.op {
	case "&&":
		left, err := evalNode(n.left, sc)
		if err != nil {
			return nil, err
		}
		if !truthy(left) {
			return left, nil
		}
		return evalNode(n.right, sc)
	case "||":
		left, err := evalNode(n.left, sc)
		if err != nil {
			return nil, err
		}
		if truthy(left) {
			return left, nil
		}
		return evalNode(n.right, sc)
	}

	left, err := evalNode(n.left, sc)
	if err != nil {
		return nil, err
	}
	right, err := evalNode(n.right, sc)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return equal(left, right), nil
	case "!=":
		return !equal(left, right), nil
	case "<", "<=", ">", ">=":
		return compare(n.op, left, right), nil
	}
	return nil, fmt.Errorf("unknown operator %q", n.op)
}

// filtered is the result of a `.*` filter.
//
// It is a distinct type rather than a plain []any because the next access has
// to behave differently: `needs.*.outputs.sha` collects `outputs.sha` from
// every element, where the same syntax against an ordinary array would be an
// out-of-range index and answer null. It never escapes the package — Eval
// converts it back to []any — so callers only ever see JSON types.
type filtered []any

func (f filtered) plain() []any { return []any(f) }

// index reads a property or element. Anything that is not there is null, and
// indexing a scalar is null rather than an error.
func index(recv, key Value) Value {
	switch r := recv.(type) {
	case filtered:
		// Map, dropping elements that do not have the property. That is
		// GitHub's behaviour and it is what makes `needs.*.outputs.x` useful
		// when only some jobs declare that output.
		out := make(filtered, 0, len(r))
		for _, e := range r {
			if v := index(e, key); v != nil {
				out = append(out, v)
			}
		}
		return out
	case map[string]any:
		name := toString(key)
		if v, ok := r[name]; ok {
			return v
		}
		// Context property lookup is case-insensitive in GitHub's
		// implementation. The exact-match above covers the ordinary path
		// without walking the map.
		for k, v := range r {
			if strings.EqualFold(k, name) {
				return v
			}
		}
		return nil
	case []any:
		f := toNumber(key)
		i := int(f)
		if float64(i) != f || i < 0 || i >= len(r) {
			return nil
		}
		return r[i]
	default:
		return nil
	}
}

// starFilter implements `x.*`, which collects one level: for an array, the
// elements themselves; for an object, its values in key order so the result is
// deterministic.
func starFilter(recv Value) Value {
	switch r := recv.(type) {
	case filtered:
		return r
	case []any:
		out := make(filtered, 0, len(r))
		out = append(out, r...)
		return out
	case map[string]any:
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(filtered, 0, len(r))
		for _, k := range keys {
			out = append(out, r[k])
		}
		return out
	default:
		return filtered{}
	}
}

func evalCall(n *node, sc Scope) (Value, error) {
	args := make([]Value, len(n.args))
	for i, a := range n.args {
		v, err := evalNode(a, sc)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	name := lowerASCII(n.name)

	arity := func(want int) error {
		if len(args) != want {
			return fmt.Errorf("%s() takes %d argument(s), got %d", n.name, want, len(args))
		}
		return nil
	}

	switch name {
	case "success":
		return sc.Status == StatusSuccess, nil
	case "failure":
		return sc.Status == StatusFailure, nil
	case "cancelled":
		return sc.Status == StatusCancelled, nil
	case "always":
		return true, nil

	case "contains":
		if err := arity(2); err != nil {
			return nil, err
		}
		return containsFn(args[0], args[1]), nil

	case "startswith":
		if err := arity(2); err != nil {
			return nil, err
		}
		return strings.HasPrefix(strings.ToLower(toString(args[0])), strings.ToLower(toString(args[1]))), nil

	case "endswith":
		if err := arity(2); err != nil {
			return nil, err
		}
		return strings.HasSuffix(strings.ToLower(toString(args[0])), strings.ToLower(toString(args[1]))), nil

	case "format":
		if len(args) == 0 {
			return nil, fmt.Errorf("format() needs a format string")
		}
		return formatFn(toString(args[0]), args[1:])

	case "join":
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("join() takes 1 or 2 arguments, got %d", len(args))
		}
		sep := ","
		if len(args) == 2 {
			sep = toString(args[1])
		}
		return joinFn(args[0], sep), nil

	case "tojson":
		if err := arity(1); err != nil {
			return nil, err
		}
		return toJSONString(args[0]), nil

	case "fromjson":
		if err := arity(1); err != nil {
			return nil, err
		}
		var out any
		if err := json.Unmarshal([]byte(toString(args[0])), &out); err != nil {
			return nil, fmt.Errorf("fromJSON: %w", err)
		}
		return out, nil

	case "hashfiles":
		return hashFilesFn(sc, args)
	}
	return nil, fmt.Errorf("unknown function %s()", n.name)
}

// containsFn is two functions wearing one name: substring search when the
// haystack is a string, membership when it is an array. Both are
// case-insensitive, as the string comparison rules are throughout.
func containsFn(haystack, needle Value) bool {
	if f, ok := haystack.(filtered); ok {
		haystack = f.plain()
	}
	if arr, ok := haystack.([]any); ok {
		for _, e := range arr {
			if equal(e, needle) {
				return true
			}
		}
		return false
	}
	return strings.Contains(
		strings.ToLower(toString(haystack)),
		strings.ToLower(toString(needle)),
	)
}

// formatFn expands {0}, {1}, … and treats {{ and }} as literal braces.
func formatFn(f string, args []Value) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		switch {
		case f[i] == '{' && i+1 < len(f) && f[i+1] == '{':
			b.WriteByte('{')
			i++
		case f[i] == '}' && i+1 < len(f) && f[i+1] == '}':
			b.WriteByte('}')
			i++
		case f[i] == '{':
			j := strings.IndexByte(f[i:], '}')
			if j < 0 {
				return "", fmt.Errorf("format: unclosed '{' at offset %d", i)
			}
			spec := f[i+1 : i+j]
			idx := 0
			if _, err := fmt.Sscanf(spec, "%d", &idx); err != nil {
				return "", fmt.Errorf("format: %q is not an argument index", spec)
			}
			if idx < 0 || idx >= len(args) {
				return "", fmt.Errorf("format: {%d} has no argument", idx)
			}
			b.WriteString(toString(args[idx]))
			i += j
		default:
			b.WriteByte(f[i])
		}
	}
	return b.String(), nil
}

func joinFn(v Value, sep string) string {
	if f, ok := v.(filtered); ok {
		v = f.plain()
	}
	arr, ok := v.([]any)
	if !ok {
		return toString(v)
	}
	parts := make([]string, len(arr))
	for i, e := range arr {
		parts[i] = toString(e)
	}
	return strings.Join(parts, sep)
}

// hashFilesFn hashes the matched files' contents.
//
// The hash is over each file's sha256 in sorted path order, then hashed again —
// the same shape GitHub uses, and the property that matters is that it is
// stable across machines and independent of directory iteration order. A
// pattern matching nothing returns the empty string, which is what callers
// compare against to detect "no lockfile here".
func hashFilesFn(sc Scope, args []Value) (Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("hashFiles() needs at least one pattern")
	}
	if sc.WorkspaceDir == "" {
		return nil, fmt.Errorf("hashFiles() has no workspace to resolve patterns against")
	}
	root, err := filepath.Abs(sc.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("hashFiles: %w", err)
	}

	var matches []string
	for _, a := range args {
		pattern := toString(a)
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, fmt.Errorf("hashFiles: %w", err)
		}
		for _, hit := range m {
			// A pattern is repository content: it arrives from jobs.yaml or a
			// workflow at the resolved commit, which is inside the trust
			// boundary but is not therefore entitled to read outside the
			// workspace. `hashFiles('../../../etc/passwd')` globs cleanly and
			// would otherwise be read and hashed, which is a file-existence
			// oracle for anything the agent user can open. Containment is
			// checked after the glob because that is where the traversal
			// actually resolves.
			if !within(root, hit) {
				return nil, fmt.Errorf("hashFiles: pattern %q escapes the workspace", pattern)
			}
			matches = append(matches, hit)
		}
	}
	sort.Strings(matches)

	outer := sha256.New()
	hashed := 0
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || info.IsDir() {
			continue
		}
		// Lstat, and a symlink is skipped rather than followed: a link
		// committed into a repository is a second way out of the workspace,
		// and the containment check above only covers the path it was
		// reached by.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		data, err := os.ReadFile(m) //#nosec G304 -- m is glob output proven within the workspace root by within(), and is not a symlink
		if err != nil {
			return nil, fmt.Errorf("hashFiles: %w", err)
		}
		sum := sha256.Sum256(data)
		outer.Write(sum[:])
		hashed++
	}
	if hashed == 0 {
		return "", nil
	}
	return hex.EncodeToString(outer.Sum(nil)), nil
}

// within reports whether path is inside root, after both are cleaned. It is the
// check that makes a glob result safe to open.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
