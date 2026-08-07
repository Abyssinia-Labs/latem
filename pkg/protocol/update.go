package protocol

import "time"

// Update publishes a state transition. It is idempotent by (run id, state):
// a broker receiving the same transition twice must treat the second as a
// no-op, because the agent replays unacknowledged transitions on reconnect.
type Update struct {
	State      RunState      `json:"state"`
	ExitCode   *int          `json:"exit_code,omitempty"`
	ErrorCode  ErrorCode     `json:"error_code,omitempty"`
	ErrorMsg   string        `json:"error_message,omitempty"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	Artifacts  []ArtifactRef `json:"artifacts,omitempty"`
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
