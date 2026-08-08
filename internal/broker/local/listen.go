package local

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Modes for the socket and the directory holding it (RUNNER_SPEC §7.3, §19).
//
// Both matter, and the directory matters more. A Unix socket's own mode is
// checked by the kernel on connect on Linux and macOS, but the portable and
// unarguable guard is that ~/.latem is 0700: another user cannot reach the
// path at all, whatever the socket's own bits say.
const (
	DirMode    os.FileMode = 0o700
	SocketMode os.FileMode = 0o600
)

// ErrAgentRunning means the socket already has a live listener.
//
// It is distinct from a bind failure because the operator's next step is
// different: this one says "you already have an agent", and the answer is to
// stop it rather than to remove a file.
var ErrAgentRunning = errors.New("local: an agent is already listening on this socket")

// probeTimeout bounds the "is anybody there?" connect against an existing
// socket. It is short because both ends are on this machine: a live agent
// accepts immediately, and a dead one's socket file refuses immediately. The
// timeout only covers the pathological case of a listener whose backlog is
// full, which is a live agent by any useful definition.
const probeTimeout = 250 * time.Millisecond

// listen binds the Unix socket, clearing a stale socket file if one is in the
// way.
//
// # Why unlinking needs a probe first
//
// A socket file outlives the process that bound it. An agent killed with
// SIGKILL, or a machine that lost power mid-run, leaves ~/.latem/agent.sock on
// disk, and bind(2) refuses to reuse an existing path — so an agent that only
// tried to bind would never start again after a crash, which is the least
// acceptable failure mode a daemon can have.
//
// The fix is to unlink and rebind, and the fix is dangerous. Unlinking
// unconditionally means a second agent started by mistake silently steals the
// socket from the first: the running agent keeps its listener and keeps
// accepting nothing, because the path clients dial now resolves to a different
// inode. Every `latem run` goes to the new agent, the old one still holds the
// runs it is executing, and nothing anywhere reports a problem.
//
// So the file is probed before it is removed. A successful connect means a
// live agent and is refused with [ErrAgentRunning]; a refused connect means
// nothing is listening on that inode and the file is debris.
//
// This is a heuristic and is documented as one. The actual guarantee of one
// agent per config directory is the flock at RUNNER_SPEC §6 step 2, taken
// before this is ever called. The probe is what makes the crash case recover
// cleanly and what stops an operator who bypassed the lock from losing a
// build in silence.
func listen(path string) (*net.UnixListener, error) {
	if path == "" {
		return nil, errors.New("local: socket path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, fmt.Errorf("local: creating %s: %w", filepath.Dir(path), err)
	}

	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("local: binding %s: %w", path, err)
	}

	// The socket is created with the process umask applied to 0777, so it is
	// tightened immediately after bind rather than before — there is no bind
	// mode argument to pass. The window between the two calls is real and is
	// covered by the 0700 on the parent directory, which no other user can
	// traverse. Changing the process umask instead would be worse: it is
	// global to the process and would race every other goroutine that creates
	// a file.
	if err := os.Chmod(path, SocketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("local: securing %s: %w", path, err)
	}
	return ln, nil
}

// clearStaleSocket removes a socket file left by a dead agent, and refuses to
// touch anything else.
func clearStaleSocket(path string) error {
	// Lstat, not Stat: a symlink at the socket path is not something to follow
	// and then unlink the target of.
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("local: inspecting %s: %w", path, err)
	}

	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf(
			"local: %s exists and is a %s, not a socket; refusing to delete it — move it aside by hand",
			path, describeMode(fi.Mode()))
	}

	if live, err := probe(path); err != nil {
		return err
	} else if live {
		return fmt.Errorf("%w: %s", ErrAgentRunning, path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("local: removing the stale socket %s: %w", path, err)
	}
	return nil
}

// probe reports whether something is accepting connections at path.
//
// A connect that is refused, or that finds nothing there, is the answer this
// function exists to give and is not an error. Any other failure is: it means
// the question could not be asked, and unlinking on the strength of an
// unanswered question is exactly the mistake the probe is here to prevent.
func probe(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, probeTimeout)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) || isRefused(err) {
		return false, nil
	}
	return false, fmt.Errorf(
		"local: cannot tell whether an agent holds %s; refusing to unlink it: %w", path, err)
}

// isRefused reports whether err is the "nothing is listening here" answer.
//
// It is written against the syscall error rather than against a string because
// the message differs between platforms, and a string match that silently
// stopped matching would turn "an agent is running" into "unlink it".
func isRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// describeMode names a file type for an error message, because "0x8000" is not
// something an operator can act on.
func describeMode(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m&os.ModeDir != 0:
		return "directory"
	case m.IsRegular():
		return "regular file"
	default:
		return "special file"
	}
}
