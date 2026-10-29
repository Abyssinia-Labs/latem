// Package remote is the [broker.Broker] that reaches a control plane.
//
// It is a WebSocket client to the URL in `[broker.remote] url`, authenticated
// with `Authorization: Bearer <machine-token>` read from `token_file`
// (RUNNER_SPEC §7.4). It reconnects on its own, resumes without duplicating
// log output, and never becomes a reason the machine stops building.
//
// # INV-1 is why this is a client
//
// The agent only makes outbound connections. This package dials; it never
// listens, binds a port, or accepts anything. That is what makes the product
// installable on a Mac behind a domestic NAT with no router
// configuration, no inbound firewall rule, and no conversation with anybody's
// network team. TestINV1_RemoteBrokerBindsNothing asserts it by enumerating
// the process's own sockets while a broker is connected.
//
// # The resume frame is the replay mechanism — and that differs from local
//
// This is the single most important thing to know when reading the agent,
// because both transports are consumed through one interface and the
// difference is invisible at the call site.
//
//   - internal/broker/local deliberately does NOT replay. sqlite is the truth
//     and its socket is only a doorbell, so a run enqueued while the agent was
//     down is picked up by store.ClaimNext and never arrives as a command.
//   - Here there is no shared database. The control plane holds work this
//     machine has not seen, so on every connect the agent sends
//     [protocol.Resume] — `{run_id, state, last_acked_seq}` for every
//     non-terminal run it owns — and the server replays the commands that were
//     missed and picks log ingestion back up at `last_acked_seq + 1`.
//
// So [Broker.Commands] on this transport does yield a backlog, and every
// handler must be idempotent by run id. The two places that matters are
// [enqueueRun], which re-queues an existing non-terminal run rather than
// failing, and cancel, which §7.1 already defines as a no-op for an unknown,
// unclaimed, or terminal id.
//
// # A revoked token is a product property, not an error path
//
// On HTTP 401 or 403 the reconnect loop stops, logs loudly, and stays stopped.
// It does not return an error to the agent, does not close [Broker.Commands],
// and above all does not prevent [Open] from succeeding: the local broker keeps
// serving and the machine keeps building. A hosted layer that can switch off
// somebody's build machine by revoking a token is a hosted layer nobody should
// install.
//
// For the same reason [Open] never blocks on the network. It returns as soon as
// the transport exists, and the first connection happens in the background —
// so an unreachable control plane costs the agent no startup time at all.
//
// # Contract defects recorded here
//
// Three, none of them fixed in pkg/protocol, which is frozen:
//
//   - There is no envelope type. A WebSocket carries six kinds of message in
//     two directions and [protocol.Command] is only one of them, so this
//     package defines [frame]. It is unexported for the same reason the local
//     broker's `reply` was: nothing outside may mistake it for protocol. See
//     the [frame] comment.
//   - [protocol.Update] carries no run id. [broker.Broker.Report] takes the id
//     as a separate argument, so a transport that puts an update on a wire has
//     to pair them itself. [protocol.LogFrame] has a RunID field; Update does
//     not.
//   - [protocol.Ack] has no correlation identifier and no sequence number
//     (amendment A18 added the type, not a request/response discipline). Both
//     live on the envelope instead, which is why [frame.ID] exists.
//
// A fourth was recorded here when this package was written and has since been
// fixed: §22 gave `reported_state` and `last_acked_seq` one column each for the
// whole machine, and the local broker's meaning of both won. Amendment A28
// replaced them with the `broker_state` table, keyed by transport, which is what
// lets this package run beside the local one. Every store call here passes
// [store.BrokerRemote]; nothing in this package reads a number the local
// transport wrote.
package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// LogMode is `[agent] remote_logs` (§7.4). internal/config declares the same
// two values; they are restated here so this package does not depend on the
// config loader, exactly as internal/broker/local takes a socket path rather
// than a *config.Config.
type LogMode string

const (
	// LogsAlways ships frames as they are produced. It is the default.
	LogsAlways LogMode = "always"
	// LogsOnFailure buffers a run's output and ships it only if the run reaches
	// a failing terminal state. It exists for verbose Gradle builds and trades
	// live viewing for bandwidth.
	LogsOnFailure LogMode = "on_failure"
)

