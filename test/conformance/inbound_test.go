package conformance

// What a server may send, and what the agent must do with it.
//
// The other half of the lost cross-check. testdata/server-frames.jsonl is
// hand-authored — it is the far end of the wire, and generating it from the
// agent would produce a mirror of the agent's own assumptions rather than an
// independent statement of the format.
//
// # Refused and ignored are not the same thing, and the test says which
//
// From outside, "the agent refused a malformed frame" and "the agent quietly
// did nothing" produce identical observations: no command, no state change,
// the session still up. That is the shape of the remote-broker mutation that
// survived because two code paths were indistinguishable. So the transport's
// own logger is captured, and a fixture marked `refused` must produce the
// refusal line while a fixture marked `ignored` must not.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker/remote"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const serverCorpus = "server-frames.jsonl"

// refusalMarker is what internal/broker/remote logs when its decoder rejects an
// inbound frame. It is asserted on because it is the only thing that
// distinguishes a refusal from a shrug.
const refusalMarker = "refusing a malformed frame"

type serverFixture struct {
	Name      string          `json:"name"`
	Expect    string          `json:"expect"`
	RunID     string          `json:"run_id"`
	MachineID string          `json:"machine_id"`
	Why       string          `json:"why"`
	Frame     json.RawMessage `json:"frame"`
}

// TestConformance_ServerFrames replays every fixture in order against one live
// session.
//
// One session, in file order, deliberately: `hello` is stateful — the machine
// id it assigns persists — and a control plane does not get to send its frames
// in isolation. Running each fixture against a fresh broker would hide any
// cross-frame effect, which is where the interesting bugs are.
func TestConformance_ServerFrames(t *testing.T) {
	fixtures := loadServerFixtures(t)
	if len(fixtures) < 20 {
		t.Fatalf("the server corpus has %d fixtures; it covered more than that when it was "+
			"written, so something has truncated it", len(fixtures))
	}

	dir := shortTemp(t)
	db := openStore(t, dir)
	rec := newRecorder(t, "tok-inbound")
	logs := &logCapture{}

	b, err := remote.Open(remote.Options{
		URL:            rec.URL(),
		TokenFile:      writeTokenFile(t, dir, "tok-inbound"),
		MachineID:      "mac-mini-01",
		Store:          db,
		RequestTimeout: 20 * time.Second,
		Logger:         slog.New(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	ctx := context.Background()
	commands, err := b.Commands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec.waitForKind(t, "resume", 20*time.Second)

	// Negative control for the log capture. Every `refused` assertion below is
	// an assertion about a log line, so the capture has to be shown to work
	// before it is trusted to report an absence.
	logs.reset()
	rec.push(t, []byte(`{"kind":"nonsense-control"}`))
	if !logs.waitFor(refusalMarker, 10*time.Second) {
		t.Fatalf("the transport's logger produced no refusal for an obviously bad frame; every "+
			"`refused` fixture below would pass against anything.\ncaptured: %v", logs.lines())
	}
	drain(commands)

	canary := 0
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			logs.reset()
			drain(commands)
			before := b.MachineID()

			rec.push(t, f.Frame)

			switch f.Expect {
			case "command":
				cmd := waitForCommand(t, commands, 20*time.Second)
				if got := commandID(cmd); got != f.RunID {
					t.Errorf("the agent yielded a command for %q, want %q", got, f.RunID)
				}
				assertNoRefusal(t, logs, f)

			case "hello":
				waitUntil(t, 20*time.Second, func() bool { return b.MachineID() == f.MachineID })
				if b.MachineID() != f.MachineID {
					t.Errorf("machine id = %q, want %q", b.MachineID(), f.MachineID)
				}
				assertNoRefusal(t, logs, f)

			case "ignored":
				assertNoCommand(t, commands)
				if b.MachineID() != before {
					t.Errorf("a frame with no documented effect changed the machine id to %q",
						b.MachineID())
				}
				assertNoRefusal(t, logs, f)

			case "refused":
				if !logs.waitFor(refusalMarker, 10*time.Second) {
					t.Errorf("the agent did not refuse this frame — %s\ncaptured: %v",
						f.Why, logs.lines())
				}
				assertNoCommand(t, commands)
				if b.MachineID() != before {
					t.Errorf("a refused frame changed the machine id to %q", b.MachineID())
				}

			default:
				t.Fatalf("the fixture has an unknown expect value %q", f.Expect)
			}

			// A refusal must not be fatal to the session (PROTOCOL §2.3):
			// WebSocket messages carry their own boundaries, so the next one is
			// still intelligible. The canary proves the connection survived
			// rather than the test having asserted an absence against a socket
			// that had already gone.
			if f.Expect == "refused" {
				canary++
				id := canaryID(canary)
				rec.push(t, []byte(canaryFrame(id)))
				cmd := waitForCommand(t, commands, 20*time.Second)
				if got := commandID(cmd); got != id {
					t.Errorf("after a refusal the session yielded %q, want the canary %q", got, id)
				}
			}
		})
	}
}

