package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/Abyssinia-Labs/latem/internal/artifact"
	"github.com/Abyssinia-Labs/latem/internal/cache"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
	"github.com/Abyssinia-Labs/latem/internal/source"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// outcome is where a run ends up and why. It is the single value every phase
// of [execution.run] returns, so the terminal transition is written once
// rather than once per failure mode.
type outcome struct {
	state    protocol.RunState
	code     protocol.ErrorCode
	msg      string
	exitCode *int
	// reason is a cancel reason, already redacted and truncated.
	reason string
}

func (o outcome) ok() bool { return o.state == protocol.StateSucceeded }

// codeOf asks every leaf package whether it recognises the error.
//
// Each of them exports a CodeOf returning the empty code for anything it did
// not produce, which is what makes this a chain rather than a type switch over
// five concrete error types. The order does not matter: no two of them claim
// the same error.
func codeOf(err error) protocol.ErrorCode {
	if err == nil {
		return ""
	}
	for _, fn := range []func(error) protocol.ErrorCode{
		registry.CodeOf, source.CodeOf, secrets.CodeOf, cache.CodeOf, artifact.Code,
	} {
		if c := fn(err); c != "" {
			return c
		}
	}
	return ""
}

// fail turns an error into an outcome, using fallback when no leaf package
// claims it.
//
// The message is the error's own text. It is redacted by the caller before it
// reaches [protocol.Update] — every one of these strings can quote a command,
// a path, or a program's stderr, and INV-4 does not make an exception for the
// error path.
func fail(err error, fallback protocol.ErrorCode) outcome {
	code := codeOf(err)
	if code == "" {
		code = fallback
	}
	state := code.TerminalState()
	if state == "" {
		state = protocol.StateFailed
	}
	return outcome{state: state, code: code, msg: err.Error()}
}

// fromDecision turns a gate refusal into an outcome. A [registry.Decision]
// already carries both halves; this only unpacks it.
func fromDecision(d registry.Decision) outcome {
	return outcome{state: d.State, code: d.Code, msg: d.Msg}
}

// secretOutcome separates the two secret failures §21 defines, which
// internal/secrets collapses into one.
//
// Amendment A16 added E_SECRET_BACKEND — failed, retryable — specifically to
// distinguish "you never set this" from "I cannot reach where you set it", on
// the grounds that a Mac's keychain being locked after a reboot is a
// machine condition that resolves without anyone editing a commit.
// internal/secrets predates the amendment: its Error.Code is a constant
// E_MISSING_SECRET, and its doc comment still says §21 defines no code for a
// broken backend. So the distinction is drawn here, where the outcome is
// decided, from the cause the error already carries.
//
// A program that is simply absent from PATH stays E_MISSING_SECRET. Installing
// `op` is a human action on the same footing as setting the secret in the
// first place, and offering a retry button for it would be offering a button
// that cannot work.
func secretOutcome(err error) outcome {
	code := protocol.ErrMissingSecret

	var ce *secrets.CommandError
	switch {
	case errors.As(err, &ce) && !ce.NotInstalled():
		code = protocol.ErrSecretBackend
	case errors.Is(err, fs.ErrPermission):
		// secrets.env with the wrong mode: §15 names it as a backend fault.
		code = protocol.ErrSecretBackend
	}

	msg := err.Error()
	if code != protocol.ErrMissingSecret {
		// internal/secrets prefixes its message with the code it assumed. A
		// message reading "E_MISSING_SECRET: ..." beside an ErrorCode of
		// E_SECRET_BACKEND is worse than either one alone.
		msg = strings.TrimPrefix(msg, string(protocol.ErrMissingSecret)+": ")
	}
	return outcome{state: code.TerminalState(), code: code, msg: msg}
}

// errStepAbandoned is what the log pipeline is torn down with when a step did
// not stop within the cancellation grace. It never reaches a user; it exists
// so the abandoned executor goroutine's writes fail fast instead of blocking
// on a reader nobody is draining.
var errStepAbandoned = errors.New("run: step abandoned after cancellation grace")

// ErrNotClaimed is returned by [Orchestrator.Execute] for a run that is not in
// the claimed state. Executing anything else would either double-run a job or
// resurrect a terminal one.
var ErrNotClaimed = errors.New("run: run is not claimed")

// ErrAlreadyExecuting means Execute was called twice for one run id. It is a
// caller bug rather than a race outcome: the store's atomic claim is what
// stops two agents reaching here, and two goroutines in one agent reaching
// here means the runner pool lost track of what it started.
var ErrAlreadyExecuting = errors.New("run: run is already executing")

// timeoutOutcome distinguishes the two wall clocks of §21.
//
// A step deadline that fired while the job budget still had room is
// E_STEP_TIMEOUT; the job budget itself is E_TIMEOUT. Both are timed_out and
// both still run collecting, so the only thing separating them is the code —
// which is exactly the point of having two: "your 5 minute install step hung"
// and "your 90 minute job ran out" send an operator to different places.
func timeoutOutcome(stepName string, jobExpired bool, budget fmt.Stringer) outcome {
	if jobExpired {
		return outcome{
			state: protocol.StateTimedOut,
			code:  protocol.ErrTimeout,
			msg:   fmt.Sprintf("job exceeded its %s wall clock during step %q", budget, stepName),
		}
	}
	return outcome{
		state: protocol.StateTimedOut,
		code:  protocol.ErrStepTimeout,
		msg:   fmt.Sprintf("step %q exceeded its %s timeout", stepName, budget),
	}
}

// isCancellation reports whether an error is the run's context being cancelled
// rather than something the build did.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled)
}
