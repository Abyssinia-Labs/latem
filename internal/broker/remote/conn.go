package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// waiter is one outstanding request.
//
// Either it is a synchronous caller holding ch — a claim or a report — or it is
// a log batch, which is resolved by advancing the run's sink rather than by
// waking anybody. Log batches deliberately do not block: the emitter calls a
// sink from its critical section (§16), and holding that for a round trip to
// another continent would stall the build's own stdout.
type waiter struct {
	ch      chan protocol.Ack
	runID   string
	highSeq uint64
}

// supervise is the reconnect loop. It is the only thing in this package that
// dials, and it runs for the whole life of the broker.
func (b *Broker) supervise(ctx context.Context) {
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		default:
		}

		conn, err := b.dial(ctx)
		if err != nil {
			if errors.Is(err, ErrRevoked) {
				// Stop. Not an error returned to anybody, not a closed command
				// channel, and above all not a stopped agent: §7.4 is explicit
				// that a revoked token must never disable local operation.
				return
			}
			if !b.stopping() {
				b.log.Warn("control plane unreachable; retrying",
					"url", b.url, "attempt", attempt+1, "err", err)
			}
			if !b.wait(ctx, attempt) {
				return
			}
			attempt++
			continue
		}

		// A session that got as far as sending its resume frame is a working
		// link, so the schedule starts again from one second. Resetting on the
		// bare handshake instead would let a server that accepts and immediately
		// hangs up hold this loop at the base delay indefinitely.
		if b.runSession(ctx, conn) {
			attempt = 0
		}
		if !b.wait(ctx, attempt) {
			return
		}
		attempt++
	}
}

// wait sleeps out one backoff step, reporting whether the loop should continue.
func (b *Broker) wait(ctx context.Context, attempt int) bool {
	d := b.backoff.Delay(attempt, b.jitter())
	return b.sleep(ctx, d)
}

