package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/workflow/actions"
	"github.com/Abyssinia-Labs/latem/internal/workflow/expr"
)

// cacheAction is GitHub's cache action. Its restore and save halves are the
// same action at a subdirectory, and get the same treatment.
const cacheAction = "actions/cache"

// cacheShim turns `uses: actions/cache` into a cache declaration (A49).
//
// §14 says caches are mounted, not archived, and this takes it at its word:
// the `path:` becomes a declared cache mounted at that path for the whole job,
// the restore and the save are both the mount being there, and the step itself
// compiles to nothing. Like the checkout shim it needs no allowlisting —
// nothing is fetched and nothing third-party runs.
//
// The semantic difference is stated rather than hidden. A keyed cache
// replaces; a mount accumulates. `key` and `restore-keys` therefore change
// nothing about what is mounted — `runner.os` in a key is free, since §14
// already scopes a cache per runner, and a lockfile hash is the part whose
// meaning changes. For the content-addressed stores actions/cache is mostly
// pointed at — npm, pnpm, Go modules, Gradle, Cargo, pip — accumulating is
// what was wanted all along.
//
// ok is false for any other action.
func cacheShim(st Step, sc expr.Scope) (decls []registry.CacheDecl, outputs map[string]*expr.Program, ok bool, err error) {
	ref, parseErr := actions.ParseRef(st.Uses)
	if parseErr != nil || !strings.EqualFold(ref.Slug(), cacheAction) {
		return nil, nil, false, nil
	}
	switch ref.Path {
	case "", "restore", "save":
	default:
		return nil, nil, false, nil
	}

	with, err := withStrings(st.With)
	if err != nil {
		return nil, nil, true, err
	}
	for _, name := range sortedNames(with) {
		v := strings.TrimSpace(with[name])
		switch strings.ToLower(name) {
		case "path", "key", "restore-keys":
		case "upload-chunk-size", "enablecrossosarchive", "save-always":
			// Tuning for an upload that does not happen, and a deprecated
			// switch for a save that always happens.
		case "fail-on-cache-miss", "lookup-only":
			if truthy(v) && v != "" {
				return nil, nil, true, fmt.Errorf(
					"uses: %s — %s: %s asks whether an entry for this exact key exists, and a mounted "+
						"cache (§14) has no keys to answer with. The mount is always there; drop the input",
					st.Uses, name, v)
			}
		default:
			return nil, nil, true, fmt.Errorf("uses: %s has an input latem does not know: %s", st.Uses, name)
		}
	}

	raw, given := with["path"]
	if !given || strings.TrimSpace(raw) == "" {
		return nil, nil, true, fmt.Errorf("uses: %s requires the input \"path\"", st.Uses)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		target, err := cacheTarget(line, sc, st.Uses)
		if err != nil {
			return nil, nil, true, err
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		decls = append(decls, registry.CacheDecl{Name: cacheNameFor(target), Path: target})
	}

	// What a later step can read. cache-hit is 'false' because it is the safe
	// answer: the install step it gates runs, and is fast against a warm
	// mount, where 'true' would let a stale mount skip work that was needed.
	// No exact key was matched, so the matched key is empty; the primary key is
	// whatever the workflow computed, still an expression, so it is as late or
	// as early as that expression is.
	outputs = map[string]*expr.Program{
		"cache-hit":         expr.Literal("false"),
		"cache-matched-key": expr.Literal(""),
	}
	if key, ok := with["key"]; ok {
		p, err := expr.Template(key)
		if err != nil {
			return nil, nil, true, fmt.Errorf("uses: %s key: %w", st.Uses, err)
		}
		outputs["cache-primary-key"] = p
	}
	return decls, outputs, true, nil
}

// cacheTarget validates one `path:` line and resolves any expression in it.
//
// A mount is fixed when the environment is prepared, before any step runs, so
// every refusal here is something that cannot be known or cannot be a mount
// point — and each is refused at compile time, naming the line, rather than
// left to fail as E_CACHE partway into preparing.
func cacheTarget(line string, sc expr.Scope, uses string) (string, error) {
	if strings.HasPrefix(line, "!") {
		return "", fmt.Errorf(
			"uses: %s path %q is an exclusion. A mount is a directory, all of it; declare the "+
				"directories to keep rather than the ones to leave out", uses, line)
	}
	if expr.Has(line) {
		spans, err := expr.Spans(line)
		if err != nil {
			return "", fmt.Errorf("uses: %s path %q: %w", uses, line, err)
		}
		for _, sp := range spans {
			w, err := classify(sp.Prog, sc)
			if err != nil {
				return "", fmt.Errorf("uses: %s path %q: %w", uses, line, err)
			}
			if w == whenLate {
				return "", fmt.Errorf(
					"uses: %s path %q is only known once the job is running, and a cache is mounted "+
						"before the first step. Name the directory itself — for pnpm, "+
						"`pnpm config get store-dir` has a fixed answer to write down", uses, line)
			}
		}
		resolved, err := expr.EvalString(line, sc)
		if err != nil {
			return "", fmt.Errorf("uses: %s path %q: %w", uses, line, err)
		}
		line = strings.TrimSpace(resolved)
	}
	if strings.ContainsAny(line, "*?[") {
		return "", fmt.Errorf(
			"uses: %s path %q is a pattern, and a mount is one directory. Name the directory", uses, line)
	}
	if strings.Contains(line, "$") {
		return "", fmt.Errorf(
			"uses: %s path %q names an environment variable, which nothing expands in a mount "+
				"point. Write ~ or an absolute path", uses, line)
	}
	anchored := line == "~" || strings.HasPrefix(line, "~/") || strings.HasPrefix(line, "/")
	if !anchored {
		return "", fmt.Errorf(
			"uses: %s path %q is inside the workspace, and a cache mounted there would shadow the "+
				"checkout (§14). Cache the store the files come from — ~/.npm rather than "+
				"node_modules — which is what both executors can mount", uses, line)
	}
	return strings.TrimRight(line, "/"), nil
}

// cacheNameFor derives a cache name from its path alone, so every job on a
// runner that caches ~/.npm shares one mount — §14.1's reasoning, that package
// stores are content-addressed and are better shared. The readable part is for
// the operator reading `latem cache ls`; the hash is what keeps two paths that
// slugify alike apart.
func cacheNameFor(target string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(target) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[len(slug)-40:], "-")
	}
	if slug == "" {
		slug = "home"
	}
	sum := sha256.Sum256([]byte(target))
	return "gha-" + slug + "-" + hex.EncodeToString(sum[:4])
}
