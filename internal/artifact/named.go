package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// namePattern bounds an artifact's name, which becomes a directory under the
// run's artifact directory. GitHub refuses the separators and the shell
// metacharacters; this refuses everything that is not plainly a name, and a
// leading dot, so neither ".." nor a hidden directory can be spelled.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,99}$`)

// ValidName reports whether a name can be an artifact's directory.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && !strings.Contains(name, "..")
}

// Upload is one `actions/upload-artifact` step, collected when it runs (A49).
type Upload struct {
	Name    string
	Include []string
	Exclude []string
	Retain  time.Duration
	// Single is upload-artifact v7's `archive: false`: one file, kept as it
	// is, and the artifact takes the file's name. Name is ignored (A77).
	Single bool
}

// ErrNotSingle is an `archive: false` upload whose paths matched more than
// one file, which upload-artifact refuses too.
var ErrNotSingle = errors.New("artifact: archive: false uploads one file")

// CollectNamed copies what Include matches, less what Exclude matches, into
// <run>/<name>/ and reports each file as <name>/<path>. base is the directory
// the patterns are relative to: the workspace, or the run's temporary
// directory (A77).
//
// <path> is relative to the artifact's root, which is upload-artifact's own
// rule (A77): the one directory the patterns search, or the deepest one all
// of them share, or a lone file's own directory. `path: dist` stores
// dist/app.bin as app.bin, as GitHub's zip has it, and a download puts it back
// where GitHub's would.
//
// A literal that names a directory is taken as everything beneath it. That
// looks like it contradicts A30, which refuses exactly that in a job's own
// artifacts: — but A30 refuses because the author's intent is unknowable,
// and upload-artifact's own documentation defines a directory as recursive.
// The intent is known, so honouring it is not a guess. It is decided here, at
// collection time, because at compile time `dist` usually does not exist yet.
//
// Every read goes through [Collector.Collect]'s path — Resolve for
// containment, os.Root for the open — so this adds naming and subtraction and
// nothing that touches the filesystem on its own terms.
func (c *Collector) CollectNamed(ctx context.Context, runID, base string, up Upload) ([]protocol.ArtifactRef, error) {
	if !up.Single && !ValidName(up.Name) {
		return nil, registryErr("artifact name %q is not a plain name", up.Name)
	}
	runDir, err := c.Dir(runID)
	if err != nil {
		return nil, err
	}

	ws, err := os.OpenRoot(base)
	if err != nil {
		return nil, artifactErr(unwrapPath(err), "opening %s", base)
	}
	defer func() { _ = ws.Close() }()

	excluded := map[string]bool{}
	for _, pat := range up.Exclude {
		matches, err := Resolve(base, expandDir(ws, pat))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			excluded[m.Rel] = true
		}
	}

	var order []string
	seen := map[string]bool{}
	searched := make([]string, 0, len(up.Include))
	for _, pat := range up.Include {
		expanded := expandDir(ws, pat)
		searched = append(searched, expanded)
		matches, err := Resolve(base, expanded)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if excluded[m.Rel] || seen[m.Rel] {
				continue
			}
			seen[m.Rel] = true
			order = append(order, m.Rel)
		}
	}
	if len(order) == 0 {
		return nil, nil
	}

	name, root := up.Name, artifactRoot(searched, order)
	if up.Single {
		if len(order) > 1 {
			return nil, artifactErr(ErrNotSingle, "archive: false uploads one file, and %d matched", len(order))
		}
		// The file's own name, as GitHub names an unarchived artifact.
		name = path.Base(order[0])
		if !ValidName(name) {
			return nil, artifactErr(nil, "archive: false names the artifact after the file, and %q is not a plain name", name)
		}
	}

	dir := filepath.Join(runDir, name)
	if !within(runDir, dir) {
		return nil, artifactErr(nil, "artifact %q would be written outside %s", name, runDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, artifactErr(err, "creating %s", dir)
	}

	var expires *time.Time
	if up.Retain > 0 {
		t := c.now().Add(up.Retain)
		expires = &t
	}
	refs := make([]protocol.ArtifactRef, 0, len(order))
	for _, rel := range order {
		if err := ctx.Err(); err != nil {
			return nil, artifactErr(err, "collecting artifact %q", name)
		}
		stored := underRoot(root, rel)
		if up.Single {
			// The file is the artifact, wherever under the paths it was.
			stored = name
		}
		dst := filepath.Join(dir, filepath.FromSlash(stored))
		if !within(dir, dst) {
			return nil, artifactErr(nil, "artifact %q would be written outside %s", rel, dir)
		}
		size, sum, err := copyFile(ws, rel, dst)
		if err != nil {
			return nil, err
		}
		refs = append(refs, protocol.ArtifactRef{
			Path: name + "/" + stored, SizeBytes: size, SHA256: sum, ExpiresAt: expires,
		})
	}
	return refs, nil
}

