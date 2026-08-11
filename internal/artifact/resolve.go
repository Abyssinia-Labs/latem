package artifact

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxDepth bounds directory recursion. Symlink cycles are already handled by
// the visited set; this is the backstop for anything else that manages to
// produce an unbounded tree, so that a hostile repository cannot turn artifact
// collection into a stack overflow.
const maxDepth = 100

// Match is one resolved artifact.
type Match struct {
	// Rel is the workspace-relative, slash-separated path. It is the
	// artifact's identity: what gets stored under ~/.latem/artifacts/<run-id>/
	// and what appears in [protocol.ArtifactRef.Path].
	Rel string
	// Path is the absolute real path, with every symlink already resolved and
	// checked for containment.
	//
	// It is for reporting and for sizing. The collector deliberately does not
	// open it: it opens Rel through an [os.Root] scoped to the workspace, so
	// the kernel refuses a traversal out of the tree even if a still-running
	// process swaps a symlink between this check and that open.
	Path string
	Size int64
}

// Resolve expands one glob within a workspace.
//
// Semantics, matching the ref policy globs in RUNNER_SPEC §5 so that a job
// author learns one set of rules: a single star matches within one path
// segment and does not cross a "/", a double star matches zero or more whole
// segments.
//
// A *pattern* that matches a directory expands to the regular files beneath it.
// A *literal path* that names a directory does not: it is E_BAD_REGISTRY, and
// the message names the two spellings that would have worked (amendment A30).
// The difference is what the author wrote. "dist/**" describes a set and a
// directory in it is unremarkable; "dist" names one thing, and if that thing is
// a directory the declaration cannot be honoured as written. Guessing between
// "collect everything under it" and "collect nothing" is the choice this
// refuses to make silently — the first is unbounded against a 50 GB budget
// evicted oldest-first, and the second leaves an operator believing a release
// was collected when it was not.
//
// Nothing outside the workspace can be returned, and an attempt to leave it is
// an error rather than an omission:
//
//   - Absolute patterns and patterns containing ".." are refused before any
//     filesystem access, so a hostile registry costs a string comparison.
//   - Every symlink that a match either is or is reached through is resolved
//     and checked against the real workspace root. One pointing outside fails
//     the whole call with E_ARTIFACT.
//
// Failing rather than skipping is the deliberate half. A silent skip means an
// operator believes an artifact was collected when it was not, and it means a
// symlink to ~/.ssh/id_rsa in a build output directory produces no signal at
// all. Neither is a trade worth making to keep a misconfigured job green.
//
// A pattern matching nothing is not an error: build outputs are legitimately
// conditional, and failing a 40-minute release because a debug symbol bundle
// was not produced would make artifact declarations something people avoid.
func Resolve(workspace, pattern string) ([]Match, error) {
	pat, err := cleanPattern(pattern)
	if err != nil {
		return nil, err
	}

	root, err := realRoot(workspace)
	if err != nil {
		return nil, err
	}

	r := &resolver{
		root:    root,
		pattern: strings.Split(pat, "/"),
		literal: !strings.ContainsAny(pat, "*?["),
		seen:    map[string]bool{},
	}
	if err := r.walk(root, "", 0); err != nil {
		return nil, err
	}

	sort.Slice(r.out, func(i, j int) bool { return r.out[i].Rel < r.out[j].Rel })
	return r.out, nil
}

// cleanPattern rejects a glob that cannot possibly stay inside the workspace.
//
// This runs before any I/O. The registry already applies the same two checks
// at parse time, and repeating them here is deliberate: this function is also
// reachable with a pattern that came from somewhere else, and a containment
// check that only one caller performs is the shape of the next incident (see
// protocol.ValidateID for the same argument about run ids).
func cleanPattern(pattern string) (string, error) {
	p := strings.TrimSpace(pattern)
	switch {
	case p == "":
		return "", artifactErr(nil, "artifact path is empty")
	case strings.HasPrefix(p, "/") || filepath.IsAbs(p):
		return "", artifactErr(nil, "artifact path %q is absolute; it must be relative to the workspace root", pattern)
	case strings.HasPrefix(p, "~"):
		return "", artifactErr(nil, "artifact path %q starts with ~; paths are relative to the workspace root and are not expanded", pattern)
	case strings.ContainsRune(p, 0):
		return "", artifactErr(nil, "artifact path contains a NUL byte")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", artifactErr(nil, "artifact path %q escapes the workspace root", pattern)
		}
	}

	p = path.Clean(p)
	// Clean turns "./" into "." and "foo/" into "foo"; a bare "." would match
	// the root itself, which is a request to collect the entire workspace.
	if p == "." {
		return "", artifactErr(nil, "artifact path %q names the workspace root", pattern)
	}
	return p, nil
}

