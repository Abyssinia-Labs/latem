// Package local is the [broker.Broker] that a machine talks to itself over.
//
// It is a Unix domain socket at ~/.latem/agent.sock, mode 0600, carrying
// newline-delimited JSON [protocol.Command] frames, backed by the sqlite queue
// in RUNNER_SPEC §22. It is what makes `latem run` work on a laptop with no
// control plane, no account, and no network — which is the product's entire
// first impression.
//
// # INV-1 is this package's invariant
//
// The agent only makes outbound connections. There is no TCP listener on any
// non-loopback interface, no HTTP server, no port to forward, and no inbound
// firewall rule to explain to anybody. The whole inbound surface is one socket
// file inside a directory mode 0700, reachable only by the user running the
// agent. TestINV1_NoInboundListeners in this package enumerates the process's
// own listening sockets while a broker is up and asserts that every one of
// them is a Unix socket or bound to loopback; test/invariants holds the same
// claim as a skipped placeholder owned by another workstream.
//
// # Why sqlite is the truth and the socket is a doorbell
//
// The queue is not a buffer for a momentarily busy agent. §7.3 makes it the
// thing `latem run` writes to when no agent is running at all, and the two
// cases have to converge or the second one is a different product:
//
//   - Agent up. The client connects, the broker writes the run to sqlite,
//     answers with the run id, and hands the command to the agent, which
//     starts it now.
//   - Agent down. The connect fails, so the client writes the identical row to
//     sqlite itself, prints `queued (agent not running)` with the run id, and
//     exits 0. The next agent to start claims it from the queue at §6 step 6.
//
// Both paths go through one function ([enqueueRun]), so "the same state" is
// structural rather than a thing two pieces of code remember to agree on. The
// socket only ever carries a notification; everything durable happens in the
// database, and an agent that never receives the notification still runs the
// job.
//
// That has a consequence worth stating for whoever writes the agent
// supervisor: [Broker.Commands] does NOT replay the backlog. Runs enqueued
// while the agent was down are picked up by the runner pool's
// store.ClaimNext, exactly as §7.3 says ("the agent picks it up at step 6").
// An agent that waits for a command that will never arrive will sit next to a
// full queue doing nothing.
//
// # Cancel is not a special case
//
// `latem cancel` and a Stop button on a phone are the same [protocol.Command]
// arriving on two transports, and this package gives the local one no
// privileges the remote one lacks. The command set stays closed at two kinds
// (INV-2); there is no local-only verb for following logs or querying health,
// because adding one here would make the local transport a place where the
// closed set is negotiable.
//
// # Contract defect recorded here
//
// pkg/protocol has no acknowledgement type. [protocol.Command] is a one-way
// vocabulary, so a transport that wants to answer "accepted, run id r_123" or
// "refused, that run already finished" must invent a frame. This package
// defines an unexported [reply] for that and keeps it unexported so nothing
// outside can mistake it for protocol. See the [reply] comment.
package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/broker"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Defaults for the knobs in [Options].
const (
	// DefaultCommandBuffer is how many accepted commands may wait for the
	// agent to read them. It is small: the queue behind them is durable, so a
	// deep in-memory buffer buys nothing except a longer list of things lost
	// if the process dies.
	DefaultCommandBuffer = 64

	// DefaultTimeout bounds one read or one write on a client connection. A
	// client that opens the socket and then stops talking holds a goroutine
	// and a file descriptor until this expires.
	DefaultTimeout = 30 * time.Second

	// deliverTimeout bounds how long the socket handler waits for the agent to
	// take a command. For a run it is nearly irrelevant — the run is already
	// durably queued and the pool will claim it. For a cancel it is the whole
	// story, because a cancel is durable nowhere.
	deliverTimeout = 5 * time.Second
)

// Options configures a local broker.
type Options struct {
	// Socket is the path to bind. Empty is an error rather than a default,
	// because config.Paths already derives it and a second derivation here
	// would be a second place for --config-dir to be ignored.
	Socket string

	// Store is required. The broker owns no queue of its own; every claim,
	// transition, and enqueue is the store's.
	Store Store

	// QueueTTL sets expires_at on a run accepted over the socket (§11, §22).
	// Zero means no deadline. It lives here rather than on the wire because it
	// is the agent's configuration, and INV-2 keeps configuration off the
	// inbound path.
	QueueTTL time.Duration

	// CommandBuffer defaults to [DefaultCommandBuffer].
	CommandBuffer int

	// Timeout defaults to [DefaultTimeout].
	Timeout time.Duration

	// Logger defaults to a discarding logger. The local broker's failures are
	// mostly the far end hanging up, which is not worth a line by default.
	Logger *slog.Logger

	// now is injectable for tests that assert on a deadline.
	now func() time.Time

	// deliver overrides [deliverTimeout]. A test for the undeliverable-cancel
	// path would otherwise have to wait the real timeout to observe it.
	deliver time.Duration
}

