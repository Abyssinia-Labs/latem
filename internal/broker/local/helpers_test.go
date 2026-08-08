package local

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Every test here talks over a real Unix socket to a real sqlite database.
// Nothing about this transport is mocked, because everything interesting about
// it — the file mode, the stale socket, the framing, the claim race — is a
// property of the kernel and of sqlite rather than of this package's control
// flow, and a fake would assert only that the code calls the functions it
// calls.

// socketPath returns a socket path short enough for the kernel.
//
// t.TempDir is deliberately not used. On macOS it sits under /var/folders/…
// and embeds the test's own name, so a descriptive test name pushes the path
// past the 104-byte sun_path limit and bind fails with a bare "invalid
// argument" — the exact failure config.maxSocketPathLen exists to catch, and
// not one anybody enjoys diagnosing from a test.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "latem")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := os.Chmod(dir, DirMode); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	path := filepath.Join(dir, "agent.sock")
	if len(path) > 103 {
		t.Fatalf("socket path %s is %d bytes; the kernel caps sun_path at 104", path, len(path))
	}
	return path
}

func newStore(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"), logstream.NewRedactor())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// newBroker returns an open broker and its store.
func newBroker(t *testing.T, opts ...func(*Options)) (*Broker, *store.DB) {
	t.Helper()

	db := newStore(t)
	o := Options{Socket: socketPath(t), Store: db}
	for _, fn := range opts {
		fn(&o)
	}

	b, err := Open(o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, db
}

func newClient(t *testing.T, socket string, st Store) *Client {
	t.Helper()

	c, err := NewClient(ClientOptions{Socket: socket, Store: st, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// runRequest is a minimally valid request. Tests override what they are about.
func runRequest(id string) protocol.RunRequest {
	return protocol.RunRequest{
		ID:     id,
		Repo:   "myapp",
		Job:    "build",
		Ref:    "main",
		Origin: protocol.Origin{Kind: "cli", Actor: "someone"},
	}
}

// seedQueued puts a queued run in the store without going through the socket,
// for the tests that are about what happens after a run exists.
func seedQueued(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := enqueueRun(t.Context(), db, runRequest(id), 0, time.Time{}); err != nil {
		t.Fatalf("enqueueRun %s: %v", id, err)
	}
}

// mustJSON encodes a value as one frame's worth of bytes.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// dial opens a raw connection, for the tests that send bytes no client would.
func dial(t *testing.T, socket string) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", socket, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	return conn
}

// sendRaw writes exactly the bytes given, plus a newline, and reads one reply.
func sendRaw(t *testing.T, conn net.Conn, frame []byte) reply {
	t.Helper()

	if _, err := conn.Write(append(frame, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := newFrameReader(conn)
	if !sc.Scan() {
		t.Fatalf("no reply frame: %v", sc.Err())
	}
	var r reply
	if err := decodeReply(sc.Bytes(), &r); err != nil {
		t.Fatalf("decode reply %q: %v", sc.Bytes(), err)
	}
	return r
}

// consume starts the agent side of the broker and returns a function that
// waits for the next command.
func consume(t *testing.T, b *Broker) func(time.Duration) (protocol.Command, bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	ch, err := b.Commands(ctx)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	return func(d time.Duration) (protocol.Command, bool) {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case cmd, ok := <-ch:
			return cmd, ok
		case <-timer.C:
			return protocol.Command{}, false
		}
	}
}
