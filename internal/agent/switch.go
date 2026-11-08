package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// Moving a running agent onto a new binary (A56).
//
// `latem update` replaces the file the agent runs and then sends SIGUSR2. The
// signal carries no version and names no file: the agent asks the binary at
// its own path what it is, and moves onto it only if that differs from what it
// is running. So a stray signal is a question with the answer "current", and
// the thing that decides what runs next is the file on disk, exactly as it is
// for a restart.
//
// The same binary is moved onto too when the last reload read changes to
// config.toml that only a restart applies — a runner's labels or env (A74).
// That is a restart that never cancels a run, which is what `latem setup`
// asks for after it writes one.
//
// The move is §20's, arranged rather than observed: stop claiming, wait for
// what is in flight, re-exec in place. Two things differ from the daily
// self-update, both because a person asked for this one:
//
//   - The wait is shutdown_grace, the time this operator has already said runs
//     may have to finish, rather than ten minutes.
//   - Running out of it is not "try again tomorrow". The agent takes work
//     again and moves at the first moment nothing is running, checked every
//     [switchRetry]. It never cancels a run to do it.

// switchRetry is how often a busy agent with a move pending looks for an idle
// moment.
var switchRetry = 30 * time.Second

// claimSettle is how long a pause is given before idleness is measured. A
// worker that passed the pool's accepts() just before the pause is between
// claiming a run and recording it as active, and this is the width of that.
const claimSettle = time.Second

// Switch states, as [SwitchState.State] reports them.
const (
	// SwitchCurrent: the binary at the agent's path is the one it runs.
	SwitchCurrent = "current"
	// SwitchWaiting: the agent has stopped claiming and is waiting for runs
	// in flight, or has resumed claiming and is waiting for an idle moment.
	SwitchWaiting = "waiting"
	// SwitchNow: the agent is re-executing. The next status is the new
	// binary's.
	SwitchNow = "switching"
	// SwitchFailed: the agent could not read the binary's version, or the
	// re-exec failed; Error says which. It keeps running what it ran.
	SwitchFailed = "failed"
)

// SwitchState is the agent's answer to the last SIGUSR2, in its status file.
type SwitchState struct {
	// Requests counts the SIGUSR2s answered, so a reader can tell the answer
	// to its own signal from an earlier one.
	Requests int `json:"requests"`
	// State is one of the Switch* constants.
	State string `json:"state"`
	// To is what the binary on disk reported itself to be.
	To string `json:"to,omitempty"`
	// Runs is how many runs the agent is waiting for, while waiting.
	Runs int `json:"runs,omitempty"`
	// Error says why the state is failed.
	Error string `json:"error,omitempty"`
}

// Switch answers a SIGUSR2: move onto the binary at this agent's own path if
// it is a different version, once nothing is running. It returns once the
// answer is written; the wait, if there is one, runs in the background.
func (a *Agent) Switch(ctx context.Context) {
	a.reloadMu.Lock()
	ready := !a.startedAt.IsZero()
	a.reloadMu.Unlock()
	if !ready {
		// Nothing that reads the status signals before the agent has written
		// one, so this is somebody's kill(1). The binary it is starting is the
		// one on disk.
		a.log.Info("SIGUSR2 while the agent is starting; it is starting the binary on disk")
		return
	}
	a.countSwitch()

	target, err := a.selfPath()
	if err != nil {
		a.answerSwitch(SwitchFailed, "", 0, fmt.Errorf("locating the running binary: %w", err))
		return
	}
	to, err := a.binaryVersion(ctx, target)
	if err != nil {
		a.answerSwitch(SwitchFailed, "", 0, fmt.Errorf("asking %s its version: %w", target, err))
		return
	}
	restart := false
	if to == a.version {
		pending := a.pendingRestart()
		if len(pending) == 0 {
			// Nothing to move onto, and still a nudge from someone who may
			// have changed the machine under it — installed an Xcode, accepted
			// a licence — so it looks again at what it has (A59).
			a.log.Info("SIGUSR2: the binary on disk is the one running; probing toolchains again",
				"version", to, "path", target)
			a.answerSwitch(SwitchCurrent, to, 0, nil)
			a.reprobeToolchains()
			return
		}
		// The last reload read changes only a restart applies (A74): the same
		// binary, re-executed, starts on the file as it is.
		restart = true
		a.log.Info("SIGUSR2: restarting in place once nothing is running, for what the last reload did not apply",
			"version", to, "not_applied", pending)
	}
	if a.switching.Swap(true) {
		// A second request while the first waits is the same request.
		a.answerSwitch(SwitchWaiting, to, a.activeRuns(), nil)
		return
	}
	go a.switchWhenIdle(ctx, target, to, restart, a.shutdownGrace)
}

// pendingRestart is what the last reload read and did not apply.
func (a *Agent) pendingRestart() []string {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	return slices.Clone(a.deferred)
}

// switchWhenIdle waits for idleness and re-execs. It returns only if the
// re-exec failed or the agent is shutting down. restart is a re-exec of the
// same binary for config.toml's sake. hold is how long it stops claiming for
// the runs in flight before it takes work again and waits for an idle moment
// instead.
func (a *Agent) switchWhenIdle(ctx context.Context, target, to string, restart bool, hold time.Duration) {
	defer a.switching.Store(false)
	a.log.Info("moving onto the binary on disk once nothing is running",
		"running", a.version, "to", to, "path", target, "runs", a.activeRuns())

	if a.quiesce(ctx, hold, to) {
		a.moveInto(target, to, restart)
		return
	}
	a.resumeClaims()
	a.log.Info("still busy; taking work again, and moving at the first idle moment",
		"to", to, "runs", a.activeRuns(), "held", hold)
	a.answerSwitch(SwitchWaiting, to, a.activeRuns(), nil)

	t := time.NewTicker(switchRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.activeRuns() > 0 {
			continue
		}
		if a.quiesce(ctx, 0, to) {
			a.moveInto(target, to, restart)
			return
		}
		a.resumeClaims()
	}
}