// artifactRoot is the directory an upload's files are stored relative to,
// by upload-artifact's rule (its findFilesToUpload): each pattern searches
// from its literal prefix, a search path inside another is dropped, two or
// more keep their deepest common directory, and one that is itself the only
// file matched gives way to that file's directory. "" is the base itself.
//
// patterns are as Resolve takes them and files as it returns them, both
// relative to the same base, so the answer is a prefix of every file.
func artifactRoot(patterns, files []string) string {
	var searched []string
	for _, p := range patterns {
		p = path.Clean(strings.TrimSpace(p))
		if p == "." {
			p = ""
		}
		var lit []string
		for _, seg := range strings.Split(p, "/") {
			if strings.ContainsAny(seg, "*?[") {
				break
			}
			lit = append(lit, seg)
		}
		searched = append(searched, strings.Join(lit, "/"))
	}
	var kept []string
	for i, s := range searched {
		inside := false
		for j, other := range searched {
			if i != j && contains(other, s) && (other != s || j < i) {
				inside = true
				break
			}
		}
		if !inside {
			kept = append(kept, s)
		}
	}
	switch {
	case len(kept) == 0:
		return ""
	case len(kept) > 1:
		common := strings.Split(kept[0], "/")
		for _, s := range kept[1:] {
			segs := strings.Split(s, "/")
			n := 0
			for n < len(common) && n < len(segs) && common[n] == segs[n] {
				n++
			}
			common = common[:n]
		}
		return strings.Join(common, "/")
	case len(files) == 1 && files[0] == kept[0]:
		if d := path.Dir(files[0]); d != "." {
			return d
		}
		return ""
	}
	return kept[0]
}

// contains reports whether dir is p or one of its ancestors; "" is the base,
// which contains everything.
func contains(dir, p string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

// underRoot is rel with the artifact's root taken off.
func underRoot(root, rel string) string {
	if root == "" || !strings.HasPrefix(rel, root+"/") {
		return rel
	}
	return strings.TrimPrefix(rel, root+"/")
}

// expandDir turns a literal naming a directory into everything beneath it.
// Anything else — a pattern, a file, something absent — is returned unchanged
// for Resolve to answer, including with its refusals.
func expandDir(ws *os.Root, pattern string) string {
	p, err := cleanPattern(pattern)
	if err != nil || strings.ContainsAny(p, "*?[") {
		return pattern
	}
	info, err := ws.Lstat(p)
	if err != nil || !info.IsDir() {
		return pattern
	}
	return p + "/**"
}

// ErrNoSuchArtifact is a download of a name this run never uploaded.
var ErrNoSuchArtifact = errors.New("artifact: no such artifact in this run")

// Restore copies <run>/<name>/ back into base at dest, which is what
// `actions/download-artifact` means within one run (A49). base is the
// workspace or the run's temporary directory (A77). An empty name restores
// every artifact the run has, each under dest/<name>/, which is what GitHub
// does with no name.
//
// Writes go through an os.Root scoped to base, for the reason reads do: the
// run's environment is alive and may have left a symlink where a directory is
// about to be created, and the kernel refusing the traversal is the only
// check that cannot lose that race.
func (c *Collector) Restore(runID, base, name, dest string) (int, error) {
	runDir, err := c.Dir(runID)
	if err != nil {
		return 0, err
	}
	dest = strings.TrimSpace(dest)
	if dest == "" {
		dest = "."
	}
	if dest != "." {
		if dest, err = cleanPattern(dest); err != nil {
			return 0, err
		}
	}

	names := []string{name}
	if name == "" {
		if names, err = c.Names(runID); err != nil {
			return 0, err
		}
	} else if !ValidName(name) {
		return 0, registryErr("artifact name %q is not a plain name", name)
	}

	ws, err := os.OpenRoot(base)
	if err != nil {
		return 0, artifactErr(unwrapPath(err), "opening %s", base)
	}
	defer func() { _ = ws.Close() }()

	count := 0
	for _, n := range names {
		src := filepath.Join(runDir, n)
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return count, fmt.Errorf("%w: %q", ErrNoSuchArtifact, n)
		}
		base := dest
		if name == "" {
			base = path.Join(dest, n)
		}
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			out := path.Join(base, filepath.ToSlash(rel))
			if err := ws.MkdirAll(path.Dir(out), 0o750); err != nil {
				return err
			}
			if err := restoreFile(ws, p, out); err != nil {
				return err
			}
			count++
			return nil
		})
		if err != nil {
			return count, artifactErr(unwrapPath(err), "restoring artifact %q", n)
		}
	}
	return count, nil
}

// Names lists the artifacts a run has stored, sorted: the directories under
// the run's own whose names are plain ones. A run that stored none has none.
func (c *Collector) Names(runID string) ([]string, error) {
	runDir, err := c.Dir(runID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, artifactErr(err, "reading %s", runDir)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func restoreFile(ws *os.Root, src, dst string) error {
	in, err := os.Open(src) //#nosec G304 -- a path under this agent's own artifact directory, walked from a validated name
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := ws.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
