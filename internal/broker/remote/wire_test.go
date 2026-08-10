package remote

import (
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestDecode_EnforcesTheTrustBoundaryItInherits.
//
// Nothing here re-implements [protocol.Command]'s rules; the point of the test
// is that they are still in force after a frame has been wrapped in an
// envelope. A transport that unwrapped the command with a lenient decoder would
// re-open every hole INV-2 and amendment A6 closed, one layer up, and no test
// in pkg/protocol would notice.
func TestDecode_EnforcesTheTrustBoundaryItInherits(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"a third command kind": {
			`{"kind":"command","command":{"kind":"approve","run":{"id":"r_1","repo":"a","job":"b","ref":"c","origin":{"requested_at":"2024-01-01T00:00:00Z"}}}}`,
			"unknown command kind",
		},
		"a run id that traverses": {
			`{"kind":"command","command":{"kind":"run","run":{"id":"../../../../tmp/x","repo":"a","job":"b","ref":"c","origin":{"requested_at":"2024-01-01T00:00:00Z"}}}}`,
			"must match",
		},
		"an unknown field inside the payload": {
			`{"kind":"command","command":{"kind":"run","run":{"id":"r_1","repo":"a","job":"b","ref":"c","image":"evil","origin":{"requested_at":"2024-01-01T00:00:00Z"}}}}`,
			"unknown field",
		},
		"an unknown field on the envelope": {
			`{"kind":"hello","hello":{"machine_id":"m1"},"exec":"rm -rf /"}`,
			"unknown field",
		},
		"a payload that does not match its kind": {
			`{"kind":"command","hello":{"machine_id":"m1"}}`,
			"no command payload",
		},
		"no kind at all": {
			`{"hello":{"machine_id":"m1"}}`,
			"no kind",
		},
		"a frame only the agent may send": {
			`{"kind":"heartbeat","heartbeat":{"machine_id":"m1","agent_version":"x","os":"linux","arch":"arm64","runners":[],"disk_free_gb":1,"draining":false}}`,
			"may not send",
		},
		"a resume echoed back at us": {
			`{"kind":"resume","resume":{"machine_id":"m1","runs":[]}}`,
			"may not send",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decode([]byte(tc.body))
			if err == nil {
				t.Fatalf("decode accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestDecode_AcceptsWhatAControlPlaneLegitimatelySends.
func TestDecode_AcceptsWhatAControlPlaneLegitimatelySends(t *testing.T) {
	tests := map[string]string{
		"a run":    `{"kind":"command","id":"a1","command":{"kind":"run","run":{"id":"r_1","repo":"myapp","job":"build","ref":"main","origin":{"kind":"webhook","requested_at":"2024-01-01T00:00:00Z"}}}}`,
		"a cancel": `{"kind":"command","id":"a2","command":{"kind":"cancel","cancel":{"id":"r_1","actor":"sam","reason":"wrong branch"}}}`,
		"an ack":   `{"kind":"ack","id":"a3","ack":{"ok":true,"run_id":"r_1"}}`,
		"a hello":  `{"kind":"hello","hello":{"machine_id":"mch_1","protocol_version":"1.0","message":"welcome"}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decode([]byte(body)); err != nil {
				t.Errorf("decode refused %s: %v", name, err)
			}
		})
	}
}

// TestEncode_StampsTheProtocolVersion. §4 calls the wire independently
// versionable; amendment A4 gave it a version. A frame that did not carry it
// would make a version mismatch present as a decode failure with no clue in it.
func TestEncode_StampsTheProtocolVersion(t *testing.T) {
	data, err := encode(frame{Kind: kindHeartbeat, Heartbeat: &protocol.MachineHealth{MachineID: "m1"}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got frame
	if err := decodeAny(data, &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Version != protocol.Version {
		t.Errorf("frame carries protocol version %q, want %q", got.Version, protocol.Version)
	}
}

// TestEncode_RefusesAnOversizedFrame. The bound exists so a run whose single
// log line is pathological cannot produce a message the far side will refuse to
// read — which would kill the connection rather than the line.
func TestEncode_RefusesAnOversizedFrame(t *testing.T) {
	huge := protocol.LogFrame{RunID: "r_1", Seq: 1, Line: strings.Repeat("x", MaxFrameBytes+1)}
	if _, err := encode(frame{Kind: kindLogs, Logs: []protocol.LogFrame{huge}}); err == nil {
		t.Error("encode produced a frame past MaxFrameBytes")
	}
}

// TestCancelReasonIsTruncatedOnTheWayIn.
//
// The cap is [protocol.CancelRequest.Normalize]'s, applied by the frozen
// decoder. This asserts the envelope does not route around it — a 10 MB reason
// is display text from outside the trust boundary and would otherwise reach a
// log line and a database column.
func TestCancelReasonIsTruncatedOnTheWayIn(t *testing.T) {
	long := strings.Repeat("é", protocol.MaxCancelReasonRunes*3)
	body := `{"kind":"command","command":{"kind":"cancel","cancel":{"id":"r_1","reason":"` + long + `"}}}`

	f, err := decode([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len([]rune(f.Command.Cancel.Reason)); got != protocol.MaxCancelReasonRunes {
		t.Errorf("reason is %d runes, want it truncated to %d", got, protocol.MaxCancelReasonRunes)
	}
}
