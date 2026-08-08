package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// The happy path, asserted on both of its effects. A reply that says "queued"
// while nothing reached sqlite is the exact failure the durability ordering in
// dispatchRun exists to prevent, so the row is checked as well as the answer.
func TestServe_RunIsQueuedAndDelivered(t *testing.T) {
	b, db := newBroker(t)
	next := consume(t, b)

	req := runRequest("r_happy")
	req.Inputs = map[string]string{"channel": "beta"}

	conn := dial(t, b.Socket())
	got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(req)))
	if !got.OK || got.RunID != "r_happy" {
		t.Fatalf("reply = %+v, want ok with the run id", got)
	}

	run, err := db.Get(t.Context(), "r_happy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.State != protocol.StateQueued {
		t.Errorf("state = %s, want queued", run.State)
	}
	if run.Repo != "myapp" || run.Job != "build" || run.Ref != "main" {
		t.Errorf("row = %+v, want the request's repo/job/ref", run)
	}

	cmd, ok := next(2 * time.Second)
	if !ok {
		t.Fatal("the command never reached the agent")
	}
	if cmd.Kind != protocol.CommandRun || cmd.Run.ID != "r_happy" {
		t.Fatalf("delivered %+v, want a run for r_happy", cmd)
	}
	// The delivered command carries the request's own inputs, not the store's
	// redacted copy: the agent builds the environment from these.
	if cmd.Run.Inputs["channel"] != "beta" {
		t.Errorf("inputs = %v, want the values as submitted", cmd.Run.Inputs)
	}
}

// `latem cancel` and a Stop button on a phone are the same command on two
// transports (§7.3). If the local one needed its own verb, the closed set in
// INV-2 would have a local exception in it.
func TestServe_CancelIsRelayedUnchanged(t *testing.T) {
	b, _ := newBroker(t)
	next := consume(t, b)

	cmd := protocol.NewCancelCommand(protocol.CancelRequest{
		ID: "r_stop", Actor: "phone", Reason: "wrong branch",
	})

	conn := dial(t, b.Socket())
	if got := sendRaw(t, conn, mustJSON(t, cmd)); !got.OK {
		t.Fatalf("reply = %+v, want ok", got)
	}

	delivered, ok := next(2 * time.Second)
	if !ok {
		t.Fatal("the cancel never reached the agent")
	}
	if delivered.Kind != protocol.CommandCancel {
		t.Fatalf("kind = %q, want cancel", delivered.Kind)
	}
	if delivered.Cancel.ID != "r_stop" || delivered.Cancel.Reason != "wrong branch" {
		t.Errorf("cancel = %+v, want the payload unchanged", delivered.Cancel)
	}
}

// A cancel is durable nowhere: nothing replays it and no table remembers it.
// Answering "ok" for one that was never handed to the agent would make the
// Stop button lie.
func TestServe_UndeliveredCancelIsRefused(t *testing.T) {
	b, _ := newBroker(t, func(o *Options) {
		o.CommandBuffer = 1
		o.deliver = 50 * time.Millisecond
	})

	// Nobody calls Commands, so the buffer fills and stays full.
	conn := dial(t, b.Socket())
	first := sendRaw(t, conn, mustJSON(t, protocol.NewCancelCommand(protocol.CancelRequest{ID: "r_one"})))
	if !first.OK {
		t.Fatalf("first cancel = %+v, want ok (it fits in the buffer)", first)
	}

	second := dial(t, b.Socket())
	got := sendRaw(t, second, mustJSON(t, protocol.NewCancelCommand(protocol.CancelRequest{ID: "r_two"})))
	if got.OK {
		t.Fatal("a cancel nobody received was reported as delivered")
	}
	if !strings.Contains(got.Error, "not delivered") {
		t.Errorf("error = %q, want it to say the cancel was not delivered", got.Error)
	}
}

// A run, by contrast, is durable before the reply, so an agent that is not
// reading costs latency and not a build. This is the asymmetry §7.3 buys with
// the sqlite queue, and it is worth pinning because it looks like an
// inconsistency until you know why.
func TestServe_RunSurvivesAnAgentThatIsNotReading(t *testing.T) {
	b, db := newBroker(t, func(o *Options) {
		o.CommandBuffer = 1
		o.deliver = 50 * time.Millisecond
	})

	for _, id := range []string{"r_first", "r_second"} {
		conn := dial(t, b.Socket())
		if got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest(id)))); !got.OK {
			t.Fatalf("%s: reply = %+v, want ok", id, got)
		}
		run, err := db.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("%s not queued: %v", id, err)
		}
		if run.State != protocol.StateQueued {
			t.Errorf("%s state = %s, want queued", id, run.State)
		}
	}
}

