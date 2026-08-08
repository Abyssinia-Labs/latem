package local

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// MaxFrameBytes bounds one newline-delimited JSON frame in either direction.
//
// The bound exists because the far end of this socket is a process, not a
// person: a client that opens the socket and writes without ever sending a
// newline would otherwise grow the agent's read buffer until the machine
// notices. 64 KiB is far larger than any legal [protocol.Command] — a
// RunRequest is five short strings and a bounded input map — and small enough
// that a thousand concurrent connections cannot matter.
//
// Exceeding it is unrecoverable rather than skippable. Once a frame is longer
// than the reader will hold, the reader no longer knows where the next frame
// starts, so the connection is answered once and closed instead of being
// resynchronised at a newline that may itself be inside the oversized value.
const MaxFrameBytes = 64 << 10

// reply is the local broker's answer to one command frame.
//
// It is unexported, and that is a statement rather than a convenience.
// RUNNER_SPEC §4 and finding F8 put every type that crosses the wire in
// pkg/protocol, and pkg/protocol has no acknowledgement type — [protocol.Command]
// is a one-way vocabulary. A transport that cannot tell a client "accepted,
// run id r_123" or "refused, that run is already finished" has to either
// invent a frame or go silent, and going silent means `latem run` prints a run
// id for work that was never accepted.
//
// So this is the smallest frame that answers the question the client actually
// asked, kept unexported so that nothing outside this package can mistake it
// for part of the protocol. It is recorded as a contract defect rather than
// smuggled in: see the package comment.
type reply struct {
	OK    bool   `json:"ok"`
	RunID string `json:"run_id,omitempty"`
	Error string `json:"error,omitempty"`
}

// errReply builds a refusal. The message is the agent's own text, never an
// echo of anything the client sent, so a client cannot use the reply channel
// to make the agent repeat bytes it chose.
func errReply(format string, args ...any) reply {
	return reply{Error: fmt.Sprintf(format, args...)}
}

// newFrameReader returns a reader bounded at [MaxFrameBytes].
//
// bufio.Scanner is used rather than a json.Decoder over the raw connection
// because the decoder would happily stream a single unbounded value, and
// because the framing is newline-delimited by specification (§7.3): the frame
// boundary is a property of the transport, not something the JSON parser gets
// to decide.
func newFrameReader(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), MaxFrameBytes)
	return sc
}

// writeFrame encodes v as one line.
//
// json.Encoder appends the newline itself, and the whole frame reaches the
// connection in one Write, so a reader on the far side never sees a partial
// line from a well-behaved writer.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("local: encoding frame: %w", err)
	}
	if len(b)+1 > MaxFrameBytes {
		return fmt.Errorf("local: frame is %d bytes, limit %d", len(b)+1, MaxFrameBytes)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("local: writing frame: %w", err)
	}
	return nil
}

// decodeCommand decodes one frame.
//
// Every rule that matters here already lives in [protocol.Command.UnmarshalJSON]:
// unknown fields are refused at every level, the kind set is closed at run and
// cancel (INV-2), the payload must match the kind, and the id is checked
// against [protocol.ValidateID] so a traversal id is rejected before anything
// downstream turns it into a path (INV-3, amendment A6). Re-validating any of
// that here would create a second, drifting copy of the trust boundary, so
// this function is only the frame-to-value step.
func decodeCommand(line []byte) (protocol.Command, error) {
	var cmd protocol.Command
	if err := json.Unmarshal(line, &cmd); err != nil {
		return protocol.Command{}, err
	}
	return cmd, nil
}

// decodeReply decodes the broker's answer, strictly.
//
// Strict on this side too, for a reason that has nothing to do with hostile
// input: both ends of this socket are the same binary, so an unknown field in
// a reply means the client is talking to an agent built from different source.
// That is worth a loud failure — the alternative is a `latem` that silently
// misreads a newer agent's answers.
func decodeReply(line []byte, r *reply) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(r); err != nil {
		return fmt.Errorf("local: decoding the agent's reply: %w", err)
	}
	return nil
}
