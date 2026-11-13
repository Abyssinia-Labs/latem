package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// A changes run (A91) builds a commit this machine already has with a set of
// files changed on top of it. The caller sends only those files. This file
// turns them into a commit in the mirror, on no ref, which a run is then
// pinned to like any other: `latem try` (A76) for someone who has the files
// and no checkout.

// FileChange is one file a changes run writes.
type FileChange struct {
	// Path is the file's path in the repository, with forward slashes. The
	// caller has validated it; git refuses what is left (a path into .git,
	// by any spelling its filesystem checks know).
	Path string
	Data []byte
	// Mode is "100644" or "100755", or empty for the mode the base has the
	// file at, and 100644 for a file the base does not have.
	Mode string
}

// Changes is what a changes run asks for.
type Changes struct {
	// Base is the commit the changes are on.
	Base   string
	Writes []FileChange
	// Removes are paths the base has and the result does not.
	Removes []string
}

// Changed is the commit [Manager.CommitChanges] made.
type Changed struct {
	// SHA is the commit, or Base itself when the changes change nothing.
	SHA string
	// Paths is how many paths differ from the base.
	Paths int
}

// ErrBaseMissing is wrapped when the mirror does not hold a changes run's
// base and it could not be fetched.
var ErrBaseMissing = errors.New("source: the base commit is not in the mirror")

// lsFilesChunk bounds the paths one `git ls-tree` is given on its argv,
// which has a limit that five thousand long paths can reach.
const lsFilesChunk = 200

// CommitChanges makes the commit a changes run builds (A91): c.Base's tree
// with c.Writes written and c.Removes removed, as a child of Base, in repo's
// mirror and on no ref.
//
// Like [Manager.AddCommit] it is a write a command makes beside a running
// agent, and safe for the same reason: it writes objects and nothing else.
// The index it works in is a temporary file, and no ref, FETCH_HEAD or pack
// is touched, so nothing the agent's own fetches lock is written here.
//
// The same base and the same changes are the same commit, every time: its
// author and dates are fixed, not the caller's or the clock's. A caller that
// asks twice gets one commit, and a run of it is found by that name.
//
// A base the mirror lacks is fetched by its name from the repository's
// remote, with this machine's own credential, under no ref. A machine with
// no mirror of the repository yet gets one first, as gate 6 makes it.
func (m *Manager) CommitChanges(ctx context.Context, repo Repo, c Changes) (Changed, error) {
	if m.readOnly {
		return Changed{}, errReader
	}
	if err := repo.validate(); err != nil {
		return Changed{}, err
	}
	if err := validateSHA(c.Base); err != nil {
		return Changed{}, err
	}
	path, err := m.mirrorPath(repo.Key)
	if err != nil {
		return Changed{}, err
	}
	lock := m.repoLock(repo.Key)
	lock.Lock()
	defer lock.Unlock()

	if _, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return Changed{}, wrapErr(protocol.ErrSource, statErr, "inspecting mirror %s", path)
		}
		if err := m.createMirror(ctx, repo, path); err != nil {
			return Changed{}, err
		}
	}
	mi := &Mirror{mgr: m, repo: repo, path: path}
	if err := mi.ensureCommit(ctx, c.Base); err != nil {
		return Changed{}, err
	}

	tmp, err := os.MkdirTemp("", "latem-changes-")
	if err != nil {
		return Changed{}, wrapErr(protocol.ErrSource, err, "making a temporary directory")
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	if _, err := m.runWith(ctx, path, index, nil, "read-tree", c.Base); err != nil {
		return Changed{}, gitErr(err, "reading the tree of %s in %s", c.Base, repo.Key)
	}

	entries, err := mi.checkChanges(ctx, c)
	if err != nil {
		return Changed{}, err
	}

	blobs, err := m.hashObjects(ctx, path, tmp, entries)
	if err != nil {
		return Changed{}, err
	}

	// update-index reads NUL-terminated records, so no path needs quoting.
	// A removal is mode 0 and the null object name.
	var info bytes.Buffer
	zero := strings.Repeat("0", len(c.Base))
	for _, p := range c.Removes {
		fmt.Fprintf(&info, "0 %s\t%s\x00", zero, p)
	}
	for i, e := range entries {
		fmt.Fprintf(&info, "%s blob %s\t%s\x00", e.Mode, blobs[i], e.Path)
	}
	if _, err := m.runWith(ctx, path, index, &info, "update-index", "-z", "--index-info"); err != nil {
		return Changed{}, gitErr(err, "applying the changes to the tree of %s", shortName(c.Base))
	}
	tree, err := m.runWith(ctx, path, index, nil, "write-tree")
	if err != nil {
		return Changed{}, gitErr(err, "writing the changed tree")
	}
	baseTree, err := m.run(ctx, path, "rev-parse", "--verify", "--end-of-options", c.Base+"^{tree}")
	if err != nil {
		return Changed{}, gitErr(err, "reading the tree of %s", c.Base)
	}
	if tree == baseTree {
		// Every file was written as the base has it. That is the base.
		return Changed{SHA: c.Base}, nil
	}

	when, err := m.run(ctx, path, "show", "-s", "--format=%ct", "--end-of-options", c.Base)
	if err != nil {
		return Changed{}, gitErr(err, "reading the date of %s", c.Base)
	}
	if _, convErr := strconv.ParseInt(when, 10, 64); convErr != nil {
		return Changed{}, codeErr(protocol.ErrSource, "the base %s has no readable date: %q", c.Base, when)
	}
	differ, err := m.runRaw(ctx, path, 64<<20, "diff-tree", "-r", "--no-renames", "--name-only", "-z", baseTree, tree)
	if err != nil {
		return Changed{}, gitErr(err, "listing what the changes change")
	}
	n := len(splitNUL(differ))

	// Fixed, so that the commit is a function of the base and the changes.
	// The base's own date keeps it from sorting after commits made later.
	identity := []string{
		"GIT_AUTHOR_NAME=latem", "GIT_AUTHOR_EMAIL=changes@latem.invalid", "GIT_AUTHOR_DATE=@" + when + " +0000",
		"GIT_COMMITTER_NAME=latem", "GIT_COMMITTER_EMAIL=changes@latem.invalid", "GIT_COMMITTER_DATE=@" + when + " +0000",
	}
	msg := fmt.Sprintf("latem: %d path(s) changed on %s", n, shortName(c.Base))
	sha, err := m.runWith(ctx, path, identity, nil, "commit-tree", "--no-gpg-sign", "-p", c.Base, "-m", msg, tree)
	if err != nil {
		return Changed{}, gitErr(err, "committing the changes on %s", shortName(c.Base))
	}
	if err := validateSHA(sha); err != nil {
		return Changed{}, err
	}
	return Changed{SHA: sha, Paths: n}, nil
}