// realRoot resolves the workspace to a path with no symlinks left in it.
//
// This is not pedantry. On macOS every t.TempDir and every path under /var is
// reached through a symlink (/var → /private/var), so a containment check that
// compared a resolved match against an unresolved root would reject every
// artifact on the platform the mobile pipeline exists for. Resolving both
// sides is what makes the check mean "same tree" rather than "same spelling".
func realRoot(workspace string) (string, error) {
	if workspace == "" {
		return "", artifactErr(nil, "workspace is empty")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", artifactErr(err, "resolving workspace %q", workspace)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", artifactErr(err, "resolving workspace %q", workspace)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", artifactErr(err, "reading workspace %q", workspace)
	}
	if !info.IsDir() {
		return "", artifactErr(nil, "workspace %q is not a directory", workspace)
	}
	return resolved, nil
}

type resolver struct {
	root    string
	pattern []string
	// literal is true when the declaration contained no glob metacharacter, so
	// it named one path rather than describing a set. A literal that turns out
	// to be a directory is E_BAD_REGISTRY (amendment A30).
	literal bool
	// seen holds resolved directories already walked, so a symlink loop inside
	// the workspace terminates instead of recursing until the stack runs out.
	seen map[string]bool
	out  []Match
}

func (r *resolver) walk(dir, rel string, depth int) error {
	if depth > maxDepth {
		return artifactErr(nil, "artifact search exceeded %d directory levels at %q", maxDepth, rel)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return artifactErr(unwrapPath(err), "reading %q", r.display(dir))
	}

	for _, e := range entries {
		childRel := e.Name()
		if rel != "" {
			childRel = rel + "/" + e.Name()
		}
		segs := strings.Split(childRel, "/")
		matched := matchSegments(r.pattern, segs)
		descend := matched || canDescend(r.pattern, segs)

		switch {
		case e.Type()&os.ModeSymlink != 0:
			if !descend {
				continue // not on any path the glob can reach
			}
			if err := r.follow(filepath.Join(dir, e.Name()), childRel, matched, depth); err != nil {
				return err
			}

		case e.IsDir():
			if matched && r.literal {
				// The author named a path, and it is a directory. Neither
				// available silence is acceptable: expanding it is unbounded
				// against a 50 GB budget evicted oldest-first, so one
				// `artifacts: [dist]` over a directory of build intermediates
				// quietly deletes *other runs'* artifacts; and matching
				// nothing leaves an operator believing a release was
				// collected when it was not. Say so in seconds instead
				// (amendment A30).
				return registryErr(
					"artifact path %q names a directory, not a file; write %q to collect "+
						"the files beneath it, or %q for its immediate children",
					childRel, childRel+"/**", childRel+"/*")
			}
			// A glob that matched a directory contributes nothing of its own:
			// a directory is not an artifact. Whether the walk continues into
			// it is `descend`'s business, which is what makes "*" stop at one
			// segment and "**" not.
			if !descend {
				continue
			}
			if err := r.walk(filepath.Join(dir, e.Name()), childRel, depth+1); err != nil {
				return err
			}

		case e.Type().IsRegular():
			if !matched {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue // vanished mid-walk; a build may still be running
			}
			r.add(Match{Rel: childRel, Path: filepath.Join(dir, e.Name()), Size: info.Size()})

			// Anything else — a socket, a device node, a fifo — is not an
			// artifact. Copying one would either block forever or produce
			// nonsense, and no build output is legitimately one of these.
		}
	}
	return nil
}

