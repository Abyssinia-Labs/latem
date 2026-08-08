package native

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// maxGlobDepth bounds the recursive descent a `**` pattern performs.
//
// Symlinked directories are followed — refusing to follow them would turn an
// escape attempt into a silent zero-match instead of an E_ARTIFACT — so a cycle
// is reachable, and a resolved-path set alone does not bound a pathological
// tree. This is a backstop, not a policy: no real workspace is this deep.
const maxGlobDepth = 64

// collectArtifacts resolves an artifact glob against the workspace (§17).
//
// The containment rules matter more here than anywhere else in the system,
// because there is no container boundary underneath them. In the docker
// executor a glob that escaped the workspace would still be confined to the
// image; here the workspace sits in a real home directory next to ~/.ssh,
// ~/Library/Keychains and the operator's other repositories, and the check in
// [globber.checkContained] is the only thing between a job's `artifacts:` block
// and any file the agent's user can read.
//
// Two failure modes are worth naming. A pattern that escapes by its own text —
// "/etc/passwd", "../../secrets" — is rejected before any I/O. A pattern that
// escapes through the filesystem — a symlink in the workspace pointing at a
// home directory — is rejected when the match is resolved. Both are E_ARTIFACT,
// because both are a job asking the agent to copy a file it was not offered,
// and the second is the one that actually happens: a build tool writes a
// convenience symlink, a glob picks it up, and the artifact archive quietly
// acquires somebody's private key.
//
// Only regular files are returned. A directory that matches is not an artifact,
// and neither is a socket or a device.
//
// The semantics are deliberately identical to the docker executor's, including
// "a directory is not an artifact": a job's `artifacts:` block must mean the
// same thing on every runner, and the interface's own doc comment says why.
func collectArtifacts(workspace, pattern string) ([]executor.File, error) {
	segs, err := globSegments(pattern)
	if err != nil {
		return nil, err
	}

	// Resolve the workspace once. Containment is then checked against the real
	// path, so a workspace that is itself reached through a symlink — /tmp on
	// macOS is exactly this, and so is every t.TempDir under it — does not make
	// every match look like an escape.
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, wrapErr(protocol.ErrArtifact, err, "resolve workspace %q", workspace)
	}

	g := &globber{root: root, matches: map[string]struct{}{}, visited: map[string]struct{}{}}
	if err := g.walk(root, segs, 0); err != nil {
		return nil, err
	}

	rels := make([]string, 0, len(g.matches))
	for m := range g.matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			return nil, wrapErr(protocol.ErrArtifact, err, "relativise %q", m)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	files := make([]executor.File, 0, len(rels))
	for _, rel := range rels {
		full := filepath.Join(root, rel)
		info, err := os.Stat(full)
		if err != nil {
			// Raced with the job deleting it between match and stat. Not an
			// error: the artifact simply is not there any more.
			continue
		}
		files = append(files, executor.File{
			Path:      filepath.ToSlash(rel),
			SizeBytes: info.Size(),
			Mode:      uint32(info.Mode().Perm()),
			Content:   &lazyFile{path: full},
		})
	}
	return files, nil
}

// globSegments validates a pattern and splits it into path segments.
//
// The textual rules are deliberately strict. An absolute pattern is not
// "relative to the workspace root" whatever else it is; a ".." segment is a
// request to leave; and an empty pattern matching everything by accident is how
// an artifact declaration turns into an exfiltration primitive.
func globSegments(pattern string) ([]string, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, codeErr(protocol.ErrArtifact, "artifact glob is empty")
	}
	if strings.ContainsRune(pattern, 0) {
		return nil, codeErr(protocol.ErrArtifact, "artifact glob contains a NUL byte")
	}
	if path.IsAbs(pattern) {
		return nil, codeErr(protocol.ErrArtifact,
			"artifact glob %q is absolute; globs are relative to the workspace root", pattern)
	}

	var segs []string
	for _, seg := range strings.Split(pattern, "/") {
		switch seg {
		case "", ".":
			// "a//b" and "./a" are sloppy but harmless.
			continue
		case "..":
			return nil, codeErr(protocol.ErrArtifact,
				"artifact glob %q escapes the workspace", pattern)
		}
		if _, err := path.Match(seg, ""); err != nil && seg != "**" {
			return nil, wrapErr(protocol.ErrArtifact, err, "artifact glob %q is malformed", pattern)
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return nil, codeErr(protocol.ErrArtifact, "artifact glob %q names nothing", pattern)
	}
	return segs, nil
}