// Broker is the local transport. It satisfies [broker.Broker].
type Broker struct {
	socket   string
	st       Store
	queueTTL time.Duration
	timeout  time.Duration
	deliver  time.Duration
	log      *slog.Logger
	now      func() time.Time

	ln   *net.UnixListener
	cmds chan protocol.Command

	// done is closed by Close and is what unblocks every goroutine this
	// package starts.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	consumed bool
	health   protocol.MachineHealth
	healthAt time.Time
}

var _ broker.Broker = (*Broker)(nil)

// Open binds the socket and starts accepting.
//
// Binding happens here rather than on the first [Broker.Commands] call so that
// a socket the agent cannot bind is a startup failure with a path in the
// message, at §6 step 7, rather than a silence discovered by the first person
// to run `latem run`.
//
// Accepting also starts here, and that is deliberate: a client whose command
// is durably queued gets its answer whether or not the agent is currently
// reading commands. The alternative — accept only while someone is consuming —
// makes `latem run` hang for exactly as long as the agent is busy, which is
// the failure §7.3's queue exists to make impossible.
func Open(opts Options) (*Broker, error) {
	if opts.Store == nil {
		return nil, errors.New("local: Open requires a store: the queue in RUNNER_SPEC §22 is where a run becomes durable")
	}
	if opts.CommandBuffer <= 0 {
		opts.CommandBuffer = DefaultCommandBuffer
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.deliver <= 0 {
		opts.deliver = deliverTimeout
	}

	ln, err := listen(opts.Socket)
	if err != nil {
		return nil, err
	}

	b := &Broker{
		socket:   opts.Socket,
		st:       opts.Store,
		queueTTL: opts.QueueTTL,
		timeout:  opts.Timeout,
		deliver:  opts.deliver,
		log:      opts.Logger,
		now:      opts.now,
		ln:       ln,
		cmds:     make(chan protocol.Command, opts.CommandBuffer),
		done:     make(chan struct{}),
	}

	b.wg.Add(1)
	go b.serve()
	return b, nil
}

// Socket returns the bound path.
func (b *Broker) Socket() string { return b.socket }

// Close stops accepting, waits for in-flight connections, and unlinks the
// socket.
//
// Unlinking is [net.UnixListener]'s own behaviour for a socket it created, and
// it is why a clean shutdown leaves nothing for the next start to probe. The
// probe in [listen] exists for the shutdowns that are not clean.
func (b *Broker) Close() error {
	var err error
	b.closeOnce.Do(func() {
		// closed is set under the same lock that [Broker.Commands] takes before
		// it adds to the wait group, so no goroutine can join the group after
		// the group is being waited on.
		b.mu.Lock()
		b.closed = true
		close(b.done)
		b.mu.Unlock()

		err = b.ln.Close()
		b.wg.Wait()
	})
	return err
}

// Commands yields commands arriving on the socket. The channel closes when ctx
// is cancelled, or when the broker is closed.
//
// One consumer at a time. A second call is refused rather than served, because
// two consumers would split the stream between them and a cancel would reach
// whichever half was not executing the run — a bug that would look like an
// intermittently ignored Stop button.
func (b *Broker) Commands(ctx context.Context) (<-chan protocol.Command, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("local: Commands on a closed broker")
	}
	if b.consumed {
		b.mu.Unlock()
		return nil, errors.New("local: Commands already has a consumer; the local broker serves one agent")
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

// Claim takes exclusive ownership of a run, returning [broker.ErrAlreadyClaimed]
// if another runner won.
//
// The atomicity the interface demands is the store's conditional UPDATE, not
// anything this package adds: one statement whose WHERE clause both selects
// the run and re-asserts that it is still queued. Two runner goroutines racing
// here are serialised by sqlite's single writer and the loser's clause no
// longer matches. Implementing a lock here instead would make the guarantee a
// property of one process, and the queue is shared with every `latem` command
// on the machine.
func (b *Broker) Claim(ctx context.Context, runID string) error {
	if err := protocol.ValidateID(runID); err != nil {
		return fmt.Errorf("local: Claim: %w", err)
	}
	err := b.st.Claim(ctx, runID)
	if errors.Is(err, store.ErrNotClaimable) {
		// Losing a claim is an ordinary outcome, and the interface says so.
		// The store's reason is kept in the chain so a caller debugging one
		// can still see which state the run was actually in.
		return fmt.Errorf("%w: %w", broker.ErrAlreadyClaimed, err)
	}
	return err
}

// Report persists a state transition and marks it acknowledged.
//
// For a remote broker "report" means send. Locally there is nobody to send to,
// so the honest local meaning is: make it durable, then record that this
// transport has seen it. The second half is not decoration —
// broker_state.reported_state is what store.Unreported keys on, and a local
// broker that never marked anything reported would leave every run in its own
// replay set forever.
//
// The mark is written under [store.BrokerLocal] and under nothing else
// (amendment A28). "Persisted here" is not "accepted by a control plane", and
// while the two shared one column this call silently satisfied the remote
// transport's replay condition as well — so a run that finished during an
// outage was never sent to anybody.
//
// The transition is applied, but only while the run is still in the state the
// update describes. RUNNER_SPEC §11 has the agent persist before it reports,
// which makes this a self-transition — legal and idempotent by construction
// (§7.2). That was the reasoning, and the word doing too much work in it was
// "construction": reports are queued and delivered on a goroutine, so between
// the agent's write and this call the run can legitimately move. It was
// observed moving: a run parked at gate 8, approved by a human, re-queued and
// claimed again, all before the park's own report was delivered — whereupon
// the report rewound the row to awaiting_approval and amendment A27's clearing
// rule discarded the approval. §11's graph cannot catch that, because
// preparing → awaiting_approval is a real edge; it is how a park happens.
//
// So the guard is the update's own state, and a report of a state the run has
// left writes nothing. It is still marked reported: what this transport has
// seen is a fact about delivery, not about the row, and leaving it unmarked
// would keep the update in store.Unreported's replay set forever.
func (b *Broker) Report(ctx context.Context, runID string, u protocol.Update) error {
	if err := protocol.ValidateID(runID); err != nil {
		return fmt.Errorf("local: Report: %w", err)
	}
	if !u.State.Valid() {
		return fmt.Errorf("local: Report %s: unknown state %q", runID, u.State)
	}
	// §22 and amendment A8: expires_at belongs to a waiting state and a
	// transition out of one clears it. An update that asks for a deadline on a
	// running run is a contradiction, and honouring it would arm the TTL sweep
	// against a build in flight.
	if u.ExpiresAt != nil && !u.State.IsWaiting() {
		return fmt.Errorf(
			"local: Report %s: an expiry belongs to a waiting state, not %s (RUNNER_SPEC §11)", runID, u.State)
	}

	applied, err := b.st.ReapplyIfCurrent(ctx, runID, u)
	if err != nil {
		return err
	}

	// The three fields amendment A8 added. Transition applies none of them;
	// see the [Store] comment for why that is a contract defect rather than an
	// oversight here. They are written only when the transition was, and for
	// the same reason: a stale report's runner, deadline and cancel reason
	// describe the same superseded moment its state does.
	if applied {
		if u.Runner != "" {
			if err := b.st.SetRunner(ctx, runID, u.Runner); err != nil {
				return err
			}
		}
		if u.ExpiresAt != nil {
			if err := b.st.SetExpiry(ctx, runID, *u.ExpiresAt); err != nil {
				return err
			}
		}
		if u.CancelReason != "" {
			if err := b.st.SetCancelReason(ctx, runID, u.CancelReason); err != nil {
				return err
			}
		}
	}

	return b.st.MarkReported(ctx, store.BrokerLocal, runID, u.State)
}

// Logs returns the sequenced sink for one run.
//
// The run is looked up first so that a sink for an id that does not exist
// fails here, where the caller still has context, rather than on the first
// batch of a build that has already started producing output.
//
// The resume point is read back under this transport's own key. Before A28 it
// came off the [store.Run] the lookup already had, which was the same column the
// remote transport wrote — so a restarted agent could open a local sink already
// positioned at whatever the control plane happened to have acknowledged.
func (b *Broker) Logs(ctx context.Context, runID string) (broker.LogSink, error) {
	if err := protocol.ValidateID(runID); err != nil {
		return nil, fmt.Errorf("local: Logs: %w", err)
	}
	run, err := b.st.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	acked, err := b.st.LastAckedSeq(ctx, store.BrokerLocal, run.ID)
	if err != nil {
		return nil, err
	}
	return newSink(b.st, run.ID, acked), nil
}

// Heartbeat records liveness locally.
//
// There is no local recipient, and pretending otherwise would be the wrong
// kind of honesty: a heartbeat to yourself is a variable, not a message. §20's
// heartbeat exists so a control plane can mark a machine offline after three
// missed beats, and no control plane is involved here. The value is kept so
// `latem doctor` and `latem status` can show what the agent last believed
// about itself without asking every subsystem again.
// ReportPlan refuses: a plan is a control plane's question (A52), and the
// local socket never asks it, so there is nobody here to answer.
func (b *Broker) ReportPlan(context.Context, protocol.PlanResult) error {
	return fmt.Errorf("local: ReportPlan: the local socket does not plan events: %w", errors.ErrUnsupported)
}

func (b *Broker) Heartbeat(_ context.Context, h protocol.MachineHealth) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.health = h
	b.healthAt = b.now()
	return nil
}

// Health returns the last value passed to [Broker.Heartbeat] and when it
// arrived. The bool is false before the first beat.
//
// Not part of [broker.Broker]: it is the local read of something the interface
// only knows how to write.
func (b *Broker) Health() (protocol.MachineHealth, time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.health, b.healthAt, !b.healthAt.IsZero()
}