// follow resolves a symlink and enforces containment.
//
// The containment check is the security boundary this package exists for, so
// it is applied to every symlink the glob can reach — not only to the ones it
// matches. A pattern of "build/*.ipa" against a `build` symlink pointing at
// /tmp would otherwise return nothing at all, which reads as "no artifacts"
// when the truth is "your artifact declaration points off the machine".
func (r *resolver) follow(link, rel string, matched bool, depth int) error {
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		// Broken links are ordinary in a build tree. Decide on the target's
		// spelling instead: one that points outside is still a declaration of
		// intent worth refusing, and one that points inside is simply missing.
		target, rerr := lexicalTarget(link)
		if rerr != nil {
			return artifactErr(rerr, "reading symlink %q", rel)
		}
		if !within(r.root, target) {
			return r.escape(rel, target)
		}
		return nil
	}
	if !within(r.root, resolved) {
		return r.escape(rel, resolved)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil // raced with a removal; nothing to collect
	}

	switch {
	case info.IsDir():
		// A contained symlinked directory is walked, because a build that
		// links its output directory somewhere else in the workspace has done
		// nothing wrong. The visited set stops a cycle.
		if r.seen[resolved] {
			return nil
		}
		r.seen[resolved] = true
		if matched && r.literal {
			return registryErr(
				"artifact path %q names a directory, not a file; write %q to collect "+
					"the files beneath it, or %q for its immediate children",
				rel, rel+"/**", rel+"/*")
		}
		return r.walk(resolved, rel, depth+1)

	case info.Mode().IsRegular():
		if matched {
			r.add(Match{Rel: rel, Path: resolved, Size: info.Size()})
		}
	}
	return nil
}

func (r *resolver) escape(rel, target string) error {
	return artifactErr(nil,
		"artifact %q is a symlink to %q, which is outside the workspace", rel, target)
}

func (r *resolver) add(m Match) {
	r.out = append(r.out, m)
}

// display renders an absolute path relative to the workspace for messages, so
// an error never leaks more of the host's layout than the job already knows.
func (r *resolver) display(p string) string {
	if rel, err := filepath.Rel(r.root, p); err == nil {
		return rel
	}
	return p
}

// unwrapPath strips the absolute path an *fs.PathError carries, leaving the
// reason. The message has already named the file the way the job spells it,
// and "reading \"build\": permission denied" is both more useful and less
// revealing than the same sentence with /Users/someone/.latem/work/… in it.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// lexicalTarget is where a symlink points, for the case where the target does
// not exist and filepath.EvalSymlinks therefore cannot say.
//
// The result is still put through [deepestReal], because the containment check
// compares against a workspace root with every symlink resolved out of it. On
// macOS that is not a corner case: /var is a symlink to /private/var, so a
// dangling link inside a workspace under /var would read as an escape if the
// two sides were spelled differently.
func lexicalTarget(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return deepestReal(filepath.Clean(target)), nil
}

// deepestReal resolves the longest existing prefix of p and re-attaches the
// rest, so a path naming a file that does not exist is still comparable with a
// root that has been through filepath.EvalSymlinks.
func deepestReal(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether p is root or sits beneath it.
//
// filepath.Rel rather than strings.HasPrefix: "/work/ws" is not a prefix
// relationship away from "/work/ws-evil", and a string comparison says it is.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// matchSegments is the glob matcher. A single star stays within one segment; a
// double star spans zero or more whole segments.
//
// So build/* matches build/app.ipa but not build/ios/app.ipa, and build/**
// matches both. The distinction is the difference between "the build outputs"
// and "the build outputs and everything anyone nests under them", and it is
// the same rule the ref policy uses (RUNNER_SPEC §5).
func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], name[0])
		if err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// canDescend reports whether anything beneath dir could still match.
//
// It is what keeps collection off the hot path of a workspace with a 200k-file
// node_modules in it: a pattern of "build/*.ipa" reads the workspace root and
// then descends into exactly one directory.
func canDescend(pattern, dir []string) bool {
	for len(dir) > 0 {
		if len(pattern) == 0 {
			return false
		}
		if pattern[0] == "**" {
			return true
		}
		ok, err := path.Match(pattern[0], dir[0])
		if err != nil || !ok {
			return false
		}
		pattern, dir = pattern[1:], dir[1:]
	}
	return true
}
