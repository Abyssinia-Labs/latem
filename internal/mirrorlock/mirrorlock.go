// Package mirrorlock is the file lock beside each bare mirror under
// ~/.latem/mirrors.
//
// A mirror has writers that change which files its object store holds —
// `git fetch`, `git lfs fetch`, and `git gc`, which deletes the packs it
// repacks — and readers that take those files as files: `git clone --local`
// walks objects/ and hard-links what it finds. A clone that lists a pack a
// gc then deletes, or that links a pack's .pack and not the .idx a repack
// replaced, ends with a workspace missing objects its own commit needs, and
// `git checkout` says "unable to read tree" (A112).
//
// The writers are in more than one process. The agent fetches and runs the
// daily gc; `latem cache prune` runs the same gc from a shell; `latem try`
// adds a commit (A76). An in-process mutex cannot see any of them across that
// line, so the lock is a flock(2) on a file beside the mirror: exclusive for a
// write, shared for a clone. Shared holders do not exclude each other, so runs
// of one repository still clone in parallel.
//
// flock is held by an open file description, not by a process, so two holds
// taken through two opens in one process exclude each other as two processes'
// would. The lock goes when the file is closed, which a crash does too: a
// killed agent cannot leave a mirror locked.
package mirrorlock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// poll is how often a wait retries. flock cannot be interrupted by a
// context, so a wait is a non-blocking try on a timer.
const poll = 25 * time.Millisecond

// Path is the lock file for the mirror at mirror: a dot-name beside it, so a
// listing of the mirrors directory that skips dot-names skips it, and
// nothing inside the repository, where git owns every name.
func Path(mirror string) string {
	mirror = filepath.Clean(mirror)
	return filepath.Join(filepath.Dir(mirror), "."+filepath.Base(mirror)+".lock")
}

// Lock waits, as long as ctx allows, for mirror's lock: exclusive for a
// write, shared for a read. It returns the release.
func Lock(ctx context.Context, mirror string, exclusive bool) (func(), error) {
	for {
		release, ok, err := TryLock(mirror, exclusive)
		if err != nil || ok {
			return release, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the lock on %s: %w", mirror, context.Cause(ctx))
		case <-time.After(poll):
		}
	}
}

// TryLock takes mirror's lock if nothing holds it the other way, and reports
// whether it did. The lock file is made if it is not there.
func TryLock(mirror string, exclusive bool) (release func(), ok bool, err error) {
	path := Path(mirror)
	// #nosec G304 -- path is a literal dot-name beside a mirror the caller
	// built from a validated repo key, or read from the mirrors directory's
	// own listing.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("opening the lock on %s: %w", mirror, err)
	}
	got, err := tryFlock(f, exclusive)
	if err != nil || !got {
		_ = f.Close()
		if err != nil {
			return nil, false, fmt.Errorf("locking %s: %w", mirror, err)
		}
		return nil, false, nil
	}
	return func() { _ = f.Close() }, true, nil
}