// dial performs the handshake.
//
// The token is read here rather than at Open so that rotating it takes effect
// at the next reconnect. A 401 or 403 is translated to [ErrRevoked] and is the
// one failure the caller must not retry; everything else — DNS, refused, a 502
// from a proxy, a TLS error — is a partition, and partitions end.
func (b *Broker) dial(ctx context.Context) (*websocket.Conn, error) {
	header, err := b.authHeader()
	if err != nil {
		return nil, err
	}

	dctx, cancel := context.WithTimeout(ctx, b.handshake)
	defer cancel()

	// bodyclose is suppressed on the call below. websocket.Dial owns this
	// response body and documents that a caller never closes it: on success it
	// sets resp.Body to nil and the underlying ReadWriteCloser becomes the
	// connection, and on failure it has already closed the real body and
	// replaced it with a NopCloser over the first kilobyte, for the error
	// message. Closing it here would be a nil dereference on the success path.
	conn, resp, err := websocket.Dial(dctx, b.url, &websocket.DialOptions{ //nolint:bodyclose
		HTTPHeader: header,
		// Compression negotiation is left on: what this link carries is a
		// build's log volume, which is exactly what it pays off on.
	})
	if err != nil {
		if code := statusOf(resp); code == http.StatusUnauthorized || code == http.StatusForbidden {
			b.markRevoked(code)
			return nil, ErrRevoked
		}
		return nil, err
	}
	conn.SetReadLimit(MaxFrameBytes)
	return conn, nil
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// markRevoked records the refusal and says so once, loudly.
//
// Once, because the loop stops here: a line per retry would be the opposite of
// useful. The message names the file rather than the token, and no part of the
// token reaches the log — that is the whole reason it lives in its own file.
func (b *Broker) markRevoked(status int) {
	b.mu.Lock()
	already := b.revoked
	b.revoked = true
	b.mu.Unlock()
	if already {
		return
	}
	b.log.Error("the control plane rejected this machine's token; remote operation is stopped until a new token is written and the agent restarts",
		"url", b.url,
		"status", status,
		"token_file", b.tokenFile,
		"fix", "latem register --url "+b.url+" --token-file <new-token>",
		"note", "local operation is unaffected: `latem run` and every configured runner keep working")
}

// runSession runs one connection to its end and reports whether it got far
// enough to be called a working link.
//
// The reader starts before the resume frame is sent, because the
// acknowledgements for the replay and the log flush arrive on it. Everything
// after that point is ordered on purpose: resume first so the server knows what
// this machine holds, then health so it knows what the machine can do, then the
// transitions it missed, then the log frames covering the outage.
//
// The reader ends the session itself rather than leaving that to this function,
// and the difference is not cosmetic. The replay above waits for
// acknowledgements, so a link that dies during it would otherwise leave those
// waiters with nothing to wake them — the session channel is closed by
// [Broker.closeSession], which would not run until the waiting finished. The
// symptom was a partition costing a full request timeout per outstanding
// request instead of failing in milliseconds, on exactly the path a partition
// is most likely to interrupt.
func (b *Broker) runSession(ctx context.Context, conn *websocket.Conn) bool {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	b.openSession(conn)
	defer b.closeSession(conn)

	readDone := make(chan error, 1)
	go func() {
		err := b.readLoop(sctx, conn)
		b.closeSession(conn)
		readDone <- err
	}()

	ok := true
	if err := b.onConnect(sctx); err != nil {
		if !b.stopping() {
			b.log.Warn("resuming with the control plane failed", "url", b.url, "err", err)
		}
		_ = conn.CloseNow()
		ok = false
	}

	err := <-readDone
	if ok && !b.stopping() {
		b.log.Warn("control plane connection lost", "url", b.url, "err", err)
	}
	return ok
}

// onConnect is everything that must happen before this link is ordinary.
func (b *Broker) onConnect(ctx context.Context) error {
	if err := b.sendResume(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	h, has := b.health, b.hasHB
	b.mu.Unlock()

	b.log.Info("connected to the control plane", "url", b.url, "machine_id", b.MachineID())

	if has {
		// Publishing the last known health immediately means a machine that was
		// away for ten minutes is current the moment it returns, rather than
		// looking idle until the next fifteen-second beat.
		h = b.healthForPeer(h)
		if err := b.send(ctx, frame{Kind: kindHeartbeat, Heartbeat: &h}); err != nil {
			return err
		}
	}

	b.replayReports(ctx)
	b.flushSinks(ctx)
	return nil
}

// sendResume is finding F6 made mechanical.
//
// It carries `{run_id, state, last_acked_seq}` for every non-terminal run this
// machine owns, so the server can replay the commands that were missed and pick
// log ingestion back up at last_acked_seq+1.
//
// Every number in it is this transport's own. The store is read under
// [store.BrokerRemote] (amendment A28) and the live sink is preferred over it,
// and both halves of that are load-bearing:
//
//   - The key is what keeps the local broker out of the answer. It advances its
//     own resume point to the highest sequence it has *persisted* the moment a
//     batch is written, which is the honest local meaning and is arbitrarily far
//     ahead of any network. While the two shared one column, reading it here
//     claimed the control plane already held frames it had never seen, and the
//     frames lost were exactly the ones covering the outage.
//   - The live sink is preferred because persisting is best effort: an ack the
//     sink has taken but not yet written down is still an ack, and reporting the
//     lower number would re-send frames the server has. The store's value is
//     used for runs this process has no sink for, which is the case it is right
//     for: a run that survived an agent restart.
func (b *Broker) sendResume(ctx context.Context) error {
	runs, err := b.st.NonTerminal(ctx)
	if err != nil {
		// Not fatal to the connection. A resume frame listing nothing is worse
		// than one listing everything, but it is far better than no link at all:
		// frames carry their own sequence numbers, so ingestion is unambiguous
		// even when the server has to work out the resume point itself.
		b.log.Warn("could not read non-terminal runs for the resume frame", "err", err)
		runs = nil
	}

	r := protocol.Resume{MachineID: b.MachineID(), Runs: make([]protocol.RunResume, 0, len(runs))}
	for _, run := range runs {
		if run.IsLocal() {
			continue // latem try's, which the control plane never sent (A76)
		}
		var seq uint64
		if s := b.sinkFor(run.ID); s != nil {
			seq = s.Acked()
		} else if stored, serr := b.st.LastAckedSeq(ctx, store.BrokerRemote, run.ID); serr != nil {
			// Zero is the safe direction: the server re-sends from 1 and its
			// (run_id, seq) dedupe discards what it already has. Claiming a
			// resume point this transport could not read would lose frames.
			b.log.Warn("could not read the log resume point; resuming this run from the start",
				"run_id", run.ID, "err", serr)
		} else {
			seq = stored
		}
		r.Runs = append(r.Runs, protocol.RunResume{
			RunID:        run.ID,
			State:        run.State,
			LastAckedSeq: seq,
			// Not held back from an older control plane: resume goes before
			// its hello says what it speaks, and an older one reads the
			// fields it knows and leaves the rest.
			NeedsTokens: b.lostTokens(run),
		})
	}
	return b.send(ctx, frame{Kind: kindResume, Resume: &r})
}

// lostTokens reports a workflow job from a control plane that holds neither
// of the tokens it came with: one that arrived before this process started,
// since tokens live in memory only (A52, A72). Queued or just claimed, so that
// the `run` sent again in answer only hands it fresh ones (A75): a job past
// claimed has fetched already, and asking for it again would be asking to
// start it twice, which the control plane would rightly refuse.
func (b *Broker) lostTokens(run store.Run) bool {
	if run.Workflow == nil || run.OriginKind == "cli" {
		return false
	}
	if run.State != protocol.StateQueued && run.State != protocol.StateClaimed {
		return false
	}
	return b.st.Token(run.ID) == "" && b.st.FetchToken(run.ID) == ""
}

// replayReports re-sends the transitions this transport failed to deliver.
//
// Two sources, in order. First this transport's own memory of what it could not
// send, which is the only thing that knows a run finished *during* the
// partition. Then store.Unreported under [store.BrokerRemote], the restart-safe
// backstop that survives the process.
//
// The key is what makes the second source work at all (amendment A28). While
// reported_state was one column for the machine, the local broker marked every
// transition the instant it was persisted, so this query came back empty on
// every machine that had a local broker — which is every machine — and a run
// that finished while the process was down was never reported to anybody.
//
// Both are idempotent by (run id, state) per §7.2, so overlap costs a round
// trip and nothing else.
func (b *Broker) replayReports(ctx context.Context) {
	for runID, updates := range b.takePending() {
		for _, u := range updates {
			if err := b.reportOnce(ctx, runID, u); err != nil {
				// Put the rest back: a link that died again mid-replay must not
				// eat the transitions it had not reached yet.
				b.rememberReport(runID, u)
			}
		}
	}

	rows, err := b.st.Unreported(ctx, store.BrokerRemote)
	if err != nil {
		b.log.Warn("could not read unreported transitions", "err", err)
		return
	}
	for _, run := range rows {
		if run.IsLocal() {
			// Report marks these delivered as they happen (A76); one here is
			// from an agent that predates that, and is marked now.
			b.markReported(ctx, run.ID, run.State)
			continue
		}
		u := protocol.Update{
			State:         run.State,
			ExitCode:      run.ExitCode,
			ErrorCode:     run.ErrorCode,
			ErrorMsg:      run.ErrorMsg,
			StartedAt:     run.StartedAt,
			FinishedAt:    run.FinishedAt,
			Runner:        run.Runner,
			CancelReason:  run.CancelReason,
			RefNormalized: run.RefNormalized,
			SHA:           run.SHA,
		}
		if err := b.reportOnce(ctx, run.ID, u); err != nil {
			b.log.Debug("replaying a transition failed", "run_id", run.ID, "state", run.State, "err", err)
			return
		}
	}
}

// reportOnce publishes one update without the memory that [Broker.Report] keeps.
//
// It does record the delivery in broker_state, because that record is what
// stops the run coming back out of store.Unreported on the next connection. A
// refusal is not a delivery and is deliberately not marked: the server declined
// this state, and pretending it took it would hide the transition from the
// replay forever.
func (b *Broker) reportOnce(ctx context.Context, runID string, u protocol.Update) error {
	ack, err := b.request(ctx, frame{Kind: kindUpdate, Update: &updateFrame{RunID: runID, Update: u}})
	if err != nil {
		return err
	}
	if !ack.OK {
		return fmt.Errorf("remote: replaying %s %s refused: %s", runID, u.State, ackReason(ack))
	}
	b.markReported(ctx, runID, u.State)
	return nil
}

// flushSinks ships whatever every live run still owes the control plane.
func (b *Broker) flushSinks(ctx context.Context) {
	for _, s := range b.liveSinks() {
		s.flush(ctx)
	}
}

// readLoop dispatches inbound messages until the connection dies.
func (b *Broker) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			// The wire is JSON by specification. A binary message is either a
			// different protocol or a probe, and answering either is worse than
			// saying so and moving on.
			b.log.Debug("ignoring a non-text message from the control plane", "type", typ.String())
			continue
		}

		f, derr := decode(data)
		if derr != nil {
			// One bad message is skippable here and is not skippable on the
			// local socket, and the difference is the framing rather than the
			// strictness. Newline-delimited JSON that overruns its bound leaves
			// the reader with no idea where the next frame starts, so the local
			// broker answers once and closes. WebSocket messages carry their own
			// boundaries, so the next one is still intelligible and dropping the
			// connection would cost a build's log for a stray field.
			b.log.Warn("refusing a malformed frame from the control plane", "err", derr)
			continue
		}
		b.dispatch(ctx, f)
	}
}

