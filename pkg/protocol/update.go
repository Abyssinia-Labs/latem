package protocol

import "time"

// Update publishes a state transition. It is idempotent by (run id, state):
// a broker receiving the same transition twice must treat the second as a
// no-op, because the agent replays unacknowledged transitions on reconnect.
//
// Runner, ExpiresAt, and CancelReason were added in amendment A8. Draft 02's
// Update could not express any of them, which left three columns of the §22
// schema unreachable through the [store.Store] interface that owns it: the
// deadline set on entering a waiting state, the reason a run was stopped, and
// which runner claimed the work. Each also belongs on the wire — a control
// plane showing a run wants all three.
type Update struct {
	State      RunState      `json:"state"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	ErrorCode  ErrorCode     `json:"error_code,omitempty"`
	ErrorMsg   string        `json:"error_message,omitempty"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"`

	// Runner names the execution slot that claimed this run. It is not known
	// at Create time — gate 13 selects it during preparing, after the claim.
	Runner string `json:"runner,omitempty"`

	// ExpiresAt is the approval_ttl or queue_ttl deadline (§11). Set on
	// entering queued or awaiting_approval; a transition out of a waiting
	// state clears it. Only the caller knows the configured TTLs, which is
	// why this travels with the transition rather than being derived.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`

	// CancelReason echoes [CancelRequest.Reason] for audit and display. It
	// originated outside the trust boundary, so it is redacted and truncated
	// before it reaches here, exactly like ErrorMsg.
	CancelReason string `json:"cancel_reason,omitempty"`

	// RefNormalized is the fully-qualified ref that policy was matched against
	// (INV-5), and SHA is the commit it resolved to. Both are decided at gates
	// 7 and 9, long after the run row is created, and §22 has columns for
	// exactly this pair — but nothing could write them, so the audit trail
	// INV-5 exists to leave was permanently NULL (amendment A21).
	//
	// They belong on the wire as well as in the database: "requested as main,
	// built refs/heads/main at 3bf1261" is the answer to the only question
	// anyone asks about a surprising build.
	RefNormalized string `json:"ref_normalized,omitempty"`
	SHA           string `json:"sha,omitempty"`
}

// Ack is a transport's reply to an inbound [Command].
//
// Draft 02 made Command a one-way vocabulary, so every transport had to invent
// its own way of saying "accepted, run id r_123" or "refused, that run already
// finished". The local broker did; the WebSocket broker would have invented a
// different one, and the two would have disagreed about a case neither author
// had thought about (amendment A18).
//
// This is a reply, not a command: it carries no verb and cannot cause work.
type Ack struct {
	OK    bool   `json:"ok"`
	RunID string `json:"run_id,omitempty"`
	// Code is set when OK is false, and is one of the §21 codes.
	Code ErrorCode `json:"code,omitempty"`
	// Error is human-facing, already redacted.
	Error string `json:"error,omitempty"`
}

// ArtifactRef describes an artifact without carrying it.
//
// Content never crosses the boundary (RUNNER_SPEC §17). An .ipa is 50–300 MB
// and belongs on the machine that signed it, or in TestFlight — not relayed
// through a control plane that has no business holding it.
type ArtifactRef struct {
	Path      string     `json:"path"`
	SizeBytes int64      `json:"size_bytes"`
	SHA256    string     `json:"sha256"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Stream identifies where a log line came from.
type Stream string

const (
	// StreamStdout and StreamStderr are a step's own output.
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	// StreamAgent is the agent narrating: step boundaries, teardown, and the
	// markers recording that log frames were dropped while disconnected.
	StreamAgent Stream = "agent"
)

// LogFrame is the unit of log transport.
//
// Seq is per-run and monotonic from 1. It is assigned at the point of
// emission, after redaction (§16) and before fan-out, which gives two
// properties worth stating plainly:
//
// A LogFrame that exists has already been redacted. Constructing one is the
// last step of the pipeline, not the first, so there is no path by which a
// new sink receives unredacted output (INV-4).
//
// Seq makes resumption non-duplicating. State transitions are idempotent by
// (run id, state); log lines are idempotent by nothing, so a reconnect that
// re-sent a buffer would duplicate exactly the output covering the outage.
// The server keeps the first frame it sees for a given (RunID, Seq) and
// discards later ones (§7.2).
type LogFrame struct {
	RunID  string    `json:"run_id"`
	Seq    uint64    `json:"seq"`
	Stream Stream    `json:"stream"`
	At     time.Time `json:"at"`
	Line   string    `json:"line"`
}