// Everything a hostile or broken client can put on the socket.
//
// The assertions are the same for every row on purpose: a refusal, and nothing
// written. A decoder that quietly zero-valued an unknown kind, or an id check
// that ran after the row was created, would pass one of those and fail the
// other.
func TestServe_RejectsHostileFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		// want is a fragment the refusal must mention, so a test cannot pass
		// on a refusal that happened for an unrelated reason.
		want string
	}{
		{
			name:  "unknown kind is refused, not zero-valued",
			frame: []byte(`{"kind":"exec","run":{"id":"r_1","repo":"myapp","job":"build","ref":"main","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`),
			want:  "unknown command kind",
		},
		{
			name:  "empty kind",
			frame: []byte(`{"kind":""}`),
			want:  "no kind",
		},
		{
			name:  "traversal run id",
			frame: []byte(`{"kind":"run","run":{"id":"../../../../tmp/x","repo":"myapp","job":"build","ref":"main","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`),
			want:  "run id",
		},
		{
			name:  "dotted run id",
			frame: []byte(`{"kind":"run","run":{"id":"..","repo":"myapp","job":"build","ref":"main","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`),
			want:  "run id",
		},
		{
			name:  "traversal cancel id",
			frame: []byte(`{"kind":"cancel","cancel":{"id":"../../etc/passwd"}}`),
			want:  "run id",
		},
		{
			name:  "unknown top-level field",
			frame: []byte(`{"kind":"cancel","cancel":{"id":"r_1"},"priority":9}`),
			want:  "unknown field",
		},
		{
			name:  "unknown field inside the payload",
			frame: []byte(`{"kind":"run","run":{"id":"r_1","repo":"myapp","job":"build","ref":"main","command":"rm -rf /","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}}}`),
			want:  "unknown field",
		},
		{
			name:  "kind and payload disagree",
			frame: []byte(`{"kind":"run","cancel":{"id":"r_1"}}`),
			want:  "no run payload",
		},
		{
			name:  "both payloads",
			frame: []byte(`{"kind":"cancel","run":{"id":"r_1","repo":"myapp","job":"build","ref":"main","origin":{"kind":"cli","requested_at":"2026-01-01T00:00:00Z"}},"cancel":{"id":"r_1"}}`),
			want:  "run payload",
		},
		{
			name:  "truncated json",
			frame: []byte(`{"kind":"run","run":{"id":"r_1"`),
			want:  "unexpected end of JSON input",
		},
		{
			name:  "not an object",
			frame: []byte(`["kind","run"]`),
			want:  "decoding command",
		},
		{
			name:  "json null",
			frame: []byte(`null`),
			want:  "no kind",
		},
		{
			name:  "not json at all",
			frame: []byte(`GET / HTTP/1.1`),
			want:  "invalid character",
		},
		{
			name:  "empty object",
			frame: []byte(`{}`),
			want:  "no kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, db := newBroker(t)
			consume(t, b)

			conn := dial(t, b.Socket())
			got := sendRaw(t, conn, tc.frame)
			if got.OK {
				t.Fatalf("accepted %s", tc.frame)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Errorf("error = %q, want it to mention %q", got.Error, tc.want)
			}

			if runs, err := db.List(t.Context(), store.Filter{}); err != nil {
				t.Fatalf("List: %v", err)
			} else if len(runs) != 0 {
				t.Errorf("a refused frame created %d run(s): %+v", len(runs), runs)
			}
		})
	}
}

