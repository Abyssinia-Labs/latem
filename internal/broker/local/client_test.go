package local

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The case RUNNER_SPEC §7.3 is written about. With no agent running, `latem
// run` must queue the work and exit 0 — a refusal here is a product where
// nothing can be submitted until a daemon is up, which is a different product.
func TestSubmit_AgentDownQueuesAndSucceeds(t *testing.T) {
	db := newStore(t)
	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)

	sub, err := c.Submit(t.Context(), runRequest("r_offline"), SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit with no agent: %v", err)
	}
	if sub.RunID != "r_offline" {
		t.Errorf("RunID = %q, want r_offline", sub.RunID)
	}
	if sub.AgentRunning {
		t.Error("AgentRunning = true with no agent listening")
	}

	run, err := db.Get(t.Context(), "r_offline")
	if err != nil {
		t.Fatalf("the run was not queued: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued", run.State)
	}
}

// A socket file left by a crashed agent looks exactly like a live one to
// anything that only stats it. The client has to dial, or `latem run` reports
// success against a socket nobody is holding.
func TestSubmit_StaleSocketCountsAsAgentDown(t *testing.T) {
	db := newStore(t)
	path := socketPath(t)

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c := newClient(t, path, db)
	sub, err := c.Submit(t.Context(), runRequest("r_stale"), SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.AgentRunning {
		t.Error("a stale socket was read as a live agent")
	}
	if _, err := db.Get(t.Context(), "r_stale"); err != nil {
		t.Errorf("the run was not queued: %v", err)
	}
}

// §7.3: `--wait` and `--follow` against a down agent are an error, not a hang.
// The message has to carry the start command, because the first ten minutes of
// this product are entirely people who have not started the agent yet.
func TestSubmit_RequireAgentIsAnErrorNotAHang(t *testing.T) {
	db := newStore(t)
	socket := filepath.Join(t.TempDir(), "nobody.sock")
	c := newClient(t, socket, db)

	sub, err := c.Submit(t.Context(), runRequest("r_wait"), SubmitOptions{RequireAgent: true})
	if err == nil {
		t.Fatal("Submit with RequireAgent succeeded against a down agent")
	}
	if !errors.Is(err, ErrAgentDown) {
		t.Errorf("error does not match ErrAgentDown: %v", err)
	}

	var down *AgentDownError
	if !errors.As(err, &down) {
		t.Fatalf("error is %T, want *AgentDownError", err)
	}
	if down.Code() != protocol.ErrAgentDown {
		t.Errorf("Code() = %q, want %q", down.Code(), protocol.ErrAgentDown)
	}
	if down.RunID != "r_wait" {
		t.Errorf("RunID = %q, want the run the caller asked for", down.RunID)
	}
	if !strings.Contains(err.Error(), "latem agent") {
		t.Errorf("message = %q, want the start command in it", err)
	}
	if !strings.Contains(err.Error(), socket) {
		t.Errorf("message = %q, want the socket path in it", err)
	}
	if !strings.Contains(err.Error(), string(protocol.ErrAgentDown)) {
		t.Errorf("message = %q, want the stable code in it", err)
	}

	// Nothing is recorded. Queueing a run the user asked to watch would leave
	// a build to start hours later, unwatched, after they moved on.
	if sub.RunID != "" {
		t.Errorf("Submission = %+v, want the zero value when nothing was queued", sub)
	}
	if _, err := db.Get(t.Context(), "r_wait"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a run was queued anyway: %v", err)
	}

	// The exit status is part of the interface a script sees.
	if ExitAgentDown != 3 {
		t.Errorf("ExitAgentDown = %d, want 3", ExitAgentDown)
	}
}

// With an agent up the client must not write the row itself: the agent is the
// process that read the config and therefore the only one that knows the queue
// TTL, and one writer on the happy path is one less thing contending for
// sqlite's write lock.
func TestSubmit_AgentUpLetsTheAgentWrite(t *testing.T) {
	b, db := newBroker(t, func(o *Options) { o.QueueTTL = time.Hour })
	consume(t, b)

	c := newClient(t, b.Socket(), db)
	sub, err := c.Submit(t.Context(), runRequest("r_live"), SubmitOptions{})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sub.AgentRunning {
		t.Error("AgentRunning = false against a live broker")
	}

	run, err := db.Get(t.Context(), "r_live")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.ExpiresAt == nil {
		t.Error("no expires_at; the agent's configured queue_ttl was not applied")
	}
}

// The agent's refusal has to reach the user. A fire-and-forget client would
// print a run id for work that was declined.
func TestSubmit_SurfacesTheAgentsRefusal(t *testing.T) {
	b, db := newBroker(t)
	consume(t, b)

	seedQueued(t, db, "r_finished")
	if err := db.Transition(t.Context(), "r_finished", protocol.StateCancelled,
		protocol.Update{State: protocol.StateCancelled}); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	c := newClient(t, b.Socket(), db)
	if _, err := c.Submit(t.Context(), runRequest("r_finished"), SubmitOptions{}); err == nil {
		t.Fatal("Submit succeeded for a run that already finished")
	}
}

func TestSubmit_RejectsATraversalID(t *testing.T) {
	db := newStore(t)
	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)

	req := runRequest("../../../../tmp/x")
	if _, err := c.Submit(t.Context(), req, SubmitOptions{}); err == nil {
		t.Fatal("Submit accepted a traversal run id")
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "..", "..", "..", "..", "tmp", "x")); err == nil {
		t.Fatal("something was written outside the temp directory")
	}
}