// Defaults for the knobs in [Options].
const (
	// DefaultCommandBuffer bounds how many inbound commands may wait for the
	// agent to read them.
	//
	// Unlike the local broker's buffer this one has no durable queue behind it
	// for cancels, so it is the only thing standing between a slow consumer and
	// a dropped Stop button. It is still small: a full buffer means the agent is
	// not draining commands at all, and blocking the reader to wait would also
	// stall the log acknowledgements every other run is waiting on.
	DefaultCommandBuffer = 64

	// DefaultRequestTimeout bounds one round trip that waits for an
	// acknowledgement — a claim or a report. Beyond this the far side is not
	// answering, and the caller has a durable copy either way.
	DefaultRequestTimeout = 30 * time.Second

	// DefaultHandshakeTimeout bounds the HTTP upgrade. It is short because a
	// control plane that has not answered in ten seconds is one the backoff
	// loop should be waiting on rather than one this dial should keep holding.
	DefaultHandshakeTimeout = 10 * time.Second

	// DefaultMaxBufferBytes is §7.4's per-run log buffer bound: 50 MB. Beyond
	// it frames are dropped and one marker records how many, so the gap is
	// visible rather than silent. See [sink].
	DefaultMaxBufferBytes int64 = 50 << 20
)

// ErrRevoked is returned by operations attempted after the control plane
// refused the machine token. It is terminal: nothing this process does will
// clear it, and rotating the token means writing a new one and restarting.
var ErrRevoked = errors.New("remote: the control plane rejected the machine token")

// ErrDisconnected means there is no live connection right now. It is an
// ordinary condition — the reconnect loop is already working on it — and
// callers should treat it as "not yet", not as a failure of the run.
var ErrDisconnected = errors.New("remote: not connected to the control plane")

// Options configures a remote broker.
type Options struct {
	// URL is the control plane's agent endpoint, wss:// in anything but a test.
	// The scheme is validated by the config loader, which rejects plain ws:// to
	// a non-loopback host because the machine token rides in a header.
	URL string

	// TokenFile holds the machine token, mode 0600. It is a path rather than a
	// value because it is re-read on every dial: rotating a token with
	// `latem register` then takes effect at the next reconnect rather than at
	// the next restart, which is what internal/config's reloader promises.
	TokenFile string

	// MachineID is `[machine] id`. It may be empty on a machine that has
	// connected but never completed registration; the control plane assigns one
	// and this package records what it was told (see [Broker.MachineID]).
	MachineID string

	// Store is required. The resume frame is built from store.NonTerminal, and
	// an inbound run is made durable before it is acknowledged — the same
	// property §7.3 gives the local socket, for the same reason: a control plane
	// told "accepted" for a run that evaporates on the next restart was lied to.
	Store Store

	// QueueTTL sets expires_at on a run accepted from the control plane (§11,
	// §22). Zero means no deadline. It is configuration, so it lives here and
	// not on the wire (INV-2).
	QueueTTL time.Duration

	// LogMode is `[agent] remote_logs`. Empty means [LogsAlways].
	LogMode LogMode

	// MaxBufferBytes overrides [DefaultMaxBufferBytes].
	MaxBufferBytes int64

	// Backoff is the reconnect schedule. The zero value is [DefaultBackoff].
	Backoff Backoff

	// CommandBuffer, RequestTimeout, HandshakeTimeout default to the constants
	// above.
	CommandBuffer    int
	RequestTimeout   time.Duration
	HandshakeTimeout time.Duration

	// Logger receives the transport's narration. Nil discards it. Unlike the
	// local broker, whose failures are mostly a client hanging up, this one's
	// failures are the ones an operator most wants in the log: a partition and
	// a revoked token both look like "builds are not appearing in the web app".
	Logger *slog.Logger

	// sleep waits for d and reports whether the wait completed. It is the only
	// clock in the reconnect loop, and it is injectable so a test can simulate a
	// ten-minute partition in microseconds — see [Backoff] and helpers_test.go.
	sleep func(ctx context.Context, d time.Duration) bool

	// jitter returns a value in [0,1) for [Backoff.Delay]. Injectable so the
	// schedule can be asserted exactly rather than probabilistically.
	jitter func() float64

	// now is the clock for anything that is not a delay.
	now func() time.Time
}

// Broker is the WebSocket transport. It satisfies [broker.Broker].
type Broker struct {
	url        string
	tokenFile  string
	st         Store
	queueTTL   time.Duration
	mode       LogMode
	maxBuffer  int64
	backoff    Backoff
	reqTimeout time.Duration
	handshake  time.Duration
	log        *slog.Logger
	sleep      func(ctx context.Context, d time.Duration) bool
	jitter     func() float64
	now        func() time.Time

	cmds chan protocol.Command

	// done is closed by Close and is what unblocks every goroutine here.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	cancel    context.CancelFunc

	mu        sync.Mutex
	closed    bool
	consumed  bool
	revoked   bool
	machineID string
	conn      *websocket.Conn
	// session is closed when the current connection ends. Waiters select on it
	// so a partition unblocks them instead of holding them for RequestTimeout.
	session chan struct{}
	nextID  uint64
	waiters map[string]*waiter
	sinks   map[string]*sink
	// pending holds transitions this transport has not delivered, per run, in
	// order. See [Broker.replayReports].
	pending map[string][]protocol.Update
	health  protocol.MachineHealth
	hasHB   bool
}

