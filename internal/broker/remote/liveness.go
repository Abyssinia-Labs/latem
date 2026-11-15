package remote

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// liveness is what one connection knows of the control plane at its far end:
// when anything last arrived from it, and whether this side has given the
// connection up (A98).
//
// The kernel cannot answer that question for us. A write returns once its
// bytes are in the socket's send buffer, so on a link whose far end has gone —
// a gateway redeployed under it, a route that stopped carrying IPv6 — every
// heartbeat "succeeds" while the buffer fills and TCP retransmits into nothing
// for the quarter of an hour tcp_retries2 allows. Meanwhile the control plane
// has marked the machine offline and is holding its work. The read side is the
// only one that can tell: a frame or a pong arrived, or it did not.
//
// The clock is the monotonic one, not [Options] now: this measures an
// interval on this machine, and a wall clock stepped by NTP must not end a
// healthy connection or keep a dead one.
type liveness struct {
	mu     sync.Mutex
	heard  time.Time
	gaveUp bool
}

// newLiveness starts a connection's clock at the handshake, which is the first
// thing the control plane said on it.
func newLiveness() *liveness {
	return &liveness{heard: time.Now()}
}

// hear records that something arrived: a frame of any kind, or a pong.
func (l *liveness) hear() {
	l.mu.Lock()
	l.heard = time.Now()
	l.mu.Unlock()
}

func (l *liveness) lastHeard() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heard
}

// giveUp marks the connection given up, and reports whether this call was the
// one that did it, so the reason is said once however many paths reach it.
func (l *liveness) giveUp() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gaveUp {
		return false
	}
	l.gaveUp = true
	return true
}

func (l *liveness) givenUp() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gaveUp
}

// keepalive pings the control plane every ping interval, and gives the
// connection up once nothing has been heard on it for the silence window: a
// read deadline that every frame and every pong extends (A98).
//
// Pings rather than the heartbeat, because the heartbeat is not answered
// (PROTOCOL.md §4.5): an idle machine hears nothing from a healthy control
// plane for hours, so silence alone proves nothing. A WebSocket ping is
// answered by the far side's WebSocket layer — RFC 6455 §5.5.2 requires it —
// without a frame of the protocol changing, and latem-cloud's gateways answer
// it without code of their own: the Workers runtime does it for a hibernating
// Durable Object without waking it, and `ws` does it for the Node server.
//
// Any frame extends the deadline as well as a pong. A link busy with a
// reconnect's log flush has the control plane's acknowledgements arriving
// steadily while this side's ping waits behind megabytes of its own output in
// the send buffer; judging by the pong alone would end exactly the connection
// that is catching up.
//
// The ping goes out on a goroutine of its own and at most one is outstanding,
// so the deadline is checked on time even while a ping is still waiting for a
// write lock or its pong; it ends when its pong arrives or the session does.
// The deadline is exact: it is woken for, not polled.
func (b *Broker) keepalive(ctx context.Context, conn *websocket.Conn, live *liveness) {
	var pings sync.WaitGroup
	defer pings.Wait()
	// Holds a token while a ping is out.
	outstanding := make(chan struct{}, 1)

	nextPing := time.Now().Add(b.pingEvery)
	timer := time.NewTimer(b.pingEvery)
	defer timer.Stop()
	for {
		now := time.Now()
		deadline := live.lastHeard().Add(b.silence)
		if !now.Before(deadline) {
			b.giveUp(conn, live, fmt.Sprintf("no frame from the control plane in %s, reconnecting", b.silence))
			return
		}
		if !now.Before(nextPing) {
			nextPing = now.Add(b.pingEvery)
			select {
			case outstanding <- struct{}{}:
				pings.Add(1)
				go func() {
					defer pings.Done()
					defer func() { <-outstanding }()
					b.ping(ctx, conn, live)
				}()
			default:
				// The last one is still out. A second would queue behind it
				// on the same write lock and prove nothing the first will not.
			}
		}

		wake := nextPing
		if deadline.Before(wake) {
			wake = deadline
		}
		timer.Reset(time.Until(wake))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// ping sends one ping and records its pong.
//
// It has no deadline of its own, only the session's, and that is deliberate.
// The library closes a connection whose write outlives its context, so a ping
// with a deadline, stuck behind a full send buffer, would end the connection
// without its reason being said. Without one, only the two deadlines that say
// why can end it: a pong that never comes is the read deadline's to judge, and
// a ping that cannot be written is queued with frames that have a write
// deadline of their own. A failure is not judged here, so it is not logged.
func (b *Broker) ping(ctx context.Context, conn *websocket.Conn, live *liveness) {
	if err := conn.Ping(ctx); err == nil {
		live.hear()
	}
}

// giveUp ends a connection this side has stopped believing in, and says why,
// once. The reconnect loop does the rest: the read fails, [Broker.runSession]
// returns, and the next dial follows the ordinary backoff — from its first
// step, since a session that got as far as its resume frame was a working
// link.
//
// CloseNow rather than a close handshake. The far side is not answering, and a
// close frame would only wait out its own timeout behind the bytes already
// stuck in the send buffer.
func (b *Broker) giveUp(conn *websocket.Conn, live *liveness, why string, args ...any) {
	if live.giveUp() && !b.stopping() {
		b.log.Warn(why, append([]any{"url", b.url}, args...)...)
	}
	_ = conn.CloseNow()
}