// ensureCommit makes sure the mirror holds sha, fetching it by its name from
// the remote when it does not. Under no ref, with no FETCH_HEAD, as
// [Manager.AddCommit] fetches: objects only.
func (mi *Mirror) ensureCommit(ctx context.Context, sha string) error {
	has, err := mi.HasCommit(ctx, sha)
	if err != nil || has {
		return err
	}
	fetch := []string{"-c", "protocol.version=2", "fetch", "--no-tags", "--no-write-fetch-head",
		"--no-auto-maintenance", "--no-write-commit-graph", "--quiet", "--", mi.repo.URL, sha}
	if _, err := mi.mgr.runRemote(ctx, mi.repo, mi.path, fetch...); err != nil {
		return fmt.Errorf("%w: %s is not in this machine's mirror of %s, and fetching it from %s failed: %w",
			ErrBaseMissing, sha, mi.repo.Key, mi.repo.URL, err)
	}
	return nil
}

// OnRef reports whether commit is in the history of normalizedRef: as the
// mirror has the ref, or, when that does not hold it, as the remote has the
// ref now. tip is the ref's commit that answered.
//
// The second look is for a mirror nothing has fetched since the commit was
// pushed. It asks the remote where the ref is and fetches that commit by its
// name, objects only, as [Manager.CommitChanges] fetches a base: no ref of the
// mirror moves, so the agent's next fetch finds its mirror as it left it. A
// remote that cannot be asked is not an error here. The mirror's answer
// stands, and the caller says what it was.
func (mi *Mirror) OnRef(ctx context.Context, commit, normalizedRef string) (on bool, tip string, err error) {
	if err := validateSHA(commit); err != nil {
		return false, "", err
	}
	if !strings.HasPrefix(normalizedRef, "refs/") {
		return false, "", codeErr(protocol.ErrInvalidRef, "OnRef needs a fully-qualified ref, got %q", normalizedRef)
	}
	if has, err := mi.HasRef(ctx, normalizedRef); err != nil {
		return false, "", err
	} else if has {
		if tip, err = mi.Resolve(ctx, normalizedRef); err != nil {
			return false, "", err
		}
		if on, err = mi.IsAncestor(ctx, commit, tip); err != nil || on {
			return on, tip, err
		}
	}
	out, lsErr := mi.mgr.runRemote(ctx, mi.repo, mi.path, "ls-remote", "--", mi.repo.URL, normalizedRef)
	if lsErr != nil {
		return false, tip, nil
	}
	remote := ""
	for line := range strings.Lines(out) {
		if sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && name == normalizedRef {
			remote = sha
		}
	}
	if validateSHA(remote) != nil || remote == tip {
		return false, tip, nil
	}
	if err := mi.ensureCommit(ctx, remote); err != nil {
		return false, tip, nil
	}
	on, err = mi.IsAncestor(ctx, commit, remote)
	return on, remote, err
}

