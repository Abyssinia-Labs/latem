package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// LockMode is the mode of ~/.latem/agent.lock. The file holds a pid and
// nothing else, but it lives in a directory that is 0700 and there is no
// reason for it to be looser than its neighbours.
const LockMode os.FileMode = 0o600

// ErrLocked means another agent already holds the config directory.
//
// It is a sentinel because the caller's answer differs from every other
// startup failure: a second `latem agent` is a mistake to be named, not a
// machine fault to be fixed, and the message has to carry the pid or the
// operator's only recourse is `ps`.
var ErrLocked = errors.New("agent: another agent holds this config directory")

// LockedError reports which process holds the lock.
type LockedError struct {
	// Path is the lock file.
	Path string
	// PID is the holder, or 0 when the file held nothing readable — which
	// happens legitimately in the window between a competitor taking the flock
	// and writing its pid.
	PID int
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("%s: pid %d holds %s — stop it first (`kill %d`), "+
			"or run this agent against a different directory with --config-dir",
			ErrLocked, e.PID, e.Path, e.PID)
	}
	return fmt.Sprintf("%s: %s is held by a running agent whose pid the file does not name yet — "+
		"retry in a moment, or use --config-dir for a second agent", ErrLocked, e.Path)
}

func (e *LockedError) Unwrap() error { return ErrLocked }

// lockFile is a held advisory lock on the config directory.
type lockFile struct {
	path string
	f    *os.File
}

// acquireLock takes the exclusive advisory lock at path (RUNNER_SPEC §6 step
// 2) and records this process's pid in it.
//
// The lock is advisory and the pid is a courtesy. Neither is what makes the
// guarantee: flock is held by the kernel against the open file description, so
// it is released when the process dies however it dies — including SIGKILL,
// which is the case that matters here. A pid file alone would survive the
// process and lock the directory out until someone deleted it by hand, which
// is the failure mode this deliberately does not have.
func acquireLock(path string) (*lockFile, error) {
	// #nosec G304 -- path is <config-dir>/agent.lock, derived by config.Paths
	// from --config-dir, LATEM_CONFIG_DIR, or $HOME. It crosses no trust
	// boundary.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, LockMode)
	if err != nil {
		return nil, fmt.Errorf("agent: opening %s: %w", path, err)
	}

	if err := lockExclusive(f); err != nil {
		pid := readPID(path)
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, &LockedError{Path: path, PID: pid}
		}
		return nil, fmt.Errorf("agent: locking %s: %w", path, err)
	}

	// The pid is written after the lock is held, so a reader that finds a pid
	// knows the holder was alive when it wrote it. Truncating first keeps a
	// shorter pid from leaving the tail of a longer one behind.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		_ = f.Sync()
	}
	return &lockFile{path: path, f: f}, nil
}

// Close releases the lock. §6 says the lock is released last, so this is the
// final teardown step and not a deferred afterthought.
func (l *lockFile) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// Holder reports whether an agent holds the lock at path, and its pid when the
// file names one.
//
// It asks the kernel rather than the pid: a shared lock is refused exactly
// while an agent holds the exclusive one, and is released at once when it is
// granted. A pid alone can outlive its process and come to name another one.
//
// Taking the shared lock, however briefly, can make an agent starting in that
// same instant fail as though it had a competitor. A service manager restarts
// it; the window is a person typing a command.
func Holder(path string) (pid int, held bool, err error) {
	// #nosec G304 -- see acquireLock.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("agent: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := lockShared(f); err != nil {
		if errors.Is(err, errWouldBlock) {
			return readPID(path), true, nil
		}
		return 0, false, fmt.Errorf("agent: probing %s: %w", path, err)
	}
	_ = unlock(f)
	return 0, false, nil
}

// readPID reads the holder's pid, best effort. A lock file with no readable
// pid is not an error: the holder may not have written it yet.
func readPID(path string) int {
	// #nosec G304 -- see acquireLock.
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
