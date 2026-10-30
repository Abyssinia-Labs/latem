package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Reading a whole commit of a repository without a checkout, for a command
// that wants to know what the repository's workflows say — `latem workflows
// check --repo`, and doctor asking what this machine's repositories need
// (A57).
//
// Two rules make it safe to do from a terminal while the agent runs. A reader
// never writes a mirror: fetches into one bare repository race, and the lock
// that serialises them lives inside the agent's process, so a command typed
// beside it only ever reads, by object id, from objects that are immutable.
// And what it reads is regular blobs only — no symlink is followed and no
// submodule entered — each read when it is opened and refused past a cap.

// ErrNoMirror is what [Manager.Open] says when the agent has not made a
// repository's mirror yet. It makes one on its first plan or run.
var ErrNoMirror = errors.New("no mirror yet")

// errReader is what a reader says when asked to write.
var errReader = errors.New("source: this manager only reads; the agent owns its mirrors")

// NewReader returns a Manager that reads mirrors and writes nothing: it makes
// no directory, checks no filesystem, and refuses [Manager.Fetch] and
// [Mirror.Checkout]. cfg.WorkDir is not used.
func NewReader(cfg Config) (*Manager, error) {
	if !filepath.IsAbs(cfg.MirrorDir) {
		return nil, codeErr(protocol.ErrSource, "mirror dir %q is not absolute", cfg.MirrorDir)
	}
	resolved, err := lookGit(cfg.GitBinary)
	if err != nil {
		return nil, err
	}
	cfg.MirrorDir = filepath.Clean(cfg.MirrorDir)
	return &Manager{cfg: cfg, git: resolved, repoMu: map[string]*sync.Mutex{}, readOnly: true}, nil
}

// Open returns the mirror the agent made of repo, without fetching. It wraps
// [ErrNoMirror] when there is none.
func (m *Manager) Open(repo Repo) (*Mirror, error) {
	if err := repo.validate(); err != nil {
		return nil, err
	}
	p, err := m.mirrorPath(repo.Key)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", repo.Key, ErrNoMirror)
		}
		return nil, wrapErr(protocol.ErrSource, err, "inspecting mirror %s", p)
	}
	return &Mirror{mgr: m, repo: repo, path: p}, nil
}

// NewSnapshot clones repo's default branch into dir — which the caller owns,
// and removes — as a bare, one-commit, blobless clone: the listing arrives at
// once and each file's content only when [Mirror.Tree] reads it. It is how a
// repository is read before the agent has a mirror of it. The returned
// Mirror's manager only reads.
func NewSnapshot(ctx context.Context, cfg Config, repo Repo, dir string) (*Mirror, error) {
	cfg.MirrorDir = dir
	m, err := NewReader(cfg)
	if err != nil {
		return nil, err
	}
	if err := repo.validate(); err != nil {
		return nil, err
	}
	p, err := m.mirrorPath(repo.Key)
	if err != nil {
		return nil, err
	}
	if _, err := m.run(ctx, "", "clone", "--bare", "--depth=1", "--filter=blob:none",
		"--single-branch", "--no-tags", "--quiet", "--", repo.URL, p); err != nil {
		return nil, gitErr(err, "reading %s", repo.URL)
	}
	return &Mirror{mgr: m, repo: repo, path: p}, nil
}

// TreeLimits bounds what [Mirror.Tree] will hold and read.
type TreeLimits struct {
	// MaxFiles refuses a commit with more regular files than this.
	MaxFiles int
	// MaxFileBytes is the most of one file that is read; a larger file is
	// listed and cannot be opened.
	MaxFileBytes int
}

// DefaultTreeLimits suit reading a repository's workflows and the scripts and
// manifests they name.
var DefaultTreeLimits = TreeLimits{MaxFiles: 50000, MaxFileBytes: 1 << 20}

