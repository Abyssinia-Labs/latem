package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/registry"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runSteps executes a job's steps in order, stopping at the first that does not
// succeed (RUNNER_SPEC §9).
//
// Each step is a new process. Working directory, exported variables, shell
// functions and traps do not survive one; the filesystem does, for the whole
// run. That contract belongs to the executor and this loop does nothing to
// weaken it: the same environment is handed to every step, regardless of what
// the previous one did to its own.
func (x *execution) runSteps(ctx context.Context, budget time.Duration) outcome {
	total := len(x.resolved.Steps)
	for i, s := range x.resolved.Steps {
		if out, done := x.checkBudget(ctx, stepLabel(s, i), budget); done {
			return out
		}

		label := stepLabel(s, i)
		x.note("step %d/%d: %s", i+1, total, label)
		start := x.o.now()

		out := x.runStep(ctx, s, label, budget)

		x.note("step %d/%d: %s finished in %s", i+1, total, label,
			x.o.now().Sub(start).Round(time.Millisecond))
		if !out.ok() {
			return out
		}
	}
	return outcome{state: protocol.StateSucceeded}
}

// checkBudget refuses to start a step the job has no time left for. Without it
// a job whose wall clock expired between two steps would start the next one and
// be killed a moment later, with a log that reads as though the step failed.
func (x *execution) checkBudget(ctx context.Context, label string, budget time.Duration) (outcome, bool) {
	if requested, reason, actor := x.h.cancelled(); requested {
		return cancelOutcome(outcome{}, reason, actor), true
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutOutcome(label, true, budget), true
	}
	return outcome{}, false
}

// runStep executes one step and classifies what came back.
//
// The classification is the whole job of this function, and the order of the
// checks is what makes §21's codes mean different things:
//
//   - a cancel outranks everything, because a step killed by SIGTERM exits
//     non-zero and calling that E_STEP would tell an operator their build is
//     broken when they stopped it themselves;
//   - the job wall clock outranks the step's, so a 90 minute job that ran out
//     during a step with its own 75 minute budget is E_TIMEOUT and not
//     E_STEP_TIMEOUT;
//   - a step deadline that fired while the job budget still had room is
//     E_STEP_TIMEOUT (§9);
//   - a non-zero exit is E_STEP, and the code travels with it;
//   - anything else — the environment could not run the step, or could not be
//     waited on — is whatever the executor said, defaulting to E_EXECUTOR.
func (x *execution) runStep(
	ctx context.Context, s registry.ResolvedStep, label string, budget time.Duration,
) outcome {
	stepCtx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	stdout := x.stream(protocol.StreamStdout)
	stderr := x.stream(protocol.StreamStderr)

	step := executor.Step{
		Name:    label,
		Run:     s.Run,
		Env:     x.stepEnv(s),
		Timeout: s.Timeout,
	}

	type finished struct {
		code int
		err  error
	}
	done := make(chan finished, 1)
	go func() {
		code, err := x.env.Exec(stepCtx, step, stdout.pw, stderr.pw)
		done <- finished{code, err}
	}()

	var r finished
	abandoned := false
	select {
	case r = <-done:
	case <-stepCtx.Done():
		// §11 hands the executor the signalling contract — SIGTERM, 30
		// seconds, SIGKILL — so this waits for it rather than reimplementing
		// it. The bound exists because an executor that does not honour the
		// contract must not be able to hold the run open forever; the run then
		// proceeds to collecting, which is where the value is.
		timer := time.NewTimer(x.o.cancelGrace)
		select {
		case r = <-done:
		case <-timer.C:
			abandoned = true
			r = finished{code: -1, err: context.Cause(stepCtx)}
		}
		timer.Stop()
	}

	if abandoned {
		x.note("step %q did not stop within %s of cancellation; abandoning it",
			label, x.o.cancelGrace)
		stdout.abandon()
		stderr.abandon()
	} else {
		stdout.close()
		stderr.close()
	}

	if requested, reason, actor := x.h.cancelled(); requested {
		return cancelOutcome(outcome{}, reason, actor)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutOutcome(label, true, budget)
	}
	if s.Timeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		return timeoutOutcome(label, false, s.Timeout)
	}
	if isCancellation(r.err) || (r.err == nil && isCancellation(ctx.Err())) {
		// The context closed without a cancel command and without a deadline:
		// the agent is shutting down under it.
		return outcome{
			state: protocol.StateCancelled,
			msg:   fmt.Sprintf("run stopped during step %q", label),
		}
	}
	if r.err != nil {
		return fail(r.err, protocol.ErrExecutor)
	}
	if r.code != 0 {
		code := r.code
		return outcome{
			state:    protocol.StateFailed,
			code:     protocol.ErrStep,
			msg:      fmt.Sprintf("step %q exited %d", label, code),
			exitCode: &code,
		}
	}
	return outcome{state: protocol.StateSucceeded}
}

// stepLabel names a step for the log and for [executor.Step.Name]. A step's
// name is optional in the registry, and "step 3" beats an empty string in an
// error message somebody has to act on.
func stepLabel(s registry.ResolvedStep, i int) string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("step %d", i+1)
}
