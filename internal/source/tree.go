package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ReadDir reads one directory of a commit out of the mirror, without a
// checkout, as an [fs.FS] holding that directory's regular files and nothing
// else (A52).
//
// A plan reads .github/workflows at the commit an event names, and a checkout
// to read a few kilobytes of YAML would be a workspace per webhook. Reading
// the blobs by object id is also exact in a way a checkout is not: no
// smudge filter, no LFS pointer resolution, no symlink followed — a symlink
// in the tree is not a regular file and is left out, which is what keeps the
// directory from naming a file elsewhere in the repository.
//
// It refuses more than maxFiles entries, and a file over maxBytes is present
// but cannot be read, so the plan reports that workflow rather than this
// reading a blob of any size into memory.
func (mi *Mirror) ReadDir(ctx context.Context, sha, dir string, maxFiles, maxBytes int) (fs.FS, error) {
	if err := validateSHA(sha); err != nil {
		return nil, err
	}
	dir = path.Clean(dir)
	if !fs.ValidPath(dir) || dir == "." {
		return nil, codeErr(protocol.ErrSource, "%q is not a directory in a repository", dir)
	}
	if ok, err := mi.HasCommit(ctx, sha); err != nil {
		return nil, err
	} else if !ok {
		return nil, codeErr(protocol.ErrInvalidRef, "commit %s is not in the mirror of %s", sha, mi.repo.Key)
	}

	out, err := mi.mgr.runRaw(ctx, mi.path, 1<<20, "ls-tree", "-l", "-z", "--end-of-options", sha, "--", dir+"/")
	if err != nil {
		return nil, gitErr(err, "listing %s at %s in %s", dir, sha, mi.repo.Key)
	}
	tree := &treeFS{dir: dir, files: map[string]treeFile{}}
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// <mode> SP <type> SP <object> SP+ <size> TAB <path>
		meta, name, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 {
			return nil, codeErr(protocol.ErrSource, "unreadable ls-tree entry %q", rec)
		}
		mode, kind, oid, size := fields[0], fields[1], fields[2], fields[3]
		if kind != "blob" || (mode != "100644" && mode != "100755") || path.Dir(name) != dir {
			continue
		}
		if len(tree.files) == maxFiles {
			return nil, codeErr(protocol.ErrBadRegistry, "%s at %s holds more than %d files", dir, sha, maxFiles)
		}
		n, err := strconv.Atoi(size)
		if err != nil {
			return nil, codeErr(protocol.ErrSource, "unreadable size in ls-tree entry %q", rec)
		}
		if n > maxBytes {
			tree.files[name] = treeFile{err: fmt.Errorf("%s is larger than %d bytes", name, maxBytes)}
			continue
		}
		if err := validateSHA(oid); err != nil {
			return nil, err
		}
		data, err := mi.mgr.runRaw(ctx, mi.path, maxBytes, "cat-file", "blob", oid)
		if err != nil {
			return nil, gitErr(err, "reading %s at %s in %s", name, sha, mi.repo.Key)
		}
		tree.files[name] = treeFile{data: data}
	}
	return tree, nil
}

// Changed lists the paths that differ between two commits, for a paths filter
// (A52). from may be empty — a new branch, or a pull request's merge commit
// diffed against its base — and then the commit is compared with its first
// parent, or, for a root commit, every path in it counts as changed. A from
// the mirror does not have, after a force-push, is treated as empty.
func (mi *Mirror) Changed(ctx context.Context, from, to string) ([]string, error) {
	if err := validateSHA(to); err != nil {
		return nil, err
	}
	if from != "" {
		if err := validateSHA(from); err != nil {
			return nil, err
		}
		if ok, err := mi.HasCommit(ctx, from); err != nil {
			return nil, err
		} else if !ok {
			from = ""
		}
	}
	if from == "" {
		parent, err := mi.mgr.runQuery(ctx, mi.path, "rev-parse", "--verify", "--quiet", "--end-of-options", to+"^1")
		if err != nil {
			return nil, gitErr(err, "finding the parent of %s in %s", to, mi.repo.Key)
		}
		if !parent {
			out, err := mi.mgr.runRaw(ctx, mi.path, 64<<20, "ls-tree", "-r", "--name-only", "-z", "--end-of-options", to)
			if err != nil {
				return nil, gitErr(err, "listing %s in %s", to, mi.repo.Key)
			}
			return splitNUL(out), nil
		}
		from = to + "^1"
	}
	out, err := mi.mgr.runRaw(ctx, mi.path, 64<<20,
		"diff", "--name-only", "-z", "--no-renames", "--end-of-options", from, to, "--")
	if err != nil {
		return nil, gitErr(err, "comparing %s with %s in %s", from, to, mi.repo.Key)
	}
	return splitNUL(out), nil
}

func splitNUL(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// runRaw executes git and returns its stdout as it is — not trimmed, since a
// blob's bytes are its content — refusing more than limit bytes of it.
func (m *Manager) runRaw(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	cmd := m.command(ctx, dir, args)
	stdout := &cappedBuffer{limit: limit}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.over {
		return nil, fmt.Errorf("git %s wrote more than %d bytes", args[0], limit)
	}
	if err != nil {
		return nil, &gitError{args: args, stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	return stdout.Bytes(), nil
}

// cappedBuffer stops accepting output past its limit, so a blob of any size
// cannot be read into memory whole before it is refused.
type cappedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.limit {
		c.over = true
		return 0, errors.New("output limit reached")
	}
	return c.Buffer.Write(p)
}

// treeFS is one directory's files, read out of a commit.
type treeFS struct {
	dir   string
	files map[string]treeFile
}

type treeFile struct {
	data []byte
	err  error
}

var _ fs.ReadDirFS = (*treeFS)(nil)

func (t *treeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f, ok := t.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if f.err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	}
	return &treeHandle{name: path.Base(name), Reader: bytes.NewReader(f.data), size: int64(len(f.data))}, nil
}

func (t *treeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != t.dir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, 0, len(t.files))
	for full, f := range t.files {
		out = append(out, fs.FileInfoToDirEntry(treeInfo{name: path.Base(full), size: int64(len(f.data))}))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type treeHandle struct {
	*bytes.Reader
	name string
	size int64
}

func (h *treeHandle) Stat() (fs.FileInfo, error) { return treeInfo{name: h.name, size: h.size}, nil }
func (h *treeHandle) Close() error               { return nil }

var _ io.Reader = (*treeHandle)(nil)

type treeInfo struct {
	name string
	size int64
}

func (i treeInfo) Name() string       { return i.name }
func (i treeInfo) Size() int64        { return i.size }
func (i treeInfo) Mode() fs.FileMode  { return 0o444 }
func (i treeInfo) ModTime() time.Time { return time.Time{} }
func (i treeInfo) IsDir() bool        { return false }
func (i treeInfo) Sys() any           { return nil }