// checkChanges holds c against what its base has at each path, and returns
// the writes with their modes settled.
//
// `update-index --index-info` takes any record it is given: a path under
// .git, a file where the index has a directory, a file under a path that is
// a file. It would write a tree no checkout accepts, or one that means
// something else. So every rule a working tree would have enforced is
// enforced here, before anything is written.
func (mi *Mirror) checkChanges(ctx context.Context, c Changes) ([]FileChange, error) {
	removed := make(map[string]bool, len(c.Removes))
	for _, p := range c.Removes {
		if err := validateChangePath(p); err != nil {
			return nil, err
		}
		if removed[p] {
			return nil, codeErr(protocol.ErrSource, "the changes remove %s twice", p)
		}
		removed[p] = true
	}
	written := make(map[string]bool, len(c.Writes))
	ask := append([]string(nil), c.Removes...)
	asked := map[string]bool{}
	for _, w := range c.Writes {
		if err := validateChangePath(w.Path); err != nil {
			return nil, err
		}
		switch {
		case written[w.Path]:
			return nil, codeErr(protocol.ErrSource, "the changes write %s twice", w.Path)
		case removed[w.Path]:
			return nil, codeErr(protocol.ErrSource, "the changes both write and remove %s", w.Path)
		}
		written[w.Path] = true
		for p := w.Path; p != "."; p = pathDir(p) {
			if !asked[p] {
				asked[p] = true
				ask = append(ask, p)
			}
		}
	}
	have, err := mi.treeEntries(ctx, c.Base, ask)
	if err != nil {
		return nil, err
	}

	for _, p := range c.Removes {
		if e, ok := have[p]; !ok || e.kind == "tree" {
			return nil, codeErr(protocol.ErrSource,
				"the changes remove %s, which is not a file the base %s has", p, shortName(c.Base))
		}
	}
	entries := make([]FileChange, len(c.Writes))
	for i, w := range c.Writes {
		entries[i] = w
		e := have[w.Path]
		switch {
		case e.kind == "tree":
			return nil, codeErr(protocol.ErrSource,
				"the changes write %s, which is a directory in the base", w.Path)
		case e.mode == "120000":
			return nil, codeErr(protocol.ErrSource,
				"the changes write %s, which is a symbolic link in the base; changing one is not supported", w.Path)
		case e.mode == "160000":
			return nil, codeErr(protocol.ErrSource,
				"the changes write %s, which is a submodule in the base", w.Path)
		}
		for dir := pathDir(w.Path); dir != "."; dir = pathDir(dir) {
			if written[dir] {
				return nil, codeErr(protocol.ErrSource, "the changes write both %s and %s, a file under it", dir, w.Path)
			}
			if a, ok := have[dir]; ok && a.kind != "tree" && !removed[dir] {
				return nil, codeErr(protocol.ErrSource,
					"the changes write %s, and %s is a file in the base", w.Path, dir)
			}
		}
		switch {
		case w.Mode == "100644" || w.Mode == "100755":
		case w.Mode != "":
			return nil, codeErr(protocol.ErrSource, "%s has mode %q; a file is 100644 or 100755", w.Path, w.Mode)
		case e.mode != "":
			entries[i].Mode = e.mode
		default:
			entries[i].Mode = "100644"
		}
	}
	return entries, nil
}

// pathDir is path.Dir for a clean, relative, slash-separated path.
func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

// MaxChangePath bounds one path of a changes run, as most filesystems bound
// one.
const MaxChangePath = 4096

