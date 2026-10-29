package local

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// serve accepts connections until the broker is closed.
//
// One goroutine per connection, unbounded. The bound that matters is the file
// mode: the socket is 0600 inside a directory that is 0700, so every peer is
// the user running the agent, and a user who wants to exhaust their own
// machine's descriptors has simpler ways. Adding a semaphore here would trade
// that non-problem for a real one — `latem run` queueing behind an operator
// who left a connection open.
func (b *Broker) serve() {
	defer b.wg.Done()

	for {
		conn, err := b.ln.Accept()
		if err != nil {
			select {
			case <-b.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			b.log.Error("local broker stopped accepting", "socket", b.socket, "err", err)
			return
		}

		// serve holds a count of its own for as long as it is running, so the
		// group's counter is never zero at this point and this Add cannot race
		// the Wait in Close.
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(conn)
		}()
	}
}

// handle reads command frames from one connection until the client hangs up or
// sends something the broker will not answer twice.
//
// A malformed frame ends the connection. That is stricter than it needs to be
// for a truncated JSON object, where the newline framing is still intact, and
// it is exactly right for an oversized one, where it is not: a frame longer
// than [MaxFrameBytes] leaves the reader with no idea where the next frame
// begins, so continuing would mean parsing the tail of a value the client
// chose as if it were a command. Treating both the same way means there is one
// rule — a client that sends nonsense gets one answer and a closed
// connection — rather than a rule whose safety depends on which kind of
// nonsense arrived.
func (b *Broker) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// The connection's lifetime is the broker's, not any one caller's: Close
	// unblocks a client that connected and then went quiet.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-b.done:
			_ = conn.SetDeadline(time.Now())
			cancel()
		case <-ctx.Done():
		}
	}()

	sc := newFrameReader(conn)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(b.timeout)); err != nil {
			return
		}
		if !sc.Scan() {
			if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
				b.respond(conn, errReply("command frame exceeds the %d byte limit", MaxFrameBytes))
			}
			return
		}

		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			// A blank line is not a command. Skipping it keeps a client that
			// writes "\n" as a keepalive from being disconnected for it.
			continue
		}

		cmd, err := decodeCommand(line)
		if err != nil {
			// protocol.Command.UnmarshalJSON is the trust boundary: an unknown
			// kind, an unknown field, a payload that does not match its kind,
			// and a traversal run id all arrive here as this one error.
			b.respond(conn, errReply("%v", err))
			return
		}

		if !b.dispatch(ctx, conn, cmd) {
			return
		}
	}
}

// dispatch handles one decoded command and reports whether the connection may
// carry another.
func (b *Broker) dispatch(ctx context.Context, conn net.Conn, cmd protocol.Command) bool {
	switch cmd.Kind {
	case protocol.CommandRun:
		return b.dispatchRun(ctx, conn, *cmd.Run)
	case protocol.CommandCancel:
		return b.dispatchCancel(ctx, conn, cmd)
	case protocol.CommandPlan:
		// A plan answers a GitHub event, which reaches a machine only through
		// a control plane; there is no reply on this socket that could carry
		// one (A52).
		b.respond(conn, errReply("the local socket does not plan events; plans come from a control plane"))
		return false
	default:
		// Unreachable: the decoder closes the kind set (INV-2). It is here so
		// that a third kind added to pkg/protocol without a handler is a
		// refusal rather than a silent no-op, which is the failure mode
		// Command.UnmarshalJSON's own comment is written about.
		b.respond(conn, errReply("unsupported command kind %q", cmd.Kind))
		return false
	}
}

// dispatchRun makes the run durable, answers, and then wakes the agent.
//
// The order is the point. Once the row is in the queue the run will happen —
// by this agent in a moment, or by the next agent to start if this one dies
// between the two statements below. So the client is told "queued, here is
// your id" as soon as that is true, and delivery to the agent is a wake-up
// whose failure costs latency rather than a build. The opposite order would
// make `latem run` wait on an agent that is busy, which is the hang §7.3's
// queue exists to prevent.
func (b *Broker) dispatchRun(ctx context.Context, conn net.Conn, req protocol.RunRequest) bool {
	var expires time.Time
	if b.queueTTL > 0 {
		expires = b.now().Add(b.queueTTL)
	}

	if err := enqueueRun(ctx, b.st, req, 0, expires); err != nil {
		b.respond(conn, errReply("%v", err))
		return false
	}
	if !b.respond(conn, reply{OK: true, RunID: req.ID}) {
		return false
	}

	if !b.send(ctx, protocol.NewRunCommand(req)) {
		// Not an error the client needs: the run is queued and the runner pool
		// claims it. It is logged because an agent that is not draining its
		// command channel is a bug somewhere above this package.
		b.log.Warn("run queued but not delivered to the agent",
			"run_id", req.ID, "socket", b.socket)
	}
	return true
}

// dispatchCancel relays a cancel to the agent.
//
// Unlike a run, a cancel is durable nowhere. Nothing replays it, no table
// remembers it, and the only thing that can act on it is the goroutine holding
// the run's context — so the answer to the client is the delivery, and it is
// sent after the fact rather than before it.
//
// The store is not touched here. A cancel's effect is to cancel the context of
// work the agent itself started (§7.1), and the transition to cancelled
// belongs to the teardown path in §11, which knows whether the run reached
// collecting and must therefore be left alone.
func (b *Broker) dispatchCancel(ctx context.Context, conn net.Conn, cmd protocol.Command) bool {
	if !b.send(ctx, cmd) {
		b.respond(conn, errReply("the agent is not reading commands; the cancel was not delivered"))
		return false
	}
	return b.respond(conn, reply{OK: true, RunID: cmd.Cancel.ID})
}

// send hands a command to whoever is consuming [Broker.Commands].
func (b *Broker) send(ctx context.Context, cmd protocol.Command) bool {
	timer := time.NewTimer(b.deliver)
	defer timer.Stop()

	select {
	case b.cmds <- cmd:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	case <-b.done:
		return false
	}
}

// respond writes one reply frame and reports whether the connection is still
// usable.
func (b *Broker) respond(conn net.Conn, r reply) bool {
	if err := conn.SetWriteDeadline(time.Now().Add(b.timeout)); err != nil {
		return false
	}
	if err := writeFrame(conn, r); err != nil {
		b.log.Debug("local broker could not answer a client", "err", err)
		return false
	}
	return true
}

// notFound reports whether err is the store saying there is no such run. It is
// here so the client and the broker agree on what "no such run" looks like
// without either importing the other's error handling.
func notFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