// dispatch routes one decoded frame.
func (b *Broker) dispatch(ctx context.Context, f frame) {
	switch f.Kind {
	case kindCommand:
		b.handleCommand(ctx, f.ID, *f.Command)
	case kindAck:
		b.resolve(f.ID, *f.Ack)
	case kindHello:
		b.handleHello(*f.Hello)
	}
}

// handleHello records what the control plane calls this machine.
func (b *Broker) handleHello(h helloFrame) {
	if h.Version != "" && h.Version != protocol.Version {
		b.log.Warn("the control plane speaks a different protocol version",
			"theirs", h.Version, "ours", protocol.Version)
	}
	b.mu.Lock()
	b.peerVersion = h.Version
	b.mu.Unlock()
	if h.MachineID == "" {
		return
	}
	b.mu.Lock()
	changed := b.machineID != h.MachineID && b.machineID != ""
	b.machineID = h.MachineID
	b.mu.Unlock()
	if changed {
		b.log.Warn("the control plane assigned a different machine id; `latem register` writes it into config.toml",
			"machine_id", h.MachineID)
	}
}

// handleCommand makes an inbound run durable, answers, and then rings the
// doorbell.
//
// The order is the local broker's, for the local broker's reason: once the row
// is in the queue the run will happen — by this agent in a moment, or by the
// next agent to start if this one dies between the two statements. So the
// control plane is told "accepted" as soon as that is true, and delivery to the
// agent is a wake-up whose failure costs latency rather than a build.
//
// It is idempotent by run id, which is what makes the server's replay after a
// resume safe: a run already queued is re-queued, and a run that already
// finished is refused rather than resurrected under its old identifier.
func (b *Broker) handleCommand(ctx context.Context, id string, cmd protocol.Command) {
	switch cmd.Kind {
	case protocol.CommandRun:
		var expires time.Time
		if b.queueTTL > 0 {
			expires = b.now().Add(b.queueTTL)
		}
		if err := enqueueRun(ctx, b.st, *cmd.Run, 0, expires); err != nil {
			b.reply(ctx, id, protocol.Ack{RunID: cmd.Run.ID, Error: err.Error()})
			return
		}
		b.reply(ctx, id, protocol.Ack{OK: true, RunID: cmd.Run.ID})
		if !b.deliver(cmd) {
			b.log.Warn("run queued but not delivered to the agent; the runner pool will claim it from the queue",
				"run_id", cmd.Run.ID)
		}

	case protocol.CommandCancel:
		// A cancel is durable nowhere. Nothing replays it, no table remembers
		// it, and the only thing that can act on it is the goroutine holding the
		// run's context — so delivery is the answer, and it is reported after
		// the fact rather than before it.
		if !b.deliver(cmd) {
			b.reply(ctx, id, protocol.Ack{
				RunID: cmd.Cancel.ID,
				Error: "the agent is not reading commands; the cancel was not delivered",
			})
			return
		}
		b.reply(ctx, id, protocol.Ack{OK: true, RunID: cmd.Cancel.ID})

	case protocol.CommandPlan:
		// A plan is durable nowhere either (A52): "ok" means the agent has
		// the question, and the answer follows as a plan frame of its own,
		// because fetching and reading a repository takes longer than an
		// acknowledgement should.
		if !b.deliver(cmd) {
			b.reply(ctx, id, protocol.Ack{
				RunID: cmd.Plan.ID,
				Error: "the agent is not reading commands; the plan was not delivered",
			})
			return
		}
		b.reply(ctx, id, protocol.Ack{OK: true, RunID: cmd.Plan.ID})
	}
}

