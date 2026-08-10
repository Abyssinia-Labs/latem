package remote

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// MaxFrameBytes bounds one WebSocket message in either direction.
//
// A megabyte is far larger than any frame this transport sends — the biggest is
// a log batch, 100 frames of at most a 64 KB line each, and the emitter batches
// well below that in practice — and small enough that a hostile or broken
// control plane cannot make the agent allocate its way out of memory. It is
// applied with Conn.SetReadLimit, so an oversized message fails the read rather
// than being assembled first.
const MaxFrameBytes = 1 << 20

// frameKind names one message type. Unlike [protocol.CommandKind] this set is
// not a trust boundary — it is a transport detail — but it is closed all the
// same, and an unknown kind is refused rather than ignored.
type frameKind string

const (
	// Server → agent.
	kindHello   frameKind = "hello"
	kindCommand frameKind = "command"

	// Agent → server.
	kindResume    frameKind = "resume"
	kindUpdate    frameKind = "update"
	kindLogs      frameKind = "logs"
	kindHeartbeat frameKind = "heartbeat"
	kindClaim     frameKind = "claim"

	// Either direction: a reply, never a verb.
	kindAck frameKind = "ack"
)

// frame is one WebSocket message.
//
// It is unexported, and that is a statement rather than a convenience —
// the same statement internal/broker/local made with its `reply` type before
// amendment A18. RUNNER_SPEC §4 and finding F8 put every type that crosses the
// wire in pkg/protocol, and pkg/protocol has no envelope: it has the payloads
// ([protocol.Command], [protocol.Update], [protocol.LogFrame],
// [protocol.MachineHealth], [protocol.Resume], [protocol.Ack]) and no way to
// say which one a given message is, or which request a given [protocol.Ack]
// answers.
//
// A Unix socket carrying one kind of message each way does not need an
// envelope, which is why the local broker never grew one. A WebSocket carrying
// six does. So this exists, unexported so nothing outside can mistake it for
// protocol, and is recorded as a contract defect in the package comment rather
// than smuggled in.
//
// Exactly one payload field is populated, and it is the one matching Kind.
type frame struct {
	Kind frameKind `json:"kind"`

	// ID correlates a request with its [protocol.Ack]. It is on the envelope
	// because A18's Ack carries only `{ok, run_id, code, error}` — enough to
	// answer "did that work", not enough to answer "which of the three things I
	// have outstanding for this run does this refer to". A log batch and a
	// report for one run can both be in flight at once.
	ID string `json:"id,omitempty"`

	// Version is [protocol.Version]. An agent and a control plane disagreeing on
	// the major component cannot usefully talk, and saying so in the frame makes
	// that diagnosable rather than mysterious.
	Version string `json:"protocol_version,omitempty"`

	Hello     *helloFrame             `json:"hello,omitempty"`
	Command   *protocol.Command       `json:"command,omitempty"`
	Ack       *protocol.Ack           `json:"ack,omitempty"`
	Resume    *protocol.Resume        `json:"resume,omitempty"`
	Update    *updateFrame            `json:"update,omitempty"`
	Logs      []protocol.LogFrame     `json:"logs,omitempty"`
	Heartbeat *protocol.MachineHealth `json:"heartbeat,omitempty"`
	Claim     *claimRequest           `json:"claim,omitempty"`
}

// helloFrame is the control plane's first message. It is how a machine learns
// the identifier that was assigned to it, which `latem register` then writes
// into `[machine] id`.
//
// pkg/protocol has no type for this. [protocol.MachineHealth] carries a
// MachineID the agent already knows; nothing carries one the server chose.
type helloFrame struct {
	MachineID string `json:"machine_id,omitempty"`
	// Version is the control plane's [protocol.Version].
	Version string `json:"protocol_version,omitempty"`
	// Message is display text for the operator, already the server's own words.
	Message string `json:"message,omitempty"`
}

// updateFrame pairs a transition with the run it belongs to.
//
// [protocol.Update] has no run id — [broker.Broker.Report] takes it as a
// separate argument — while [protocol.LogFrame] does have one. A transport
// putting an update on a wire therefore has to pair them itself.
type updateFrame struct {
	RunID  string          `json:"run_id"`
	Update protocol.Update `json:"update"`
}

// claimRequest asks for exclusive ownership of a run. It carries an identifier
// and nothing else: the control plane already knows everything about the run,
// and a claim that could assert anything would be an inbound verb pointed
// backwards.
type claimRequest struct {
	RunID string `json:"run_id"`
}

// encode marshals a frame, stamping the protocol version.
func encode(f frame) ([]byte, error) {
	f.Version = protocol.Version
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("remote: encoding %s frame: %w", f.Kind, err)
	}
	if len(b) > MaxFrameBytes {
		return nil, fmt.Errorf("remote: %s frame is %d bytes, limit %d", f.Kind, len(b), MaxFrameBytes)
	}
	return b, nil
}

// decode reads one inbound message, strictly.
//
// Unknown fields are refused at every level. That costs forward compatibility —
// a control plane adding a field breaks older agents loudly — and it is the
// same trade [protocol.Command.UnmarshalJSON] makes deliberately at the same
// boundary: a loud refusal beats a quiet misinterpretation. The command payload
// re-enters that decoder on the way through, so the closed two-kind set, the
// payload/kind agreement, and [protocol.ValidateID] are all enforced by the
// frozen code that owns them rather than by a second copy here.
func decode(data []byte) (frame, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var f frame
	if err := dec.Decode(&f); err != nil {
		return frame{}, fmt.Errorf("remote: decoding frame: %w", err)
	}
	if err := f.validate(); err != nil {
		return frame{}, err
	}
	return f, nil
}

// validate checks that the payload matches the kind.
//
// Only the two kinds a control plane may send are accepted. The rest are this
// agent's own outbound vocabulary, and a server echoing one back — a heartbeat,
// a resume — is either confused or probing, and neither deserves an answer.
func (f frame) validate() error {
	switch f.Kind {
	case kindCommand:
		if f.Command == nil {
			return fmt.Errorf("remote: %s frame with no command payload", f.Kind)
		}
		return nil
	case kindAck:
		if f.Ack == nil {
			return fmt.Errorf("remote: %s frame with no ack payload", f.Kind)
		}
		return nil
	case kindHello:
		if f.Hello == nil {
			return fmt.Errorf("remote: %s frame with no hello payload", f.Kind)
		}
		return nil
	case "":
		return fmt.Errorf("remote: frame has no kind")
	default:
		return fmt.Errorf("remote: a control plane may not send a %q frame", f.Kind)
	}
}
