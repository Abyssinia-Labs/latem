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
	status := registry.StatusSuccess
	// first is the outcome the run reports. Since A42 a failure no longer
	// returns immediately: later steps get to consult their conditions, and
	// `if: always()` is how a cleanup step asks to. The run's result is still
	// the first failure — a cleanup step succeeding does not make the job
	// green, and one failing does not change which step broke it.
	var first *outcome

	for i, s := range x.resolved.Steps {
		label := stepLabel(s, i)
		if out, done := x.checkBudget(ctx, label, budget); done {
			return x.settle(out, first)
		}

		sc := x.stepScope(status)
		switch {
		case s.If != nil:
			holds, err := s.If.Holds(sc)
			if err != nil {
				out := fail(&registry.Error{Code: protocol.ErrBadRegistry,
					Msg: fmt.Sprintf("step %d/%d: %s: if", i+1, total, label), Err: err}, protocol.ErrBadRegistry)
				return x.settle(out, first)
			}
			if !holds {
				x.note("step %d/%d: %s skipped (if: %s)", i+1, total, label, s.If.Source())
				continue
			}
		case first != nil:
			// No condition, and something has already failed. The implicit
			// condition is success(), which no longer holds.
			x.note("step %d/%d: %s skipped (an earlier step failed)", i+1, total, label)
			continue
		}

		late, err := x.resolveLate(s, sc)
		if err != nil {
			return x.settle(fail(err, protocol.ErrBadRegistry), first)
		}

		x.note("step %d/%d: %s", i+1, total, label)
		start := x.o.now()

		out, outputs := x.runStep(ctx, s, label, budget, late)
		if s.ID != "" && len(outputs) > 0 {
			if x.outputs == nil {
				x.outputs = map[string]map[string]string{}
			}
			x.outputs[s.ID] = outputs
		}

		x.note("step %d/%d: %s %s", i+1, total, label,
			stepEnding(out, x.o.now().Sub(start).Round(time.Millisecond)))
		if !out.ok() {
			if stopsTheRun(out) {
				return x.settle(out, first)
			}
			status = statusAfter(status, out)
			if first == nil {
				cp := out
				first = &cp
			}
		}
	}
	if first != nil {
		return *first
	}
	return outcome{state: protocol.StateSucceeded}
}

// settle picks what the run reports when it stops early: the first failure if
// there was one, otherwise the outcome that stopped it. A cancel arriving
// during a cleanup step after a failure is reported as the failure, because
// that is what broke the build; the cancel is in the log.
func (x *execution) settle(stopping outcome, first *outcome) outcome {
	if first != nil && stopping.state != protocol.StateCancelled {
		return *first
	}
	return stopping
}

// stepEnding words the step's closing line according to how it ended.
//
// Every step used to close with "finished in 3.854s", including one that was
// killed. In a long job that line is what an operator reads to find where the
// run stopped, and for a cancel or a timeout it said the opposite of what
// happened: the run-level line below reported "cancelled", while the step above
// it claimed to have finished, and the duration was time spent waiting to die.
//
// A non-zero exit still reads "finished", because it did: the process ran to
// completion and chose its status. Only a killed step is worded differently,
// which is the distinction this line was flattening.
func stepEnding(out outcome, d time.Duration) string {
	switch out.state {
	case protocol.StateCancelled:
		return fmt.Sprintf("cancelled after %s", d)
	case protocol.StateTimedOut:
		return fmt.Sprintf("timed out after %s", d)
	}
	return fmt.Sprintf("finished in %s", d)
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
	late map[string]string,
) (outcome, map[string]string) {
	stepCtx := ctx
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	stdout := x.stream(protocol.StreamStdout)
	stderr := x.stream(protocol.StreamStderr)

	env := x.stepEnv(s)
	// Late values arrive last and win: the registry already refused a key
	// that is both static and late, so this is a merge with nothing to
	// collide with, and the ordering is only for the reader.
	for k, v := range late {
		env[k] = v
	}
	step := executor.Step{
		Name:    label,
		Run:     s.Run,
		Env:     env,
		Timeout: s.Timeout,
	}
	if c := s.Container; c != nil {
		step.Container = &executor.Container{Image: c.Image, Entrypoint: c.Entrypoint, Args: c.Args}
	}

	type finished struct {
		res executor.Result
		err error
	}
	done := make(chan finished, 1)
	go func() {
		res, err := x.env.Exec(stepCtx, step, stdout.pw, stderr.pw)
		done <- finished{res, err}
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
			r = finished{res: executor.Result{ExitCode: -1}, err: context.Cause(stepCtx)}
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
		return cancelOutcome(outcome{}, reason, actor), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutOutcome(label, true, budget), nil
	}
	if s.Timeout > 0 && errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
		return timeoutOutcome(label, false, s.Timeout), nil
	}
	if isCancellation(r.err) || (r.err == nil && isCancellation(ctx.Err())) {
		// The context closed without a cancel command and without a deadline:
		// the agent is shutting down under it.
		return outcome{
			state: protocol.StateCancelled,
			msg:   fmt.Sprintf("run stopped during step %q", label),
		}, nil
	}
	if r.err != nil {
		return fail(r.err, protocol.ErrExecutor), nil
	}
	if r.res.ExitCode != 0 {
		code := r.res.ExitCode
		return outcome{
			state:    protocol.StateFailed,
			code:     protocol.ErrStep,
			msg:      fmt.Sprintf("step %q exited %d", label, code),
			exitCode: &code,
		}, r.res.Outputs
	}
	return outcome{state: protocol.StateSucceeded}, r.res.Outputs
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
