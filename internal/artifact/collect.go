package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Collector copies a run's declared outputs into ~/.latem/artifacts/<run-id>/.
type Collector struct {
	root string
	now  func() time.Time
}

// NewCollector builds a collector storing under root/artifacts, where root is
// the agent home (~/.latem).
func NewCollector(root string) *Collector {
	return &Collector{root: root, now: time.Now}
}

// WithClock replaces the collector's clock, for tests and for a caller that
// wants every artifact of one run stamped with the same instant.
func (c *Collector) WithClock(now func() time.Time) *Collector {
	if now == nil {
		return c
	}
	return &Collector{root: c.root, now: now}
}

// Dir is where a run's artifacts live.
//
// The run id is validated before it becomes a path. This is amendment A6: a
// control plane sending {"id": "../../../../tmp/x"} had the agent writing
// outside ~/.latem entirely, and every path the agent builds from an id — this
// one included — is a place that bug can recur.
func (c *Collector) Dir(runID string) (string, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return "", artifactErr(err, "refusing to build an artifact path from an invalid run id")
	}
	base := filepath.Join(c.root, "artifacts")
	dir := filepath.Join(base, runID)

	// Belt and braces, as in logstream.NewFileSink: whatever the id was, the
	// result must sit directly in the artifacts directory. This survives a
	// future id policy that permits something clever.
	if filepath.Dir(dir) != filepath.Clean(base) {
		return "", artifactErr(nil, "run id %q escapes the artifact directory", runID)
	}
	return dir, nil
}

// Collect resolves every declaration against the workspace, copies what
// matches, and returns one reference per artifact.
//
// The references are all that leave this machine (RUNNER_SPEC §5, §17): a
// path, a size, and a sha256 the operator can check against the file they
// download from the runner. Content stays here.
//
// A file matched by more than one declaration is stored once and keeps the
// longest retention. Two declarations, one 7d and one 30d, matching the same
// .ipa is a job author asking for it to live 30 days, and taking the first
// answer would delete it three weeks early.
func (c *Collector) Collect(
	ctx context.Context, runID, workspace string, decls []Declaration,
) ([]protocol.ArtifactRef, error) {
	dir, err := c.Dir(runID)
	if err != nil {
		return nil, err
	}

	type pending struct {
		match   Match
		expires *time.Time
	}
	var (
		order []string
		found = map[string]*pending{}
	)

	for _, decl := range decls {
		if err := ctx.Err(); err != nil {
			return nil, artifactErr(err, "collecting artifacts for run %s", runID)
		}
		matches, err := Resolve(workspace, decl.Path)
		if err != nil {
			return nil, err
		}

		var expires *time.Time
		if decl.Retain > 0 {
			t := c.now().Add(decl.Retain)
			expires = &t
		}

		for _, m := range matches {
			prev, ok := found[m.Rel]
			if !ok {
				order = append(order, m.Rel)
				found[m.Rel] = &pending{match: m, expires: expires}
				continue
			}
			// Longest retention wins, and "no expiry" is the longest of all.
			switch {
			case prev.expires == nil:
			case expires == nil:
				prev.expires = nil
			case expires.After(*prev.expires):
				prev.expires = expires
			}
		}
	}

	if len(order) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, artifactErr(err, "creating %s", dir)
	}

	// Read every artifact through a root scoped to the workspace. Resolve has
	// already established containment, and this makes the kernel enforce it a
	// second time at the moment of the open: os.Root refuses to traverse a
	// symlink out of its root at all, so the window between the check and the
	// open — during which the run's environment may still be alive and writing
	// — cannot be used to substitute a link pointing at ~/.ssh.
	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, artifactErr(unwrapPath(err), "opening the workspace")
	}
	defer func() { _ = ws.Close() }()

	refs := make([]protocol.ArtifactRef, 0, len(order))
	for _, rel := range order {
		if err := ctx.Err(); err != nil {
			return nil, artifactErr(err, "collecting artifacts for run %s", runID)
		}
		p := found[rel]
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		// Third check on the same property, for the same reason as the two
		// above: rel is built from os.ReadDir entry names, which contain no
		// separator and are never ".." — but this is the line that decides
		// where a file gets written, and it costs one comparison to say so.
		if !within(dir, dst) {
			return nil, artifactErr(nil, "artifact %q would be written outside %s", rel, dir)
		}
		size, sum, err := copyFile(ws, rel, dst)
		if err != nil {
			return nil, err
		}
		refs = append(refs, protocol.ArtifactRef{
			Path:      rel,
			SizeBytes: size,
			SHA256:    sum,
			ExpiresAt: p.expires,
		})
	}
	return refs, nil
}

// copyFile copies rel out of the workspace root to dst, returning the bytes
// written and their digest.
//
// The digest is computed from the same stream as the copy rather than by
// reading the file back. An .ipa is 50–300 MB and a release job may produce
// several; a second pass would double the I/O to compute a number that must
// describe the bytes actually stored anyway.
func copyFile(ws *os.Root, rel, dst string) (int64, string, error) {
	in, err := ws.Open(filepath.FromSlash(rel))
	if err != nil {
		return 0, "", artifactErr(unwrapPath(err), "opening artifact %q", rel)
	}
	defer func() { _ = in.Close() }() // read-only; a close error changes nothing

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, "", artifactErr(err, "creating %s", filepath.Dir(dst))
	}

	// 0o600, and the directories 0o700. An artifact is a signed build: an
	// .ipa embeds a provisioning profile, an .aab is the thing that ships to
	// users. On a Mac mini with more than one login it is nobody else's.
	// #nosec G304 -- dst is filepath.Join(<validated run dir>, <relative path
	// from Resolve>); both halves are checked for containment above.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", artifactErr(err, "creating %s", dst)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return 0, "", artifactErr(err, "copying artifact to %s", dst)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return 0, "", artifactErr(err, "writing %s", dst)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
