package docker

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/container"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

var _ executor.TempCopier = (*runEnv)(nil)

// CopyFromTemp copies rel under $RUNNER_TEMP out of the container, through
// the archive API, to the same relative path under dst (A84). It is how an
// upload-artifact step reads runner.temp here: sokie's pack uploads the
// tarball npm wrote there.
//
// What comes back is a tar the daemon built from the container's files, which
// a step wrote. It is read as hostile: every entry is placed under dst through
// an os.Root, so neither a `..` nor a symlink already there can put a file
// anywhere else; only regular files and directories are written; and the
// copy stops at limit bytes.
func (e *runEnv) CopyFromTemp(ctx context.Context, rel, dst string, limit int64) (int64, error) {
	rel, err := tempRel(rel)
	if err != nil {
		return 0, err
	}
	if err := e.usable(); err != nil {
		return 0, err
	}
	rc, _, err := e.api.CopyFromContainer(ctx, e.containerID, path.Join(TempPath, rel))
	if isNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, wrapErr(protocol.ErrArtifact, err, "copy %s out of $RUNNER_TEMP", orDot(rel))
	}
	defer func() { _ = rc.Close() }()
	root, err := os.OpenRoot(dst)
	if err != nil {
		return 0, wrapErr(protocol.ErrArtifact, err, "open %s", dst)
	}
	defer func() { _ = root.Close() }()
	return extractTemp(tar.NewReader(rc), root, rel, limit)
}

// extractTemp writes a tar of rel, as the daemon roots it — the first
// component of each name is rel's own base name — under root at rel.
func extractTemp(tr *tar.Reader, root *os.Root, rel string, limit int64) (int64, error) {
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, wrapErr(protocol.ErrArtifact, err, "read $RUNNER_TEMP's archive")
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if !filepath.IsLocal(name) {
			return total, codeErr(protocol.ErrArtifact, "$RUNNER_TEMP's archive names %q, outside %s", hdr.Name, orDot(rel))
		}
		_, rest, _ := strings.Cut(name, "/")
		target := path.Join(orDot(rel), rest)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(target, 0o750); err != nil {
				return total, wrapErr(protocol.ErrArtifact, err, "create %s", target)
			}
		case tar.TypeReg:
			if hdr.Size > limit-total {
				return total, codeErr(protocol.ErrArtifact,
					"$RUNNER_TEMP/%s is more than %d bytes, which is as much as one upload copies out of a container",
					orDot(rel), limit)
			}
			if err := root.MkdirAll(path.Dir(target), 0o750); err != nil {
				return total, wrapErr(protocol.ErrArtifact, err, "create %s", path.Dir(target))
			}
			n, err := writeEntry(root, target, tr, hdr.Size)
			total += n
			if err != nil {
				return total, wrapErr(protocol.ErrArtifact, err, "write %s", target)
			}
		default:
			// A symlink, a device, a pipe: none of them is a file to upload,
			// and a symlink is how an archive reaches outside where it lands.
		}
	}
}

func writeEntry(root *os.Root, name string, r io.Reader, size int64) (int64, error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// CopyToTemp copies the host directory src's regular files and directories
// into $RUNNER_TEMP, at the same relative paths, through the archive API
// (A84). It is how a download-artifact step writes runner.temp here: the Mac
// releases' publish jobs download their DMG into `${{ runner.temp }}/dmg`.
//
// The files are the step user's when it is a uid, and otherwise anyone's, for
// the reason [runEnv.PutTemp]'s are: the container is this run's alone.
func (e *runEnv) CopyToTemp(ctx context.Context, src string) error {
	if err := e.usable(); err != nil {
		return err
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return wrapErr(protocol.ErrArtifact, err, "open %s", src)
	}
	defer func() { _ = root.Close() }()

	uid, gid, owned := numericUser(e.user)
	dirMode, fileMode := int64(0o777), int64(0o666)
	if owned {
		dirMode, fileMode = 0o755, 0o644
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil || name == "." {
				return err
			}
			hdr := &tar.Header{Name: name, Uid: uid, Gid: gid}
			switch {
			case d.IsDir():
				hdr.Typeflag, hdr.Mode, hdr.Name = tar.TypeDir, dirMode, name+"/"
				return tw.WriteHeader(hdr)
			case d.Type().IsRegular():
				return copyEntry(tw, root, name, hdr, fileMode)
			}
			return nil
		})
		if err == nil {
			err = tw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	err = e.api.CopyToContainer(ctx, e.containerID, TempPath, pr, container.CopyToContainerOptions{})
	_ = pr.CloseWithError(err)
	if err != nil {
		return wrapErr(protocol.ErrArtifact, err, "copy into $RUNNER_TEMP")
	}
	return nil
}

func copyEntry(tw *tar.Writer, root *os.Root, name string, hdr *tar.Header, mode int64) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	hdr.Typeflag, hdr.Mode, hdr.Size, hdr.ModTime = tar.TypeReg, mode, info.Size(), info.ModTime()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, io.LimitReader(f, info.Size()))
	return err
}

// usable refuses a copy into or out of an environment already closed.
func (e *runEnv) usable() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return codeErr(protocol.ErrExecutor, "environment for run %s is closed", e.spec.RunID)
	}
	return nil
}

// tempRel is rel cleaned, and refused when it is not under $RUNNER_TEMP.
func tempRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return "", nil
	}
	if !filepath.IsLocal(rel) || strings.Contains(rel, `\`) {
		return "", codeErr(protocol.ErrArtifact, "%q is not a path under $RUNNER_TEMP", rel)
	}
	return path.Clean(rel), nil
}

func orDot(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}
