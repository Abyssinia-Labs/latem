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
}

// CollectNamed copies what Include matches, less what Exclude matches, into
// <run>/<name>/ and reports each file as <name>/<path>.
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
func (c *Collector) CollectNamed(ctx context.Context, runID, workspace string, up Upload) ([]protocol.ArtifactRef, error) {
	if !ValidName(up.Name) {
		return nil, registryErr("artifact name %q is not a plain name", up.Name)
	}
	runDir, err := c.Dir(runID)
	if err != nil {
		return nil, err
	}

	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, artifactErr(unwrapPath(err), "opening the workspace")
	}
	defer func() { _ = ws.Close() }()

	excluded := map[string]bool{}
	for _, pat := range up.Exclude {
		matches, err := Resolve(workspace, expandDir(ws, pat))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			excluded[m.Rel] = true
		}
	}

	var order []string
	seen := map[string]bool{}
	for _, pat := range up.Include {
		matches, err := Resolve(workspace, expandDir(ws, pat))
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

	dir := filepath.Join(runDir, up.Name)
	if !within(runDir, dir) {
		return nil, artifactErr(nil, "artifact %q would be written outside %s", up.Name, runDir)
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
			return nil, artifactErr(err, "collecting artifact %q", up.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if !within(dir, dst) {
			return nil, artifactErr(nil, "artifact %q would be written outside %s", rel, dir)
		}
		size, sum, err := copyFile(ws, rel, dst)
		if err != nil {
			return nil, err
		}
		refs = append(refs, protocol.ArtifactRef{
			Path: up.Name + "/" + rel, SizeBytes: size, SHA256: sum, ExpiresAt: expires,
		})
	}
	return refs, nil
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

// Restore copies <run>/<name>/ back into the workspace at dest, which is what
// `actions/download-artifact` means within one run (A49). An empty name
// restores every artifact the run has, each under dest/<name>/, which is what
// GitHub does with no name.
//
// Writes go through an os.Root scoped to the workspace, for the reason reads
// do: the run's environment is alive and may have left a symlink where a
// directory is about to be created, and the kernel refusing the traversal is
// the only check that cannot lose that race.
func (c *Collector) Restore(runID, workspace, name, dest string) (int, error) {
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
		entries, err := os.ReadDir(runDir)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, artifactErr(err, "reading %s", runDir)
		}
		names = names[:0]
		for _, e := range entries {
			if e.IsDir() && ValidName(e.Name()) {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
	} else if !ValidName(name) {
		return 0, registryErr("artifact name %q is not a plain name", name)
	}

	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return 0, artifactErr(unwrapPath(err), "opening the workspace")
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