// Tree returns the regular files of a commit as an [fs.FS], and the commit's
// SHA. rev is a branch, a tag, HEAD — the default branch, in a mirror — or a
// SHA. Directories are those the files imply; a symlink or a submodule is not
// there at all.
//
// The listing is read at once and each file when it is opened. That keeps a
// blobless clone ([NewSnapshot]) to the files actually read; it also means
// the FS answers with ctx's lifetime, and a read after ctx ends fails.
func (mi *Mirror) Tree(ctx context.Context, rev string, lim TreeLimits) (fs.FS, string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	if strings.ContainsAny(rev, "\x00\n") {
		return nil, "", codeErr(protocol.ErrInvalidRef, "%q is not a revision", rev)
	}
	sha, err := mi.mgr.run(ctx, mi.path, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || validateSHA(sha) != nil {
		return nil, "", codeErr(protocol.ErrInvalidRef, "%s is not a commit in %s", rev, mi.repo.Key)
	}
	// No -l: the size of a blob a blobless clone has not fetched is a fetch.
	out, err := mi.mgr.runRaw(ctx, mi.path, 64<<20, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", sha)
	if err != nil {
		return nil, "", gitErr(err, "listing %s in %s", sha, mi.repo.Key)
	}
	t := &commitFS{
		ctx: ctx, mi: mi, max: lim.MaxFileBytes,
		files: map[string]string{}, dirs: map[string]map[string]bool{".": {}}, read: map[string]commitFile{},
	}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// <mode> SP <type> SP <object> TAB <path>
		meta, name, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, "", codeErr(protocol.ErrSource, "unreadable ls-tree entry %q", rec)
		}
		mode, kind, oid := fields[0], fields[1], fields[2]
		if kind != "blob" || (mode != "100644" && mode != "100755") || !fs.ValidPath(name) {
			continue
		}
		if validateSHA(oid) != nil {
			return nil, "", codeErr(protocol.ErrSource, "unreadable object name in ls-tree entry %q", rec)
		}
		if len(t.files) == lim.MaxFiles {
			return nil, "", codeErr(protocol.ErrSource, "%s at %s holds more than %d files", mi.repo.Key, sha, lim.MaxFiles)
		}
		t.files[name] = oid
		t.add(name, false)
	}
	return t, sha, nil
}

// commitFS is one commit's regular files, read by object id when opened.
type commitFS struct {
	ctx   context.Context //nolint:containedctx // an fs.FS has no context of its own; this is Tree's
	mi    *Mirror
	max   int
	files map[string]string          // path → blob id
	dirs  map[string]map[string]bool // dir → child name → is a directory

	mu   sync.Mutex
	read map[string]commitFile
}

type commitFile struct {
	data []byte
	err  error
}

var (
	_ fs.ReadDirFS  = (*commitFS)(nil)
	_ fs.ReadFileFS = (*commitFS)(nil)
	_ fs.StatFS     = (*commitFS)(nil)
)

// add records name under its directory, and each directory under its parent.
func (t *commitFS) add(name string, isDir bool) {
	dir := path.Dir(name)
	children, ok := t.dirs[dir]
	if !ok {
		children = map[string]bool{}
		t.dirs[dir] = children
		t.add(dir, true)
	}
	children[path.Base(name)] = isDir
}

func (t *commitFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if _, ok := t.dirs[name]; ok {
		entries, _ := t.ReadDir(name)
		return &commitDir{name: name, entries: entries}, nil
	}
	data, err := t.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &commitHandle{Reader: bytes.NewReader(data), name: path.Base(name)}, nil
}

// commitHandle is an open file. Its Stat agrees with the FS's, size 0
// included, so a file looks the same however it was reached.
type commitHandle struct {
	*bytes.Reader
	name string
}

func (h *commitHandle) Stat() (fs.FileInfo, error) { return commitInfo{name: h.name}, nil }
func (h *commitHandle) Close() error               { return nil }

func (t *commitFS) ReadFile(name string) ([]byte, error) {
	oid, ok := t.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f, done := t.read[name]
	if !done {
		f.data, f.err = t.mi.mgr.runRaw(t.ctx, t.mi.path, t.max, "cat-file", "blob", oid)
		if f.err != nil && strings.Contains(f.err.Error(), "wrote more than") {
			f.err = fmt.Errorf("%s is larger than %d bytes", name, t.max)
		}
		t.read[name] = f
	}
	if f.err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: f.err}
	}
	return bytes.Clone(f.data), nil
}

func (t *commitFS) ReadDir(name string) ([]fs.DirEntry, error) {
	children, ok := t.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(children))
	for child, isDir := range children {
		out = append(out, fs.FileInfoToDirEntry(commitInfo{name: child, dir: isDir}))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Stat answers for a file without reading it, so a walk costs no fetch. A
// file's size is not known until it is read, and is reported as 0 everywhere.
func (t *commitFS) Stat(name string) (fs.FileInfo, error) {
	if _, ok := t.dirs[name]; ok {
		return commitInfo{name: path.Base(name), dir: true}, nil
	}
	if _, ok := t.files[name]; ok {
		return commitInfo{name: path.Base(name)}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

type commitDir struct {
	name    string
	entries []fs.DirEntry
	off     int
}

func (d *commitDir) Stat() (fs.FileInfo, error) {
	return commitInfo{name: path.Base(d.name), dir: true}, nil
}
func (d *commitDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}
func (d *commitDir) Close() error { return nil }

func (d *commitDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.off:]
	if n <= 0 {
		d.off = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.off += n
	return rest[:n], nil
}

type commitInfo struct {
	name string
	dir  bool
}

func (i commitInfo) Name() string { return i.name }
func (i commitInfo) Size() int64  { return 0 }
func (i commitInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i commitInfo) ModTime() time.Time { return time.Time{} }
func (i commitInfo) IsDir() bool        { return i.dir }
func (i commitInfo) Sys() any           { return nil }