func TestSubmit_QueuesWithPriorityAndDeadline(t *testing.T) {
	db := newStore(t)
	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)

	deadline := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)
	if _, err := c.Submit(t.Context(), runRequest("r_prio"), SubmitOptions{
		Priority: 5, ExpiresAt: deadline,
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	run, err := db.Get(t.Context(), "r_prio")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.ExpiresAt == nil || !run.ExpiresAt.Equal(deadline) {
		t.Errorf("expires_at = %v, want %v", run.ExpiresAt, deadline)
	}
	if depth, err := db.QueueDepth(t.Context()); err != nil || depth != 1 {
		t.Errorf("QueueDepth = %d (%v), want 1", depth, err)
	}
}

// Cancel with an agent up is a relay and nothing more: the store is untouched,
// because §11's teardown decides what the run becomes and it is the only thing
// that knows whether the run reached collecting.
func TestCancel_AgentUpRelaysAndDoesNotTouchTheStore(t *testing.T) {
	b, db := newBroker(t)
	next := consume(t, b)
	seedQueued(t, db, "r_relay")

	c := newClient(t, b.Socket(), db)
	if err := c.Cancel(t.Context(), protocol.CancelRequest{ID: "r_relay", Reason: "typo"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	cmd, ok := next(2 * time.Second)
	if !ok {
		t.Fatal("the cancel never reached the agent")
	}
	if cmd.Kind != protocol.CommandCancel || cmd.Cancel.ID != "r_relay" {
		t.Fatalf("delivered %+v, want a cancel for r_relay", cmd)
	}

	run, err := db.Get(t.Context(), "r_relay")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("state = %s; the broker transitioned a run the agent owns", run.State)
	}
}

// A run that has not started is held by nothing except the queue, which the
// client can reach directly. Refusing to cancel it because no daemon is up
// would leave the user with a build they asked to stop starting later anyway.
func TestCancel_AgentDownCancelsAQueuedRun(t *testing.T) {
	db := newStore(t)
	seedQueued(t, db, "r_queued")

	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)
	if err := c.Cancel(t.Context(), protocol.CancelRequest{ID: "r_queued", Reason: "changed my mind"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	run, err := db.Get(t.Context(), "r_queued")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateCancelled {
		t.Errorf("state = %s, want cancelled", run.State)
	}
	if run.CancelReason != "changed my mind" {
		t.Errorf("cancel_reason = %q, want the reason recorded", run.CancelReason)
	}
	if run.FinishedAt == nil {
		t.Error("finished_at was not set on a terminal run")
	}
	if depth, err := db.QueueDepth(t.Context()); err != nil || depth != 0 {
		t.Errorf("QueueDepth = %d (%v), want 0", depth, err)
	}
}

// Only the process that started the work can stop it. Reporting success for a
// run left mid-flight by a dead agent would be a Stop button that does nothing.
func TestCancel_AgentDownRefusesAnInFlightRun(t *testing.T) {
	db := newStore(t)
	seedQueued(t, db, "r_inflight")
	if err := db.Claim(t.Context(), "r_inflight"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)
	err := c.Cancel(t.Context(), protocol.CancelRequest{ID: "r_inflight"})
	if !errors.Is(err, ErrAgentDown) {
		t.Fatalf("Cancel = %v, want ErrAgentDown", err)
	}
}

// §7.1: a cancel is idempotent and a no-op for an unknown, unclaimed, or
// already-terminal id. A no-op reports success — a Stop button that errors
// because the build already finished is noise.
func TestCancel_NoOpForUnknownAndTerminalRuns(t *testing.T) {
	db := newStore(t)
	seedQueued(t, db, "r_over")
	if err := db.Transition(t.Context(), "r_over", protocol.StateRejected,
		protocol.Update{State: protocol.StateRejected}); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)
	for _, id := range []string{"r_over", "r_nosuchrun"} {
		if err := c.Cancel(t.Context(), protocol.CancelRequest{ID: id}); err != nil {
			t.Errorf("Cancel(%s) = %v, want a silent no-op", id, err)
		}
	}
}

func TestCancel_RejectsATraversalID(t *testing.T) {
	db := newStore(t)
	c := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)

	if err := c.Cancel(t.Context(), protocol.CancelRequest{ID: "../../etc/passwd"}); err == nil {
		t.Fatal("Cancel accepted a traversal run id")
	}
}

func TestAgentRunning(t *testing.T) {
	db := newStore(t)
	b, _ := newBroker(t)

	live := newClient(t, b.Socket(), db)
	if !live.AgentRunning(t.Context()) {
		t.Error("AgentRunning = false against a live broker")
	}

	dead := newClient(t, filepath.Join(t.TempDir(), "nobody.sock"), db)
	if dead.AgentRunning(t.Context()) {
		t.Error("AgentRunning = true against nothing")
	}
}

func TestNewClient_Requires(t *testing.T) {
	if _, err := NewClient(ClientOptions{Store: newStore(t)}); err == nil {
		t.Error("NewClient with no socket succeeded")
	}
	if _, err := NewClient(ClientOptions{Socket: "/tmp/x.sock"}); err == nil {
		t.Error("NewClient with no store succeeded; the agent-down path needs the queue")
	}
}

// An agent that accepts the connection and then dies mid-conversation must not
// look like success. This is the one transport state a real listener cannot be
// put into on demand, which is why the dialer is injectable.
func TestExchange_SilentAgentIsAnError(t *testing.T) {
	db := newStore(t)

	c, err := NewClient(ClientOptions{
		Socket:  "/nonexistent/agent.sock",
		Store:   db,
		Timeout: 500 * time.Millisecond,
		dial: func(context.Context, string) (net.Conn, error) {
			ours, theirs := net.Pipe()
			go func() {
				// Read the command, answer nothing, hang up.
				buf := make([]byte, 4096)
				_, _ = theirs.Read(buf)
				_ = theirs.Close()
			}()
			return ours, nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := c.Submit(t.Context(), runRequest("r_silent"), SubmitOptions{}); err == nil {
		t.Fatal("Submit succeeded against an agent that answered nothing")
	}
}

// A reply from an agent built from different source is worth failing on
// loudly: both ends of this socket are meant to be the same binary.
func TestDecodeReply_StrictAboutUnknownFields(t *testing.T) {
	var r reply
	if err := decodeReply([]byte(`{"ok":true,"surprise":1}`), &r); err == nil {
		t.Fatal("an unknown reply field was accepted")
	}
}
