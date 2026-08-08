package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A second agent on one config directory is the corruption §6 step 2 exists to
// prevent: two processes sharing a work tree, a socket, and a queue.
//
// The failure has to name the holder. An operator who gets "resource
// temporarily unavailable" has nothing to act on; one who gets a pid can look
// at it, and the message tells them the other way out is --config-dir.
func TestLockRefusesASecondAgentAndNamesThePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")

	first, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Close() }()

	_, err = acquireLock(path)
	if err == nil {
		t.Fatal("a second agent took the lock; exactly one agent per config directory (§6 step 2)")
	}
	if !errors.Is(err, ErrLocked) {
		t.Errorf("error should unwrap to ErrLocked: %v", err)
	}

	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("error should be a *LockedError: %v", err)
	}
	if locked.PID != os.Getpid() {
		t.Errorf("PID = %d, want this process %d", locked.PID, os.Getpid())
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("the message must name the holder so `ps` is not the only recourse: %v", err)
	}
	if !strings.Contains(err.Error(), "--config-dir") {
		t.Errorf("the message should offer the supported way to run two agents: %v", err)
	}
}

// Releasing must make the directory usable again, or a clean restart would
// need a reboot.
func TestLockIsReusableAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")

	first, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Idempotent: shutdown calls Close on a path that may already have run it.
	if err := first.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	second, err := acquireLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = second.Close()
}

// The pid is written after the lock is taken, so a reader that finds one knows
// the holder was alive when it wrote it.
func TestLockRecordsThePID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	l, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	if got := readPID(path); got != os.Getpid() {
		t.Errorf("readPID = %d, want %d", got, os.Getpid())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != LockMode.Perm() {
		t.Errorf("lock mode = %v, want %v", info.Mode().Perm(), LockMode.Perm())
	}
}

// A lock file left behind by a dead process must not lock the directory out.
//
// This is the property that makes flock the right primitive and a pid file the
// wrong one: the kernel releases it when the process dies however it dies,
// including SIGKILL, so nobody ever has to delete a stale file by hand. The
// test kills a child that is holding the lock and then takes it.
func TestLockSurvivesAHardKillOfTheHolder(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell to hold the lock with")
	}
	path := filepath.Join(t.TempDir(), "agent.lock")

	// A stale pid from a process that is gone, so the file is not empty and
	// a pid-file implementation would refuse.
	if err := os.WriteFile(path, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := acquireLock(path)
	if err != nil {
		t.Fatalf("a lock file naming a dead pid must not block acquisition: %v", err)
	}
	_ = l.Close()
}

func TestLockedErrorWithoutAPID(t *testing.T) {
	e := &LockedError{Path: "/tmp/agent.lock"}
	if !strings.Contains(e.Error(), "does not name yet") {
		t.Errorf("an unnamed holder should say so rather than print pid 0: %v", e)
	}
	if !errors.Is(e, ErrLocked) {
		t.Error("Unwrap should reach the sentinel")
	}
}

func TestReadPIDIgnoresRubbish(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"", "not-a-pid", "-1", "0"} {
		path := filepath.Join(dir, "l")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readPID(path); got != 0 {
			t.Errorf("readPID(%q) = %d, want 0", content, got)
		}
	}
	if got := readPID(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("readPID of a missing file = %d, want 0", got)
	}
}