// deliver hands a command to whoever is consuming [Broker.Commands].
//
// It never blocks. The buffer is the bound: a full one means the agent is not
// draining commands, and waiting here would stall the reader — and with it
// every log acknowledgement for every other run on the machine.
func (b *Broker) deliver(cmd protocol.Command) bool {
	select {
	case b.cmds <- cmd:
		return true
	default:
		return false
	}
}

// reply answers an inbound command. A frame that carried no id gets no answer,
// because there is nothing to correlate it with.
func (b *Broker) reply(ctx context.Context, id string, ack protocol.Ack) {
	if id == "" {
		return
	}
	if err := b.send(ctx, frame{Kind: kindAck, ID: id, Ack: &ack}); err != nil {
		b.log.Debug("could not answer the control plane", "err", err)
	}
}

// send writes one frame on the current connection.
func (b *Broker) send(ctx context.Context, f frame) error {
	data, err := encode(f)
	if err != nil {
		return err
	}

	b.mu.Lock()
	conn, revoked := b.conn, b.revoked
	b.mu.Unlock()
	switch {
	case revoked:
		return ErrRevoked
	case conn == nil:
		return ErrDisconnected
	}

	wctx, cancel := context.WithTimeout(ctx, b.reqTimeout)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("remote: writing %s frame: %w", f.Kind, err)
	}
	return nil
}

