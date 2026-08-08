package source

import (
	"os"
	"path/filepath"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// mkdirPrivate creates a directory tree at 0700.
//
// 0700 rather than 0755 because ~/.latem holds a machine token and secrets.env
// beside these directories, and a world-readable config tree is the whole
// threat model failing quietly (RUNNER_SPEC §8).
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wrapErr(protocol.ErrSource, err, "creating %s", dir)
	}
	return nil
}

// CheckSameFilesystem reports whether `git clone --local` can hardlink from
// mirrorDir into workDir.
//
// It answers the question by trying it: create a file in mirrorDir, link it
// into workDir, delete both. That is a stronger test than comparing device
// numbers, which is the usual implementation of "same filesystem". Device
// numbers say nothing about a filesystem mounted with links disabled, an
// overlay upper layer, or an SMB share that reports one device and refuses
// link(2) — and each of those produces exactly the failure this check exists
// to prevent, which is git silently copying the entire object store instead of
// linking it and every run on the machine getting slower for reasons nobody
// attributes correctly.
//
// Exported because `latem doctor` reports it (RUNNER_SPEC §12) and because a
// caller that has not yet built a [Manager] still wants the answer.
func CheckSameFilesystem(mirrorDir, workDir string) error {
	probe, err := os.CreateTemp(mirrorDir, ".latem-linkcheck-*")
	if err != nil {
		return wrapErr(protocol.ErrSource, err, "probing %s for hardlink support", mirrorDir)
	}
	src := probe.Name()
	// The probe never gets written to; only its inode matters.
	_ = probe.Close()
	defer func() { _ = os.Remove(src) }()

	dst := filepath.Join(workDir, filepath.Base(src))
	if err := os.Link(src, dst); err != nil {
		return wrapErr(protocol.ErrSource, err,
			"mirror dir %s and work dir %s are not on one filesystem, so "+
				"`git clone --local` would copy every object instead of hardlinking it",
			mirrorDir, workDir)
	}
	if err := os.Remove(dst); err != nil {
		return wrapErr(protocol.ErrSource, err, "removing hardlink probe %s", dst)
	}
	return nil
}