// validateChangePath refuses a path that is not a file's place in a
// repository: empty, absolute, not clean, climbing out, holding a control
// character, or under a .git by any spelling a filesystem here would fold to
// it. The command validates first, with a message for whoever sent it; this
// is the function that builds the tree refusing to trust that (INV-3).
func validateChangePath(p string) error {
	bad := func(why string) error {
		return codeErr(protocol.ErrSource, "the changes name the path %q, which %s", p, why)
	}
	switch {
	case p == "":
		return codeErr(protocol.ErrSource, "the changes name an empty path")
	case len(p) > MaxChangePath:
		return codeErr(protocol.ErrSource, "the changes name a path of %d bytes, limit %d", len(p), MaxChangePath)
	case strings.HasPrefix(p, "/"):
		return bad("is absolute")
	case strings.ContainsRune(p, '\\'):
		return bad("has a backslash; a path is separated by /")
	}
	if i := strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		return bad("has a control character")
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "":
			return bad("has an empty part")
		case ".", "..":
			return bad("has a " + part + " part")
		}
		// Windows and macOS both open ".GIT", and Windows ".git." and
		// "git~1", as .git. A repository is checked out on all three.
		folded := strings.ToLower(strings.TrimRight(part, ". "))
		if folded == ".git" || folded == "git~1" {
			return bad("is inside a .git")
		}
	}
	return nil
}

type treeEntry struct{ mode, kind string }

// treeEntries is what commit has at each of paths, for those it has: the
// entry itself, a directory as one entry of kind tree. GIT_LITERAL_PATHSPECS
// makes a path with a `*` in it that path, not a pattern.
func (mi *Mirror) treeEntries(ctx context.Context, commit string, paths []string) (map[string]treeEntry, error) {
	have := make(map[string]treeEntry, len(paths))
	for start := 0; start < len(paths); start += lsFilesChunk {
		chunk := paths[start:min(start+lsFilesChunk, len(paths))]
		args := append([]string{"ls-tree", "-z", "--full-tree", "--end-of-options", commit, "--"}, chunk...)
		cmd := mi.mgr.command(ctx, mi.path, args)
		cmd.Env = mi.mgr.env("GIT_LITERAL_PATHSPECS=1")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return nil, gitErr(&gitError{args: args[:1], stderr: strings.TrimSpace(stderr.String()), err: err},
				"reading the base's entries in %s", mi.repo.Key)
		}
		for _, rec := range splitNUL(stdout.Bytes()) {
			// <mode> SP <type> SP <object> TAB <path>
			meta, name, ok := strings.Cut(rec, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 {
				return nil, codeErr(protocol.ErrSource, "unreadable ls-tree entry %q", rec)
			}
			have[name] = treeEntry{mode: fields[0], kind: fields[1]}
		}
	}
	return have, nil
}

// hashObjects writes each entry's data into the repository at dir as a blob
// and returns their names, in order. The bytes are stored as they are: no
// clean filter, no line-ending conversion, since the caller sent the file as
// the repository holds it.
func (m *Manager) hashObjects(ctx context.Context, dir, tmp string, entries []FileChange) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	blobDir := filepath.Join(tmp, "blobs")
	if err := os.Mkdir(blobDir, 0o700); err != nil {
		return nil, wrapErr(protocol.ErrSource, err, "making %s", blobDir)
	}
	// The files are named by number, so a name on stdin is never a path the
	// caller chose.
	var names bytes.Buffer
	for i, e := range entries {
		file := filepath.Join(blobDir, strconv.Itoa(i))
		if err := os.WriteFile(file, e.Data, 0o600); err != nil {
			return nil, wrapErr(protocol.ErrSource, err, "staging %s", e.Path)
		}
		names.WriteString(file + "\n")
	}
	out, err := m.runWith(ctx, dir, nil, &names, "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return nil, gitErr(err, "storing the changed files")
	}
	blobs := strings.Fields(out)
	if len(blobs) != len(entries) {
		return nil, codeErr(protocol.ErrSource, "git stored %d of %d changed files", len(blobs), len(entries))
	}
	for _, b := range blobs {
		if err := validateSHA(b); err != nil {
			return nil, err
		}
	}
	return blobs, nil
}

// runWith is [Manager.run] with extra environment and, when stdin is not nil,
// input.
func (m *Manager) runWith(ctx context.Context, dir string, env []string, stdin *bytes.Buffer, args ...string) (string, error) {
	cmd := m.command(ctx, dir, args)
	if len(env) > 0 {
		cmd.Env = m.env(env...)
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	return capture(cmd, args)
}

func shortName(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