// request sends a frame and waits for its acknowledgement.
//
// It selects on the session as well as on the timeout, so a partition fails the
// call in milliseconds rather than holding a reporting goroutine for the full
// request timeout — which, multiplied by internal/run's five retries, is what
// would turn a brief outage into two and a half minutes of a run not being
// reported.
func (b *Broker) request(ctx context.Context, f frame) (protocol.Ack, error) {
	ch := make(chan protocol.Ack, 1)
	id, session, err := b.register(&waiter{ch: ch})
	if err != nil {
		return protocol.Ack{}, err
	}
	defer b.forget(id)

	f.ID = id
	if err := b.send(ctx, f); err != nil {
		return protocol.Ack{}, err
	}

	timer := time.NewTimer(b.reqTimeout)
	defer timer.Stop()
	select {
	case ack := <-ch:
		return ack, nil
	case <-session:
		return protocol.Ack{}, ErrDisconnected
	case <-timer.C:
		return protocol.Ack{}, fmt.Errorf("remote: the control plane did not answer a %s frame within %s", f.Kind, b.reqTimeout)
	case <-ctx.Done():
		return protocol.Ack{}, ctx.Err()
	case <-b.done:
		return protocol.Ack{}, errors.New("remote: broker closed")
	}
}

// register allocates a correlation id and records a waiter against the current
// session.
func (b *Broker) register(w *waiter) (string, <-chan struct{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.closed:
		return "", nil, errors.New("remote: broker closed")
	case b.revoked:
		return "", nil, ErrRevoked
	case b.conn == nil:
		return "", nil, ErrDisconnected
	}
	b.nextID++
	id := strconv.FormatUint(b.nextID, 36)
	b.waiters[id] = w
	return id, b.session, nil
}

func (b *Broker) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiters, id)
}

// resolve delivers an acknowledgement to whatever was waiting for it.
func (b *Broker) resolve(id string, ack protocol.Ack) {
	if id == "" {
		return
	}
	b.mu.Lock()
	w := b.waiters[id]
	delete(b.waiters, id)
	b.mu.Unlock()
	if w == nil {
		// An ack for a request that timed out, or one the far side invented.
		// Neither is worth more than a debug line: the frames it covered are
		// still in the sink's buffer and will be offered again.
		return
	}

	switch {
	case w.ch != nil:
		select {
		case w.ch <- ack:
		default:
		}
	case w.runID != "":
		if s := b.sinkFor(w.runID); s != nil {
			s.ack(w.highSeq, ack)
		}
	}
}

// openSession publishes a new connection and the channel that marks its life.
func (b *Broker) openSession(conn *websocket.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conn = conn
	b.session = make(chan struct{})
}

// closeSession drops the connection and releases every waiter on it.
//
// Waiters are released rather than left to time out because a report that has
// already lost its connection has nothing to gain from waiting thirty seconds
// to be told so, and because the run it belongs to may be finishing.
//
// Idempotent. It is called by the reader the moment the link dies and again by
// [Broker.runSession] on the way out, and a second call must not disturb a
// session that has since been opened by the next connection — which is why the
// conn is compared rather than assumed.
func (b *Broker) closeSession(conn *websocket.Conn) {
	b.mu.Lock()
	if b.conn != conn {
		// Already retired, or already replaced by a newer session. Either way
		// this call has nothing left to do, and clearing the waiters would
		// strand the requests belonging to the connection that is now live.
		b.mu.Unlock()
		_ = conn.CloseNow()
		return
	}
	b.conn = nil
	session := b.session
	b.session = closedChan()
	b.waiters = map[string]*waiter{}
	sinks := make([]*sink, 0, len(b.sinks))
	for _, s := range b.sinks {
		sinks = append(sinks, s)
	}
	b.mu.Unlock()

	// Every batch that was written but never answered goes back on offer. This
	// is the other half of finding F6: without it the frames covering the outage
	// would be marked sent, never acknowledged, and never seen again.
	for _, s := range sinks {
		s.resetSession()
	}

	select {
	case <-session:
	default:
		close(session)
	}
	_ = conn.CloseNow()
}

// stopping reports whether Close has been called, so shutdown does not narrate
// its own teardown as a series of failures.
func (b *Broker) stopping() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}