// An oversized frame is the one malformed input that costs the reader its
// framing: past the buffer limit there is no way to know where the next frame
// starts, so the connection has to end rather than resynchronise on a newline
// that may be inside a value the client chose.
func TestServe_OversizedFrameIsRefusedAndEndsTheConnection(t *testing.T) {
	b, db := newBroker(t)
	consume(t, b)

	req := runRequest("r_big")
	req.Inputs = map[string]string{"blob": strings.Repeat("A", MaxFrameBytes)}
	frame := append(mustJSON(t, protocol.NewRunCommand(req)), '\n')

	conn := dial(t, b.Socket())
	// The write is expected to fail partway: the broker answers and hangs up
	// as soon as the frame passes the limit, so the client's own send is what
	// gets the broken pipe. That is the behaviour under test, not a flake.
	_, _ = conn.Write(frame)

	// Whatever arrived before the close must not be an acceptance, and the
	// connection must not still be taking commands.
	sc := newFrameReader(conn)
	if sc.Scan() {
		var r reply
		if err := decodeReply(sc.Bytes(), &r); err != nil {
			t.Fatalf("decode %q: %v", sc.Bytes(), err)
		}
		if r.OK {
			t.Fatal("an oversized frame was accepted")
		}
		if !strings.Contains(r.Error, "limit") {
			t.Errorf("error = %q, want it to name the limit", r.Error)
		}
	}

	if runs, err := db.List(t.Context(), store.Filter{}); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(runs) != 0 {
		t.Errorf("an oversized frame created %d run(s)", len(runs))
	}
}

// A client that opens the socket and never sends a newline must not be able to
// grow the agent's read buffer without limit. The refusal above proves the
// bound; this proves the bound is not simply "whatever arrived in one packet".
func TestServe_UnterminatedFrameIsBounded(t *testing.T) {
	b, _ := newBroker(t)
	consume(t, b)

	conn := dial(t, b.Socket())
	payload := bytes.Repeat([]byte("A"), 8<<10)
	for range (MaxFrameBytes / len(payload)) + 4 {
		if _, err := conn.Write(payload); err != nil {
			break // the broker hung up, which is the outcome under test
		}
	}
	_, _ = conn.Write([]byte("\n"))

	sc := newFrameReader(conn)
	if !sc.Scan() {
		return // refused by hanging up
	}
	var r reply
	if err := decodeReply(sc.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK {
		t.Fatal("an unterminated frame past the limit was accepted")
	}
}

// Blank lines are framing, not commands. A client using "\n" as a keepalive
// should not be disconnected for it.
func TestServe_BlankLinesAreSkipped(t *testing.T) {
	b, _ := newBroker(t)
	next := consume(t, b)

	conn := dial(t, b.Socket())
	if _, err := conn.Write([]byte("\n\n   \n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := sendRaw(t, conn, mustJSON(t, protocol.NewCancelCommand(protocol.CancelRequest{ID: "r_after"}))); !got.OK {
		t.Fatalf("reply after blank lines = %+v, want ok", got)
	}
	if _, ok := next(2 * time.Second); !ok {
		t.Fatal("the command after the blank lines was lost")
	}
}

// One connection, several commands: the framing is a stream, not a
// request-response with a socket per call.
func TestServe_MultipleFramesOnOneConnection(t *testing.T) {
	b, db := newBroker(t)
	next := consume(t, b)

	conn := dial(t, b.Socket())
	for _, id := range []string{"r_a", "r_b", "r_c"} {
		if got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest(id)))); !got.OK {
			t.Fatalf("%s: %+v", id, got)
		}
		if _, ok := next(2 * time.Second); !ok {
			t.Fatalf("%s was not delivered", id)
		}
	}
	runs, err := db.List(t.Context(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 3 {
		t.Errorf("queued %d runs, want 3", len(runs))
	}
}

// Re-queueing a finished run under its old id would make the run history lie
// about which build produced which artifact.
func TestServe_RefusesToRequeueAFinishedRun(t *testing.T) {
	b, db := newBroker(t)
	consume(t, b)

	seedQueued(t, db, "r_done")
	if err := db.Transition(t.Context(), "r_done", protocol.StateCancelled,
		protocol.Update{State: protocol.StateCancelled}); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	conn := dial(t, b.Socket())
	got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest("r_done"))))
	if got.OK {
		t.Fatal("a terminal run was re-queued")
	}
	if !strings.Contains(got.Error, "already finished") {
		t.Errorf("error = %q, want it to say the run already finished", got.Error)
	}
}

