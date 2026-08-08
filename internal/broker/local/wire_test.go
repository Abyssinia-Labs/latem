package local

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The limit binds in both directions. An agent that refuses an oversized frame
// on the way in and then emits one on the way out has moved the problem to the
// client rather than solved it.
func TestWriteFrame_RefusesAnOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	err := writeFrame(&buf, reply{Error: strings.Repeat("x", MaxFrameBytes)})
	if err == nil {
		t.Fatal("writeFrame emitted a frame past the limit")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes of a frame it refused", buf.Len())
	}
}

// A value JSON cannot encode must fail before anything reaches the connection,
// or the far side gets half a frame and loses the stream.
func TestWriteFrame_RefusesAnUnencodableValue(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, math.Inf(1)); err == nil {
		t.Fatal("writeFrame accepted a value JSON cannot represent")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes before failing", buf.Len())
	}
}

// One frame per line, terminated, so a reader on the far side never has to
// guess where a value ends.
func TestWriteFrame_IsOneTerminatedLine(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, reply{OK: true, RunID: "r_1"}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	out := buf.Bytes()
	if out[len(out)-1] != '\n' {
		t.Error("the frame is not newline-terminated")
	}
	if bytes.Count(out, []byte("\n")) != 1 {
		t.Errorf("the frame contains %d newlines, want 1", bytes.Count(out, []byte("\n")))
	}
}

// The reason is display-only text from outside the trust boundary. The bound
// is enforced on decode by pkg/protocol, and this pins that the transport does
// not route around it.
func TestDecodeCommand_TruncatesAnOverlongCancelReason(t *testing.T) {
	long := strings.Repeat("é", protocol.MaxCancelReasonRunes*3)
	frame := mustJSON(t, protocol.Command{
		Kind:   protocol.CommandCancel,
		Cancel: &protocol.CancelRequest{ID: "r_1", Reason: long},
	})

	cmd, err := decodeCommand(frame)
	if err != nil {
		t.Fatalf("decodeCommand: %v", err)
	}
	if n := len([]rune(cmd.Cancel.Reason)); n != protocol.MaxCancelReasonRunes {
		t.Errorf("reason is %d runes, want it capped at %d", n, protocol.MaxCancelReasonRunes)
	}
}

// A directory where the socket belongs is somebody's mistake, not debris. The
// refusal has to name what it found, or the operator is left guessing.
func TestListen_RefusesADirectoryAtTheSocketPath(t *testing.T) {
	path := socketPath(t)
	if err := os.Mkdir(path, DirMode); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	_, err := listen(path)
	if err == nil {
		t.Fatal("listen over a directory succeeded")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %q, want it to say what is in the way", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the directory was removed: %v", err)
	}
}

// Close has to unblock a client that connected and then went quiet, or a
// graceful shutdown waits out the full read timeout for every idle connection.
func TestClose_UnblocksAnIdleConnection(t *testing.T) {
	db := newStore(t)
	b, err := Open(Options{Socket: socketPath(t), Store: db, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	conn := dial(t, b.Socket())
	// Give the accept loop a moment to be holding this connection.
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- b.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on an idle connection")
	}

	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the connection was still readable after Close")
	}
}
