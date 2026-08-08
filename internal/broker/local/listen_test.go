package local

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A socket anyone else on the machine can open is INV-1 with the front door
// left unlocked: the invariant is about who can reach the agent, and a 0666
// socket in a shared home directory reaches further than any TCP port would.
func TestListen_SocketModeIs0600(t *testing.T) {
	b, _ := newBroker(t)

	fi, err := os.Stat(b.Socket())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != SocketMode {
		t.Errorf("socket mode is %04o, want %04o", got, SocketMode)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket: %v", b.Socket(), fi.Mode())
	}
}

// The directory is the guard that does not depend on the platform honouring a
// socket's own permission bits, so it is asserted separately.
func TestListen_CreatesTheDirectoryPrivate(t *testing.T) {
	base := socketPath(t)
	nested := filepath.Join(filepath.Dir(base), "sub", "agent.sock")

	ln, err := listen(nested)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	fi, err := os.Stat(filepath.Dir(nested))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != DirMode {
		t.Errorf("directory mode is %04o, want %04o", got, DirMode)
	}
}

// An agent killed with SIGKILL leaves its socket file behind. If that stopped
// the next start, a crash would be unrecoverable without a manual `rm` — the
// least acceptable failure mode a daemon can have.
func TestListen_StaleSocketDoesNotBlockStartup(t *testing.T) {
	path := socketPath(t)

	// Bind and abandon without unlinking, which is what a SIGKILL leaves.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket should still exist: %v", err)
	}

	second, err := listen(path)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer func() { _ = second.Close() }()

	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("the rebound socket does not accept: %v", err)
	}
	_ = conn.Close()
}

// The dangerous half of unlink-and-rebind. Stomping a live agent's socket
// steals every subsequent `latem run` from a process that is still executing
// builds, and nothing anywhere reports it — the old agent keeps a listener
// nobody can reach.
func TestListen_RefusesToStompALiveAgent(t *testing.T) {
	b, _ := newBroker(t)

	_, err := listen(b.Socket())
	if !errors.Is(err, ErrAgentRunning) {
		t.Fatalf("listen over a live agent = %v, want ErrAgentRunning", err)
	}
}

// A regular file at the socket path is somebody's data, not debris. Deleting
// it because it is in the way is a bug that only shows up once.
func TestListen_RefusesToDeleteANonSocket(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := listen(path); err == nil {
		t.Fatal("listen over a regular file succeeded; it must refuse")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file was deleted: %v", err)
	}
}

func TestListen_RejectsAnEmptyPath(t *testing.T) {
	if _, err := listen(""); err == nil {
		t.Fatal("listen(\"\") succeeded")
	}
}

// A clean shutdown must leave nothing for the next start to probe, or every
// restart pays for a dial timeout it did not need.
func TestClose_UnlinksTheSocket(t *testing.T) {
	db := newStore(t)
	path := socketPath(t)

	b, err := Open(Options{Socket: path, Store: db})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket survived Close: %v", err)
	}

	// And Close is idempotent, because shutdown paths close things twice.
	if err := b.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestOpen_RequiresAStore(t *testing.T) {
	if _, err := Open(Options{Socket: socketPath(t)}); err == nil {
		t.Fatal("Open with no store succeeded; the queue is where a run becomes durable")
	}
}
