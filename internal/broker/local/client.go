package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ExitAgentDown is the process exit status RUNNER_SPEC §7.3 assigns to
// `latem run --wait` or `--follow` when no agent is listening.
//
// It is a constant here rather than a literal in the CLI because the number is
// part of the interface a script sees, and a script that greps stderr instead
// of checking the status is a script that breaks on the next wording change.
const ExitAgentDown = 3

// ErrAgentDown is the sentinel behind [AgentDownError]. Callers match on it
// with errors.Is; the concrete type carries the socket path and the dial
// failure for the message.
var ErrAgentDown = errors.New("local: no agent is listening")

// AgentDownError is returned when an operation needs a running agent and there
// is not one.
//
// It exists as a type rather than a bare sentinel because the CLI has to print
// something a person can act on. §7.3 requires the start command to be in the
// message — an error that says only "connection refused" against a socket
// path leaves a new user with nothing to try, and the first ten minutes of
// this product are entirely people who have not started the agent yet.
type AgentDownError struct {
	// Socket is the path that was dialled.
	Socket string
	// RunID is the run the caller was trying to act on, if any, so the message
	// above it can name one.
	RunID string
	// Err is the underlying dial failure.
	Err error
}

func (e *AgentDownError) Error() string {
	return fmt.Sprintf("no agent is listening on %s — start one with `latem agent` (%s)",
		e.Socket, protocol.ErrAgentDown)
}

// Unwrap exposes both the sentinel and the dial failure, so errors.Is finds
// [ErrAgentDown] and a caller debugging a permissions problem still finds the
// syscall underneath.
func (e *AgentDownError) Unwrap() []error { return []error{ErrAgentDown, e.Err} }

// Code is the stable identifier the CLI prints and the web app maps to copy.
// It is [protocol.ErrAgentDown], which pkg/protocol documents as a CLI
// condition that never appears in an Update.
func (e *AgentDownError) Code() protocol.ErrorCode { return protocol.ErrAgentDown }

// DefaultDialTimeout bounds the connect. Both ends are on this machine: a live
// agent accepts in microseconds, so anything longer than this is a machine in
// trouble, and making the user wait to be told the agent is down is the one
// thing worse than telling them.
const DefaultDialTimeout = 2 * time.Second

// Client is the writing half of the local broker: what `latem run` and
// `latem cancel` use.
//
// It holds a store as well as a socket path, and that is the whole design.
// §7.3 says a run submitted with no agent running is queued rather than
// refused, so the client must be able to reach the queue directly; the socket
// is how a running agent is told to start immediately, not how the work is
// recorded.
type Client struct {
	socket  string
	st      Store
	timeout time.Duration
	dial    func(ctx context.Context, path string) (net.Conn, error)
}

