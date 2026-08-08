package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The inbound command set is closed at two kinds (INV-2). A decoder that
// tolerates a third turns "I do not understand this" into "there is nothing to
// do", which is the difference between a refusal and a silent no-op.
func TestCommandUnmarshal_RejectsUnknownKind(t *testing.T) {
	for _, body := range []string{
		`{"kind":"approve","run":{"id":"r1"}}`,
		`{"kind":"exec","run":{"id":"r1"}}`,
		`{"kind":"RUN","run":{"id":"r1"}}`, // case-sensitive on purpose
		`{"kind":"","run":{"id":"r1"}}`,
		`{"run":{"id":"r1"}}`, // kind absent entirely
	} {
		var c Command
		if err := json.Unmarshal([]byte(body), &c); err == nil {
			t.Errorf("accepted %s, want rejection", body)
		}
	}
}

func TestCommandUnmarshal_PayloadMustMatchKind(t *testing.T) {
	tests := map[string]string{
		"run kind, no payload":      `{"kind":"run"}`,
		"cancel kind, no payload":   `{"kind":"cancel"}`,
		"run kind, cancel payload":  `{"kind":"run","cancel":{"id":"r1"}}`,
		"cancel kind, run payload":  `{"kind":"cancel","run":{"id":"r1"}}`,
		"both payloads":             `{"kind":"run","run":{"id":"r1"},"cancel":{"id":"r1"}}`,
		"run payload without an id": `{"kind":"run","run":{"repo":"api","job":"test"}}`,
		"cancel without an id":      `{"kind":"cancel","cancel":{"actor":"me"}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var c Command
			if err := json.Unmarshal([]byte(body), &c); err == nil {
				t.Fatalf("accepted %s, want rejection", body)
			}
		})
	}
}

// Strictness extends into the payloads. A field the agent does not understand
// is a field it would otherwise ignore, and ignoring part of a message at a
// trust boundary is how a control plane and an agent come to disagree about
// what was asked for.
func TestCommandUnmarshal_RejectsUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"kind":"run","run":{"id":"r1"},"image":"evil:latest"}`,
		`{"kind":"run","run":{"id":"r1","image":"evil:latest"}}`,
		`{"kind":"run","run":{"id":"r1","command":"rm -rf /"}}`,
		`{"kind":"run","run":{"id":"r1","origin":{"kind":"cli","escalate":true}}}`,
		`{"kind":"cancel","cancel":{"id":"r1","signal":"KILL"}}`,
	} {
		var c Command
		if err := json.Unmarshal([]byte(body), &c); err == nil {
			t.Errorf("accepted %s, want rejection", body)
		}
	}
}

// Run ids become filesystem paths — work/<id>, logs/<id>.log,
// artifacts/<id>/ — and container and VM names. INV-3 enumerates validation
// for repo, ref, and inputs and says nothing about id, which is how a control
// plane sending {"id": "../../../../tmp/x"} could get a file written wherever
// the agent's user can write.
func TestCommandUnmarshal_RejectsTraversalIDs(t *testing.T) {
	hostile := []string{
		"../../../../tmp/pwned",
		"..",
		".",
		"a/b",
		"a\\b",
		"run/../../../etc/cron.d/evil",
		"run\x00id",
		"run id",
		".hidden",
		"-starts-with-dash",
		"run.id", // dots excluded outright, so no id can be "." or ".."
		strings.Repeat("x", 65),
		"",
	}

	for _, id := range hostile {
		t.Run(id, func(t *testing.T) {
			for _, body := range []string{
				`{"kind":"run","run":{"id":` + quote(id) + `}}`,
				`{"kind":"cancel","cancel":{"id":` + quote(id) + `}}`,
			} {
				var c Command
				if err := json.Unmarshal([]byte(body), &c); err == nil {
					t.Errorf("accepted hostile id %q", id)
				}
			}
		})
	}
}

func TestValidateID_AcceptsRealIDs(t *testing.T) {
	for _, id := range []string{
		"run_01H8XGJWBWBAQ4ZZY1N2P3Q4R5",
		"r1",
		"RUN-2026-08-07-0001",
		strings.Repeat("x", MaxIDLength),
	} {
		if err := ValidateID(id); err != nil {
			t.Errorf("rejected legitimate id %q: %v", id, err)
		}
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestCommandUnmarshal_AcceptsWellFormed(t *testing.T) {
	const body = `{
		"kind":"run",
		"run":{
			"id":"run_01H",
			"repo":"myapp",
			"job":"ios-release",
			"ref":"main",
			"inputs":{"channel":"internal"},
			"origin":{"kind":"manual","actor":"michael","requested_at":"2026-08-07T12:00:00Z"}
		}
	}`
	var c Command
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("rejected a well-formed command: %v", err)
	}
	if c.Kind != CommandRun || c.Run == nil {
		t.Fatalf("got kind=%q run=%v", c.Kind, c.Run)
	}
	if c.Run.Job != "ios-release" || c.Run.Inputs["channel"] != "internal" {
		t.Errorf("payload not decoded: %+v", c.Run)
	}
	if c.Run.Origin.RequestedAt.IsZero() {
		t.Error("origin timestamp not decoded")
	}
}

// Reason is display text from outside the trust boundary. It is truncated
// rather than rejected, because a cancel should always take effect — the point
// of the bound is to stop the field being used for bulk data, not to give a
// caller a way to make Stop fail.
func TestCancelReason_TruncatedOnDecode(t *testing.T) {
	long := strings.Repeat("é", MaxCancelReasonRunes+50) // multi-byte on purpose
	body, err := json.Marshal(map[string]any{
		"kind":   "cancel",
		"cancel": map[string]any{"id": "r1", "reason": long},
	})
	if err != nil {
		t.Fatal(err)
	}

	var c Command
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("rejected an over-long reason instead of truncating: %v", err)
	}
	if got := utf8.RuneCountInString(c.Cancel.Reason); got != MaxCancelReasonRunes {
		t.Errorf("reason is %d runes, want %d", got, MaxCancelReasonRunes)
	}
	if !utf8.ValidString(c.Cancel.Reason) {
		t.Error("truncation split a multi-byte rune")
	}
}

func TestNewCancelCommand_NormalizesReason(t *testing.T) {
	c := NewCancelCommand(CancelRequest{
		ID:     "r1",
		Reason: strings.Repeat("x", MaxCancelReasonRunes+1),
	})
	if got := utf8.RuneCountInString(c.Cancel.Reason); got != MaxCancelReasonRunes {
		t.Errorf("constructor left %d runes, want %d", got, MaxCancelReasonRunes)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("constructor produced an invalid command: %v", err)
	}
}

func TestNewRunCommand_Validates(t *testing.T) {
	if err := NewRunCommand(RunRequest{ID: "r1"}).Validate(); err != nil {
		t.Errorf("valid run command rejected: %v", err)
	}
	if err := NewRunCommand(RunRequest{}).Validate(); err == nil {
		t.Error("run command with no id accepted")
	}
}

// Round-tripping matters because the local broker constructs commands directly
// while the remote broker decodes them, and both feed the same handler.
func TestCommand_RoundTrip(t *testing.T) {
	orig := NewRunCommand(RunRequest{
		ID:     "r1",
		Repo:   "api",
		Job:    "test",
		Ref:    "refs/heads/main",
		Inputs: map[string]string{"k": "v"},
	})
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var back Command
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("could not decode what we encoded: %v", err)
	}
	if back.Run.Ref != orig.Run.Ref || back.Run.Inputs["k"] != "v" {
		t.Errorf("round trip lost data: %+v", back.Run)
	}
}