var _ broker.Broker = (*Broker)(nil)

// Open creates the transport and starts connecting in the background.
//
// It does not wait for a connection and it does not fail because there is not
// one. §6 starts brokers at step 7 of a startup sequence that must finish
// whether or not a control plane is reachable, and an agent that refused to
// start because api.example.com was down would be an agent that turns an
// outage somewhere else into an outage here.
func Open(opts Options) (*Broker, error) {
	switch {
	case opts.URL == "":
		return nil, errors.New("remote: Open requires a url; it is `[broker.remote] url` in config.toml (RUNNER_SPEC §7.4)")
	case opts.TokenFile == "":
		return nil, errors.New("remote: Open requires a token_file: the machine token is a path, never an inline value (RUNNER_SPEC §8)")
	case opts.Store == nil:
		return nil, errors.New("remote: Open requires a store: the resume frame is built from store.NonTerminal, and an inbound run is durable before it is acknowledged")
	}

	switch opts.LogMode {
	case "":
		opts.LogMode = LogsAlways
	case LogsAlways, LogsOnFailure:
	default:
		return nil, fmt.Errorf("remote: agent.remote_logs is %q; it must be %q or %q (RUNNER_SPEC §7.4)",
			opts.LogMode, LogsAlways, LogsOnFailure)
	}

	if opts.CommandBuffer <= 0 {
		opts.CommandBuffer = DefaultCommandBuffer
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if opts.MaxBufferBytes <= 0 {
		opts.MaxBufferBytes = DefaultMaxBufferBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.sleep == nil {
		opts.sleep = realSleep
	}
	if opts.jitter == nil {
		opts.jitter = randomJitter
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	b := &Broker{
		url:        opts.URL,
		tokenFile:  opts.TokenFile,
		st:         opts.Store,
		queueTTL:   opts.QueueTTL,
		mode:       opts.LogMode,
		maxBuffer:  opts.MaxBufferBytes,
		backoff:    opts.Backoff.orDefault(),
		reqTimeout: opts.RequestTimeout,
		handshake:  opts.HandshakeTimeout,
		log:        opts.Logger,
		sleep:      opts.sleep,
		jitter:     opts.jitter,
		now:        opts.now,
		cmds:       make(chan protocol.Command, opts.CommandBuffer),
		done:       make(chan struct{}),
		machineID:  opts.MachineID,
		session:    closedChan(),
		waiters:    map[string]*waiter{},
		sinks:      map[string]*sink{},
		pending:    map[string][]protocol.Update{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.supervise(ctx)
	}()
	return b, nil
}

// Close stops reconnecting, drops the connection, and releases every goroutine.
//
// Idempotent, per amendment A19: graceful shutdown and a deferred close both
// run on the normal path, and a second Close reporting a failure would mask
// the first error that mattered.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		close(b.done)
		conn := b.conn
		b.conn = nil
		b.mu.Unlock()

		b.cancel()
		if conn != nil {
			_ = conn.CloseNow()
		}
		b.wg.Wait()
	})
	return nil
}

// Connected reports whether a session is live right now.
//
// Not part of [broker.Broker]: it is what the `broker_up{broker="remote"}`
// gauge in §20 publishes, and what `latem doctor` would ask.
func (b *Broker) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}

// Revoked reports whether the control plane refused the machine token. A
// revoked broker has stopped reconnecting and will not resume without a new
// token and a restart.
func (b *Broker) Revoked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.revoked
}

// MachineID is the identifier the control plane last claimed for this machine,
// or the configured one if it has never said. `latem register` persists it into
// `[machine] id`.
func (b *Broker) MachineID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.machineID
}