// quiesce stops claiming and waits up to bound for the runs in flight,
// reporting the wait. It returns with claims paused, idle or not: the caller
// re-execs or resumes.
func (a *Agent) quiesce(ctx context.Context, bound time.Duration, to string) bool {
	a.pauseClaims()
	select {
	case <-ctx.Done():
		return false
	case <-time.After(claimSettle):
	}
	if n := a.activeRuns(); n > 0 {
		a.answerSwitch(SwitchWaiting, to, n, nil)
	}
	return a.awaitIdle(ctx, bound)
}

// moveInto re-execs into target. A restart for config.toml reads the file
// once more first: one that no longer loads would leave a service manager
// starting an agent that exits, so the agent keeps running instead, and one
// whose restart-only changes were undone has nothing left to restart for.
func (a *Agent) moveInto(target, to string, restart bool) {
	if restart {
		deferred, err := a.Reload()
		switch {
		case err != nil:
			a.answerSwitch(SwitchFailed, to, 0, fmt.Errorf("config.toml no longer loads, so the agent did not restart: %w", err))
			a.resumeClaims()
			return
		case len(deferred) == 0:
			a.answerSwitch(SwitchCurrent, to, 0, nil)
			a.resumeClaims()
			return
		}
	}
	a.reexecInto(target, to)
}

// reexecInto replaces this process with target. On success it does not
// return; on failure the agent takes work again on the binary it has.
func (a *Agent) reexecInto(target, to string) {
	a.answerSwitch(SwitchNow, to, 0, nil)
	if err := a.reexec(target); err != nil {
		a.log.Error("could not move onto the binary on disk; still running the old one",
			"version", a.version, "to", to, "err", err)
		a.answerSwitch(SwitchFailed, to, 0, err)
		a.resumeClaims()
	}
}

// answerSwitch records the answer to the current SIGUSR2 in the status file.
func (a *Agent) answerSwitch(state, to string, runs int, err error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	sw := SwitchState{Requests: a.switchRequests, State: state, To: to, Runs: runs}
	if err != nil {
		sw.Error = err.Error()
		a.log.Error("SIGUSR2: not moving onto the binary on disk", "err", err)
	}
	a.switchState = &sw
	a.writeStatus()
}

// countSwitch notes that a SIGUSR2 arrived, before anything answers it, so
// every answer to it carries the new count.
func (a *Agent) countSwitch() {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	a.switchRequests++
}

// binaryVersion asks a latem binary what it is.
func (a *Agent) binaryVersion(ctx context.Context, path string) (string, error) {
	if a.opts.BinaryVersion != nil {
		return a.opts.BinaryVersion(ctx, path)
	}
	return selfupdate.BinaryVersion(ctx, path)
}

// ErrCannotSwitch: an agent holds the lock and its status names no version, so
// it predates A56 — and SIGUSR2 would stop it.
var ErrCannotSwitch = errors.New("the running agent does not move onto a new binary by itself")

// DefaultSwitchWait is how long [SignalSwitch] waits for an answer. An idle
// agent answers within a second and re-execs within a few more; a busy one
// answers "waiting" as soon as it has paused.
const DefaultSwitchWait = 30 * time.Second

// SignalSwitch asks the agent holding paths.Lock to move onto the binary at its
// own path, and returns its answer (A56). An agent already running want is
// asked anyway when its last reload left something for a restart (A74), and
// re-executes the same binary. It returns once the agent reports
// running want, or waiting, current, or failed — whichever comes first.
//
// It signals only an agent that has said it handles SIGUSR2, by writing a
// status that names its version: an older one takes Go's default action for
// the signal, which is to exit.
func SignalSwitch(paths config.Paths, want string, wait time.Duration) (ReloadStatus, error) {
	pid, held, err := Holder(paths.Lock)
	if err != nil {
		return ReloadStatus{}, err
	}
	if !held {
		return ReloadStatus{}, ErrNoAgent
	}
	before, err := ReadReloadStatus(paths.ReloadStatus)
	if err != nil || pid == 0 || before.PID != pid || before.Version == "" {
		return before, fmt.Errorf("%w (pid %d)", ErrCannotSwitch, pid)
	}
	if before.Version == want && len(before.Deferred) == 0 {
		return before, nil
	}
	asked := 0
	if before.Switch != nil {
		asked = before.Switch.Requests
	}
	if err := sendSwitch(pid); err != nil {
		return before, fmt.Errorf("signalling agent pid %d: %w", pid, err)
	}

	if wait <= 0 {
		wait = DefaultSwitchWait
	}
	deadline := time.Now().Add(wait)
	last := before
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		st, err := ReadReloadStatus(paths.ReloadStatus)
		if err != nil || st.PID != pid {
			// Mid-exec: the old image's status is gone or the new one has
			// not written yet. The pid survives an exec, so a different one
			// is a restart by something else.
			continue
		}
		last = st
		switch {
		case st.Version == want && st.Started.After(before.Started):
			return st, nil
		case st.Switch == nil || st.Switch.Requests <= asked:
			continue
		case st.Switch.State == SwitchWaiting, st.Switch.State == SwitchCurrent, st.Switch.State == SwitchFailed:
			return st, nil
		}
	}
	return last, fmt.Errorf("agent pid %d was signalled and did not report running %s within %s; its log says why", pid, want, wait)
}