// A re-notification for a run that is still queued is a retry, not a conflict.
func TestServe_RequeueingAQueuedRunIsIdempotent(t *testing.T) {
	b, db := newBroker(t)
	consume(t, b)

	seedQueued(t, db, "r_again")

	conn := dial(t, b.Socket())
	if got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest("r_again")))); !got.OK {
		t.Fatalf("reply = %+v, want ok", got)
	}
	runs, err := db.List(t.Context(), store.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("got %d rows, want the one run", len(runs))
	}
}

// The queue_ttl deadline (§11, §22) comes from the agent's configuration, so it
// is applied by the process that read the configuration.
func TestServe_AppliesTheConfiguredQueueTTL(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b, db := newBroker(t, func(o *Options) {
		o.QueueTTL = 90 * time.Minute
		o.now = func() time.Time { return frozen }
	})
	consume(t, b)

	conn := dial(t, b.Socket())
	if got := sendRaw(t, conn, mustJSON(t, protocol.NewRunCommand(runRequest("r_ttl")))); !got.OK {
		t.Fatalf("reply = %+v", got)
	}

	run, err := db.Get(t.Context(), "r_ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if run.ExpiresAt == nil {
		t.Fatal("no expires_at was recorded; the TTL sweep would never reject this run")
	}
	if want := frozen.Add(90 * time.Minute); !run.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", run.ExpiresAt, want)
	}
}

// Two consumers would split the command stream, so a cancel could reach the
// half that is not executing the run — an intermittently ignored Stop button.
func TestCommands_RefusesASecondConsumer(t *testing.T) {
	b, _ := newBroker(t)
	consume(t, b)

	if _, err := b.Commands(t.Context()); err == nil {
		t.Fatal("a second consumer was accepted")
	}
}

// The interface says the channel closes only on ctx cancellation.
func TestCommands_ClosesOnContextCancel(t *testing.T) {
	b, _ := newBroker(t)

	ctx, cancel := context.WithCancel(t.Context())
	ch, err := b.Commands(ctx)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("the channel yielded a command after cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the channel did not close on cancellation")
	}

	// And the consumer slot is released, so an agent that reconnects its own
	// command loop is not locked out by its predecessor.
	if _, err := b.Commands(t.Context()); err != nil {
		t.Errorf("Commands after cancellation: %v", err)
	}
}

func TestCommands_RefusedOnAClosedBroker(t *testing.T) {
	db := newStore(t)
	b, err := Open(Options{Socket: socketPath(t), Store: db})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := b.Commands(t.Context()); err == nil {
		t.Fatal("Commands on a closed broker succeeded")
	}
}

// Concurrent clients on one socket, under -race. The property is that every
// submission produces exactly one row and exactly one delivery: a shared
// scanner buffer, a shared reply buffer, or a racy accept loop would show up
// here as a lost run or a duplicated one.
func TestServe_ConcurrentClients(t *testing.T) {
	const clients = 24

	b, db := newBroker(t, func(o *Options) { o.CommandBuffer = clients })
	next := consume(t, b)

	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newClient(t, b.Socket(), db)
			sub, err := c.Submit(t.Context(), runRequest(fmt.Sprintf("r_%02d", i)), SubmitOptions{})
			if err != nil {
				errs <- err
				return
			}
			if !sub.AgentRunning {
				errs <- fmt.Errorf("%s: AgentRunning = false with a live broker", sub.RunID)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	seen := map[string]int{}
	for range clients {
		cmd, ok := next(5 * time.Second)
		if !ok {
			t.Fatalf("only %d of %d commands were delivered", len(seen), clients)
		}
		seen[cmd.Run.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s delivered %d times", id, n)
		}
	}

	runs, err := db.List(t.Context(), store.Filter{Limit: clients * 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != clients {
		t.Errorf("queued %d runs, want %d", len(runs), clients)
	}
}

// Exactly one claimant wins, and the rest get an ordinary outcome rather than
// a failure. Two agents executing one run is the failure Claim exists to
// prevent.
func TestClaim_ConcurrentExactlyOneWinner(t *testing.T) {
	const racers = 16

	b, db := newBroker(t)
	seedQueued(t, db, "r_contested")

	var wg sync.WaitGroup
	results := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- b.Claim(t.Context(), "r_contested")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	won, lost := 0, 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, broker.ErrAlreadyClaimed):
			lost++
		default:
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d winners, want exactly 1", won)
	}
	if lost != racers-1 {
		t.Errorf("%d losers, want %d", lost, racers-1)
	}
}