// ClientOptions configures a [Client].
type ClientOptions struct {
	// Socket is the agent socket path. Required.
	Socket string
	// Store is the sqlite queue. Required: without it a down agent has nowhere
	// to leave the run, which is the case §7.3 is written about.
	Store Store
	// Timeout bounds the dial and each frame. Defaults to
	// [DefaultDialTimeout].
	Timeout time.Duration

	// dial is injectable so a test can simulate a socket that accepts and then
	// stops talking, which is not a state a real listener can be put into on
	// demand.
	dial func(ctx context.Context, path string) (net.Conn, error)
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Socket == "" {
		return nil, errors.New("local: NewClient requires a socket path")
	}
	if opts.Store == nil {
		return nil, errors.New("local: NewClient requires a store: a run submitted with no agent running is queued, not refused (RUNNER_SPEC §7.3)")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultDialTimeout
	}
	if opts.dial == nil {
		opts.dial = dialUnix
	}
	return &Client{socket: opts.Socket, st: opts.Store, timeout: opts.Timeout, dial: opts.dial}, nil
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// Submission is the outcome of [Client.Submit].
type Submission struct {
	// RunID is the id the run was queued under. It is empty when nothing was
	// recorded, which is the case exactly when the error is an
	// [AgentDownError] — see [SubmitOptions.RequireAgent].
	RunID string

	// AgentRunning reports whether a live agent took the command. False means
	// the run is queued and nothing will start it until an agent runs — which
	// is what `queued (agent not running)` tells the user.
	AgentRunning bool
}

// SubmitOptions are the parts of a submission that are not on the wire.
//
// None of these can be expressed in a [protocol.RunRequest], and that is INV-2
// working as intended: priority is a scheduling decision and a TTL is
// configuration, so neither is something a control plane gets to assert. They
// travel beside the request, from this machine's own caller.
type SubmitOptions struct {
	// Priority is the queue discipline's first key. Higher runs first.
	Priority int

	// ExpiresAt is the queue_ttl deadline recorded on the run (§11, §22). Zero
	// means no deadline. It applies only to the agent-down path; when an agent
	// takes the command, the agent applies its own configured TTL, because the
	// agent is what read the config.
	ExpiresAt time.Time

	// RequireAgent turns a down agent into an [AgentDownError] instead of a
	// queue write. `latem run --wait` and `latem run --follow` set it: §7.3 is
	// explicit that waiting on a run nothing will start is an error and not a
	// hang, exit [ExitAgentDown].
	RequireAgent bool
}

// Submit queues a run and, if an agent is listening, asks it to start now.
//
// The two paths are deliberately not symmetrical in who writes the row. When
// an agent is up it writes, because it is the process that read the config and
// therefore the only one that knows the queue TTL, and because a single writer
// on the happy path is one less thing contending for sqlite's write lock. When
// the agent is down this client writes, because otherwise the run would be
// refused — and being refused is precisely what §7.3 says must not happen.
//
// Both writes are [enqueueRun], so what lands in the database does not depend
// on which one ran.
func (c *Client) Submit(ctx context.Context, req protocol.RunRequest, opts SubmitOptions) (Submission, error) {
	if err := protocol.ValidateID(req.ID); err != nil {
		return Submission{}, err
	}

	conn, dialErr := c.connect(ctx)
	if dialErr != nil {
		if opts.RequireAgent {
			// Nothing is written. --wait and --follow are requests to watch a
			// run happen, and queueing one that will not start while reporting
			// an error would leave a build to begin hours later, unwatched,
			// after the user has moved on.
			return Submission{}, &AgentDownError{Socket: c.socket, RunID: req.ID, Err: dialErr}
		}
		if err := enqueueRun(ctx, c.st, req, opts.Priority, opts.ExpiresAt); err != nil {
			return Submission{}, err
		}
		return Submission{RunID: req.ID}, nil
	}
	defer func() { _ = conn.Close() }()

	if err := c.exchange(conn, protocol.NewRunCommand(req)); err != nil {
		return Submission{RunID: req.ID}, err
	}
	return Submission{RunID: req.ID, AgentRunning: true}, nil
}

// Cancel stops a run.
//
// With an agent up this is a relay and nothing more: the command reaches the
// goroutine holding the run's context, and §11's teardown does the rest. It is
// the same [protocol.Command] a control plane would send, on a different
// transport, which is what §7.3 means by one code path.
//
// With no agent up, a run that has not started yet can still be cancelled,
// because nothing is holding it except the queue this client can reach. A run
// left mid-flight by a dead agent cannot: only the process that started the
// work can stop it, and startup reconciliation (§6 step 5) is what resolves
// those into interrupted. Saying so is better than pretending the cancel
// worked.
//
// Cancelling an unknown or already-terminal run is a no-op that reports
// success, per §7.1.
func (c *Client) Cancel(ctx context.Context, req protocol.CancelRequest) error {
	if err := protocol.ValidateID(req.ID); err != nil {
		return err
	}

	conn, dialErr := c.connect(ctx)
	if dialErr == nil {
		defer func() { _ = conn.Close() }()
		return c.exchange(conn, protocol.NewCancelCommand(req))
	}
	return c.cancelQueued(ctx, req, dialErr)
}

// cancelQueued cancels a run that has not started, with no agent present.
func (c *Client) cancelQueued(ctx context.Context, req protocol.CancelRequest, dialErr error) error {
	run, err := c.st.Get(ctx, req.ID)
	switch {
	case notFound(err):
		return nil
	case err != nil:
		return err
	case run.State.IsTerminal():
		return nil
	case !run.State.IsWaiting():
		return &AgentDownError{Socket: c.socket, RunID: req.ID, Err: dialErr}
	}

	req.Normalize()
	if req.Reason != "" {
		// The reason crossed no trust boundary here — it came from this user's
		// own command line — but it is stored through the same scrubbing path
		// as any other, because §22 requires cancel_reason to be redacted and
		// a second rule for locally-sourced text is a second thing to forget.
		if err := c.st.SetCancelReason(ctx, req.ID, req.Reason); err != nil {
			return err
		}
	}

	now := time.Now()
	return c.st.Transition(ctx, req.ID, protocol.StateCancelled, protocol.Update{
		State:        protocol.StateCancelled,
		FinishedAt:   &now,
		CancelReason: req.Reason,
	})
}

// AgentRunning reports whether an agent is listening. It is what `latem
// doctor` and `latem status` ask, and it never writes anything.
func (c *Client) AgentRunning(ctx context.Context) bool {
	conn, err := c.connect(ctx)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// connect dials the socket under the client's timeout.
func (c *Client) connect(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.dial(ctx, c.socket)
}

// exchange writes one command and reads the broker's answer.
//
// The reply is read rather than ignored because the broker refuses things the
// client cannot know about in advance — a run id that already belongs to a
// finished run, an agent that is not draining its command channel — and a
// fire-and-forget client would print a run id for work that was declined.
func (c *Client) exchange(conn net.Conn, cmd protocol.Command) error {
	deadline := time.Now().Add(c.timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("local: %w", err)
	}
	if err := writeFrame(conn, cmd); err != nil {
		return err
	}

	sc := newFrameReader(conn)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return fmt.Errorf("local: reading the agent's reply: %w", err)
		}
		return errors.New("local: the agent closed the connection without answering")
	}

	var r reply
	if err := decodeReply(sc.Bytes(), &r); err != nil {
		return err
	}
	if !r.OK {
		if r.Error == "" {
			r.Error = "the agent refused the command without saying why"
		}
		return fmt.Errorf("local: agent refused %s: %s", cmd.Kind, r.Error)
	}
	return nil
}