// Commands yields commands from the control plane, including the backlog the
// server replays in answer to the resume frame.
//
// One consumer at a time, for the same reason as the local broker: two
// consumers would split the stream between them and a cancel would reach
// whichever half was not executing the run.
//
// The channel closes on ctx cancellation or on [Broker.Close], and on nothing
// else. A partition, a revoked token, and a control plane that has never been
// reachable all leave it open and empty, because the interface says a dropped
// connection is not an error the caller sees — and because an agent whose
// command loop exited when the network blipped would need a restart to notice
// the network came back.
func (b *Broker) Commands(ctx context.Context) (<-chan protocol.Command, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("remote: Commands on a closed broker")
	}
	if b.consumed {
		b.mu.Unlock()
		return nil, errors.New("remote: Commands already has a consumer; the remote broker serves one agent")
	}
	b.consumed = true
	b.wg.Add(1)
	b.mu.Unlock()

	out := make(chan protocol.Command)
	go func() {
		defer b.wg.Done()
		defer close(out)
		defer func() {
			b.mu.Lock()
			b.consumed = false
			b.mu.Unlock()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case <-b.done:
				return
			case cmd := <-b.cmds:
				select {
				case out <- cmd:
				case <-ctx.Done():
					return
				case <-b.done:
					return
				}
			}
		}
	}()
	return out, nil
}

// Claim asks the control plane for exclusive ownership of a run.
//
// The atomicity the interface demands belongs to the far side here, not to
// this process: the control plane is the only thing that can see every machine
// racing for the same run, which is the whole reason the method exists. A
// refusal comes back as an [protocol.Ack] with OK false and is translated to
// [broker.ErrAlreadyClaimed] — an ordinary outcome, not a failure.
//
// A transport error is NOT a refusal. Losing the connection mid-claim means the
// answer is unknown, and reporting "somebody else has it" for "I could not ask"
// would silently idle a machine that could have taken the work.
func (b *Broker) Claim(ctx context.Context, runID string) error {
	if err := protocol.ValidateID(runID); err != nil {
		return fmt.Errorf("remote: Claim: %w", err)
	}
	ack, err := b.request(ctx, frame{Kind: kindClaim, Claim: &claimRequest{RunID: runID}})
	if err != nil {
		return fmt.Errorf("remote: Claim %s: %w", runID, err)
	}
	if !ack.OK {
		return fmt.Errorf("%w: %s", broker.ErrAlreadyClaimed, ackReason(ack))
	}
	return nil
}

// Report publishes a state transition, and remembers it if it could not be
// delivered.
//
// The remembering is the point, and it is what makes "reports the correct
// terminal state on reconnect" true rather than aspirational. A run that
// finishes during a partition cannot be reported when it finishes; the
// transition is durable in sqlite, and this transport keeps its own copy so
// that the reconnect replays it in order — see [Broker.replayReports].
//
// A delivered update is marked reported under [store.BrokerRemote], which is
// what takes the run out of this transport's replay set across a restart.
// Before amendment A28 there was one reported_state for the machine and the
// local broker set it the moment a transition was persisted, so this transport
// could neither record a delivery nor discover an outstanding one.
//
// Updates are idempotent by (run id, state) per §7.2, so replaying one the
// server already has is free.
func (b *Broker) Report(ctx context.Context, runID string, u protocol.Update) error {
	if err := protocol.ValidateID(runID); err != nil {
		return fmt.Errorf("remote: Report: %w", err)
	}
	if !u.State.Valid() {
		return fmt.Errorf("remote: Report %s: unknown state %q", runID, u.State)
	}

	// A terminal state decides what happens to an on_failure buffer, and it has
	// to decide it whether or not the report reaches the far side: the run is
	// over either way, and a buffer nobody resolves is 50 MB held until
	// shutdown.
	if u.State.IsTerminal() {
		b.resolveSink(ctx, runID, u.State)
	}

	ack, err := b.request(ctx, frame{Kind: kindUpdate, Update: &updateFrame{RunID: runID, Update: u}})
	switch {
	case err != nil:
		b.rememberReport(runID, u)
		return fmt.Errorf("remote: Report %s %s: %w", runID, u.State, err)
	case !ack.OK:
		// A refusal is the control plane's answer, not a delivery failure, so it
		// is not replayed: replaying something the far side declined would loop
		// on every reconnect for the life of the run.
		return fmt.Errorf("remote: Report %s %s refused: %s", runID, u.State, ackReason(ack))
	}
	b.forgetReport(runID, u.State)
	b.markReported(ctx, runID, u.State)
	return nil
}

// ReportPlan sends a plan's answer and waits for the control plane to accept
// it (A52). Nothing remembers it on failure: a plan is cheap to make again,
// and the control plane, which is waiting for it, is the one that knows to.
func (b *Broker) ReportPlan(ctx context.Context, p protocol.PlanResult) error {
	if err := protocol.ValidateID(p.ID); err != nil {
		return fmt.Errorf("remote: ReportPlan: %w", err)
	}
	ack, err := b.request(ctx, frame{Kind: kindPlan, Plan: &p})
	switch {
	case err != nil:
		return fmt.Errorf("remote: ReportPlan %s: %w", p.ID, err)
	case !ack.OK:
		return fmt.Errorf("remote: ReportPlan %s refused: %s", p.ID, ackReason(ack))
	}
	return nil
}