// TestConformance_ServerCorpusCoversTheDocumentedCases keeps the corpus honest.
//
// A hand-authored corpus is only as good as what somebody remembered to put in
// it, and the failure mode is silent: a fixture is deleted while refactoring,
// the file still parses, and the suite still passes. These are the claims
// docs/PROTOCOL.md makes about the inbound direction, restated as a count.
func TestConformance_ServerCorpusCoversTheDocumentedCases(t *testing.T) {
	fixtures := loadServerFixtures(t)

	byExpect := map[string]int{}
	kinds := map[string]bool{}
	for _, f := range fixtures {
		byExpect[f.Expect]++
		var env struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(f.Frame, &env)
		kinds[env.Kind] = true
		if strings.TrimSpace(f.Why) == "" {
			t.Errorf("fixture %q has no `why`; a corpus line nobody can explain is one nobody "+
				"can safely delete", f.Name)
		}
	}

	// The three kinds a control plane may send (PROTOCOL §2.4) must all appear
	// as accepted cases, and every agent-only kind must appear as a refused
	// one — a server echoing one back is "either confused or probing".
	for _, k := range []string{"hello", "command", "ack"} {
		if !kinds[k] {
			t.Errorf("the corpus never sends a %q frame, which is one of the three a control "+
				"plane may send", k)
		}
	}
	for _, k := range []string{"resume", "update", "logs", "heartbeat", "claim"} {
		if !kinds[k] {
			t.Errorf("the corpus never tries to send %q back at the agent; PROTOCOL §2.4 says "+
				"that must be refused by name", k)
		}
	}

	for _, e := range []string{"command", "hello", "ignored", "refused"} {
		if byExpect[e] == 0 {
			t.Errorf("no fixture expects %q", e)
		}
	}
	if byExpect["refused"] < 15 {
		t.Errorf("only %d refusal fixtures; the strictness of the decoder is the agent's whole "+
			"inbound defence and deserves more than that", byExpect["refused"])
	}
}

// ---------------------------------------------------------------------------

func loadServerFixtures(t *testing.T) []serverFixture {
	t.Helper()
	var out []serverFixture
	for i, line := range readCorpus(t, serverCorpus) {
		var f serverFixture
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("fixture %d is malformed: %v\n%s", i, err, line)
		}
		out = append(out, f)
	}
	return out
}

func assertNoRefusal(t *testing.T, logs *logCapture, f serverFixture) {
	t.Helper()
	// A short settle, because the refusal would be logged by the read loop
	// rather than by whatever produced the observable effect.
	time.Sleep(50 * time.Millisecond)
	for _, line := range logs.lines() {
		if strings.Contains(line, refusalMarker) {
			t.Errorf("the agent refused a frame it is supposed to accept — %s\n%s", f.Why, line)
		}
	}
}

func assertNoCommand(t *testing.T, commands <-chan protocol.Command) {
	t.Helper()
	select {
	case cmd := <-commands:
		t.Errorf("the agent yielded a command for a frame that should have had no effect: %+v", cmd)
	case <-time.After(250 * time.Millisecond):
	}
}

func waitForCommand(t *testing.T, commands <-chan protocol.Command, timeout time.Duration) protocol.Command {
	t.Helper()
	select {
	case cmd := <-commands:
		return cmd
	case <-time.After(timeout):
		t.Fatal("no command arrived")
		return protocol.Command{}
	}
}

func drain(commands <-chan protocol.Command) {
	for {
		select {
		case <-commands:
		default:
			return
		}
	}
}

func commandID(cmd protocol.Command) string {
	switch {
	case cmd.Run != nil:
		return cmd.Run.ID
	case cmd.Cancel != nil:
		return cmd.Cancel.ID
	default:
		return ""
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func canaryID(n int) string { return "rcanary" + strconv.Itoa(n) }

func canaryFrame(id string) string {
	return `{"kind":"command","protocol_version":"1.0","command":{"kind":"run","run":{` +
		`"id":"` + id + `","repo":"myapp","job":"test","ref":"main",` +
		`"origin":{"kind":"cli","requested_at":"2026-08-08T09:15:00Z"}}}}`
}

// ---------------------------------------------------------------------------
// the log capture

// logCapture is a slog.Handler that keeps messages.
//
// It is a handler rather than a writer so the assertion is about the message
// the transport chose, not about the formatting of a text handler that could be
// swapped underneath it.
type logCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.String())
		return true
	})
	c.mu.Lock()
	c.msgs = append(c.msgs, b.String())
	c.mu.Unlock()
	return nil
}

// WithAttrs returns the same capture rather than a copy.
//
// The obvious implementation returns a new handler, and a new handler has its
// own slice — so anything the transport logs through a derived logger lands
// somewhere this test never reads, and every assertion about an absent log line
// passes for the wrong reason. That is the same class of mistake as a detector
// that enumerates nothing (amendment A25), reached by following the interface's
// own shape.
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *logCapture) WithGroup(string) slog.Handler { return c }

func (c *logCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func (c *logCapture) reset() {
	c.mu.Lock()
	c.msgs = nil
	c.mu.Unlock()
}

func (c *logCapture) waitFor(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range c.lines() {
			if strings.Contains(l, substr) {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