// globber accumulates matches for one pattern.
type globber struct {
	root    string
	matches map[string]struct{}
	visited map[string]struct{}
}

// walk matches segs against the tree rooted at dir.
//
// The `**` semantics match the ref globs in RUNNER_SPEC §5: a single star
// matches within one segment and does not cross a "/", a double star matches
// zero or more whole segments. So dist/*.apk is the apks in dist and
// dist/**/*.apk is the apks anywhere under it.
//
// The walk is pruned by the pattern rather than being a full tree walk plus a
// filter, because a workspace with node_modules in it makes the difference
// between milliseconds and seconds.
func (g *globber) walk(dir string, segs []string, depth int) error {
	if len(segs) == 0 || depth > maxGlobDepth {
		return nil
	}
	seg := segs[0]
	last := len(segs) == 1

	// `**` matches zero segments: try the rest of the pattern right here.
	if seg == "**" && !last {
		if err := g.walk(dir, segs[1:], depth); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		// An unreadable directory is not a match and not a failure. A build
		// that chmod 000's one of its own output directories should not take
		// artifact collection down with it.
		return nil //nolint:nilerr // documented above: unreadable is "no match".
	}

	for _, ent := range entries {
		full := filepath.Join(dir, ent.Name())

		if seg == "**" {
			// `**` also matches this segment, and possibly more.
			isDir, err := g.enterable(full)
			if err != nil {
				return err
			}
			switch {
			case isDir:
				if err := g.descend(full, segs, depth); err != nil {
					return err
				}
			case last:
				if err := g.emit(full); err != nil {
					return err
				}
			}
			continue
		}

		ok, err := path.Match(seg, ent.Name())
		if err != nil {
			return wrapErr(protocol.ErrArtifact, err, "artifact glob segment %q is malformed", seg)
		}
		if !ok {
			continue
		}
		if last {
			if err := g.emit(full); err != nil {
				return err
			}
			continue
		}
		isDir, err := g.enterable(full)
		if err != nil {
			return err
		}
		if isDir {
			if err := g.descend(full, segs[1:], depth); err != nil {
				return err
			}
		}
	}
	return nil
}

// descend recurses into a directory once, guarding against symlink cycles.
func (g *globber) descend(dir string, segs []string, depth int) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil //nolint:nilerr // a broken link is not a match.
	}
	if _, seen := g.visited[real]; seen {
		return nil
	}
	g.visited[real] = struct{}{}
	return g.walk(dir, segs, depth+1)
}

// enterable reports whether the entry is a directory the walk may descend into,
// rejecting one that resolves outside the workspace.
//
// os.Stat follows symlinks on purpose: a symlinked directory is a legitimate
// thing to glob through, and refusing to look would turn the escape case into a
// silent miss rather than the E_ARTIFACT §17 asks for.
func (g *globber) enterable(p string) (bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		return false, nil //nolint:nilerr // broken symlink or a race with the job.
	}
	if !info.IsDir() {
		return false, nil
	}
	if err := g.checkContained(p); err != nil {
		return false, err
	}
	return true, nil
}

// emit records a match after verifying it is a regular file inside the
// workspace.
func (g *globber) emit(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return nil //nolint:nilerr // broken symlink or a race with the job.
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if err := g.checkContained(p); err != nil {
		return err
	}
	g.matches[p] = struct{}{}
	return nil
}

// checkContained is the escape check §17 requires: resolve, then verify the
// result is inside the workspace.
func (g *globber) checkContained(p string) error {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil //nolint:nilerr // unresolvable is not a match; nothing is copied.
	}
	if !within(g.root, real) {
		return codeErr(protocol.ErrArtifact,
			"artifact %q resolves to %q, outside the workspace", p, real)
	}
	return nil
}

// within reports whether p is root or is inside it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// lazyFile opens its file on first read.
//
// [executor.File] hands the caller an open io.ReadCloser, and a glob matching a
// few thousand files would otherwise exhaust the process's descriptors before
// the caller had read one of them. Not safe for concurrent use; the caller reads
// one artifact at a time.
type lazyFile struct {
	path string
	f    *os.File
	err  error
}

func (l *lazyFile) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	if l.f == nil {
		// #nosec G304 -- path is a glob match already confined to the workspace
		// by globber.checkContained, which resolves symlinks and rejects
		// anything landing outside it.
		f, err := os.Open(l.path)
		if err != nil {
			l.err = err
			return 0, err
		}
		l.f = f
	}
	return l.f.Read(p)
}

func (l *lazyFile) Close() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}