// markReported records a delivery in broker_state under this transport's key.
//
// Best effort on purpose. A failed write leaves the run in
// store.Unreported(remote) and the next connection replays a transition the
// server already has, which §7.2 makes free; refusing the Report because a
// bookkeeping row would not write would turn a successful delivery into a
// failure the caller retries.
func (b *Broker) markReported(ctx context.Context, runID string, state protocol.RunState) {
	if err := b.st.MarkReported(ctx, store.BrokerRemote, runID, state); err != nil {
		b.log.Debug("could not record a delivered transition",
			"run_id", runID, "state", state, "err", err)
	}
}

// Logs returns the sequenced sink for one run.
//
// The run is looked up first, so a sink for an id that does not exist fails
// here rather than on the first batch of a build that has already started
// producing output. That lookup is local — the store, not the network — because
// a control plane that is unreachable must not be able to stop a build from
// logging.
func (b *Broker) Logs(ctx context.Context, runID string) (broker.LogSink, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return nil, fmt.Errorf("remote: Logs: %w", err)
	}
	run, err := b.st.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	// This transport's own resume point, not the machine's (amendment A28). The
	// local broker is streaming the same run to the same disk and its row is
	// arbitrarily far ahead; starting here from that number would skip every
	// frame the control plane has not been sent.
	acked, err := b.st.LastAckedSeq(ctx, store.BrokerRemote, run.ID)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("remote: Logs on a closed broker")
	}
	if s, ok := b.sinks[runID]; ok {
		// A second sink for one run would be a second sequence of acks against
		// one resume point, and the lower of the two would win at the next
		// reconnect — losing exactly the frames covering the outage.
		return s, nil
	}
	s := newSink(b, run.ID, acked)
	b.sinks[runID] = s
	return s, nil
}

// Heartbeat publishes liveness and capacity (§20).
//
// It returns nil when there is nothing to send — no connection, or a revoked
// token — and that is deliberate. The agent beats every fifteen seconds and
// logs a warning for every error; during the ten-minute partition this package
// exists to survive that is forty identical lines burying the one line the
// reconnect loop already wrote about the outage. The health value is kept, so
// the next successful connection publishes the current state immediately
// rather than up to fifteen seconds later.
func (b *Broker) Heartbeat(ctx context.Context, h protocol.MachineHealth) error {
	b.mu.Lock()
	b.health = h
	b.hasHB = true
	b.mu.Unlock()

	err := b.send(ctx, frame{Kind: kindHeartbeat, Heartbeat: &h})
	if errors.Is(err, ErrDisconnected) || errors.Is(err, ErrRevoked) {
		return nil
	}
	return err
}

// resolveSink tells a run's sink what the run's outcome was.
//
// Under [LogsOnFailure] this is the decision the whole mode turns on: a failing
// terminal state flushes the buffer, a succeeded one discards it. Under
// [LogsAlways] the frames have already gone, and this only releases whatever is
// still unacknowledged once it drains.
func (b *Broker) resolveSink(ctx context.Context, runID string, state protocol.RunState) {
	b.mu.Lock()
	s := b.sinks[runID]
	b.mu.Unlock()
	if s == nil {
		return
	}
	s.decide(ctx, state)
}

// dropSink removes a sink that has nothing left to deliver. It is called by the
// sink itself, which is the only thing that knows when that is true.
func (b *Broker) dropSink(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sinks, runID)
}

// liveSinks returns the sinks that still hold frames, for the reconnect flush.
func (b *Broker) liveSinks() []*sink {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*sink, 0, len(b.sinks))
	for _, s := range b.sinks {
		out = append(out, s)
	}
	return out
}

// sinkFor returns the live sink for a run, or nil.
func (b *Broker) sinkFor(runID string) *sink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sinks[runID]
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// ackReason renders a refusal for a human without inventing text the far side
// did not send.
func ackReason(a protocol.Ack) string {
	switch {
	case a.Error != "" && a.Code != "":
		return fmt.Sprintf("%s (%s)", a.Error, a.Code)
	case a.Error != "":
		return a.Error
	case a.Code != "":
		return string(a.Code)
	default:
		return "the control plane refused without saying why"
	}
}

// authHeader builds the handshake headers. The token is read on every dial so a
// rotation takes effect at the next reconnect.
func (b *Broker) authHeader() (http.Header, error) {
	token, err := ReadToken(b.tokenFile)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Latem-Protocol-Version", protocol.Version)
	return h, nil
}
