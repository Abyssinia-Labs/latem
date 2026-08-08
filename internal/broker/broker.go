// Package broker defines the single seam between the runner stack and any
// control plane.
//
// This file is owned by WS-00 and frozen. Both transports — internal/broker/local
// (WS-11) and internal/broker/remote (WS-14) — implement this interface, and the
// agent contains no branch on which is in use. Changing anything here is a
// contract amendment: a small pull request touching only this file, with a
// reason, that everyone rebases onto.
//
// Every type crossing the wire lives in pkg/protocol. This package defines the
// interface and the transports; it defines no wire types of its own
// (RUNNER_SPEC §4, finding F8).
package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ErrAlreadyClaimed is returned by [Broker.Claim] when another runner won the
// race. It is an ordinary outcome, not a failure: exactly one claimant wins and
// the rest move on.
var ErrAlreadyClaimed = errors.New("broker: run already claimed")

// Broker is a source of inbound commands and a sink for outbound reports.
type Broker interface {
	// Commands yields inbound commands. It closes only on ctx cancellation.
	//
	// Implementations reconnect internally. A dropped connection is not an
	// error the caller sees, and must not be — the agent's job is to keep
	// building, and a control plane that is briefly unreachable is a normal
	// condition rather than an exceptional one.
	Commands(ctx context.Context) (<-chan protocol.Command, error)

	// Claim attempts exclusive ownership of a run. It returns
	// [ErrAlreadyClaimed] if another runner won. Implementations MUST make
	// this atomic at the broker; two agents executing one run is the failure
	// this method exists to prevent.
	Claim(ctx context.Context, runID string) error

	// Report publishes a state transition. Idempotent by (runID, state), so a
	// replay after reconnect is safe.
	Report(ctx context.Context, runID string, u protocol.Update) error

	// Logs returns a sequenced sink for one run's output.
	Logs(ctx context.Context, runID string) (LogSink, error)

	// Heartbeat publishes liveness and capacity.
	Heartbeat(ctx context.Context, h protocol.MachineHealth) error

	// Close releases the transport: a listener for the local broker, a
	// connection for the remote one.
	//
	// Draft 02 had no lifecycle method, so the agent had to hold a concrete
	// type to shut a broker down — which undercuts the one property this
	// interface exists for, that the agent contains no branch on which
	// transport is in use (amendment A19).
	//
	// MUST be idempotent: graceful shutdown and a deferred close both run on
	// the normal path.
	Close() error
}

// LogSink accepts batched log frames.
//
// It replaces the io.WriteCloser this interface carried in draft 01 of the
// spec. A plain writer cannot express resumption: after a reconnect the agent
// has to know what the far side already holds, or it re-sends a buffer and
// duplicates precisely the output covering the outage (finding F6).
type LogSink interface {
	// Write submits a batch. Frames carry monotonic per-run sequence numbers
	// and have already passed the redactor.
	Write(ctx context.Context, frames []protocol.LogFrame) error

	// Acked reports the highest sequence durably accepted for this run. The
	// agent persists it and resumes from Acked()+1.
	Acked() uint64

	Close(ctx context.Context) error
}

// NotImplemented satisfies [Broker] by refusing every call.
//
// It exists so that downstream workstreams can wire and test against a real
// [Broker] value before their transport is written, and so that the interface
// is proven implementable at the moment it is frozen rather than at the moment
// someone first tries.
type NotImplemented struct{}

var _ Broker = NotImplemented{}

func (NotImplemented) Commands(context.Context) (<-chan protocol.Command, error) {
	return nil, fmt.Errorf("broker: Commands: %w", errors.ErrUnsupported)
}

func (NotImplemented) Claim(context.Context, string) error {
	return fmt.Errorf("broker: Claim: %w", errors.ErrUnsupported)
}

func (NotImplemented) Report(context.Context, string, protocol.Update) error {
	return fmt.Errorf("broker: Report: %w", errors.ErrUnsupported)
}

func (NotImplemented) Logs(context.Context, string) (LogSink, error) {
	return nil, fmt.Errorf("broker: Logs: %w", errors.ErrUnsupported)
}

func (NotImplemented) Heartbeat(context.Context, protocol.MachineHealth) error {
	return fmt.Errorf("broker: Heartbeat: %w", errors.ErrUnsupported)
}

func (NotImplemented) Close() error { return nil }
