package docker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ReadyBudget is how long Prepare waits, in all, for a started container to
// be running with a pid and to take an exec (LAT-23).
//
// Docker's API promises that a container whose start has returned takes an
// exec, and Docker Desktop keeps the promise. Kiri 0.2.1 does not always: an
// exec too soon after the start is answered `stat /bin/sh: no such file or
// directory`, exit 127, and the first step failed as E_STEP in no time at all.
// Measured on Kiri 0.2.1 with latem-ubuntu:local, whose /opt/hostedtoolcache
// holds 1.1 GB that a new tool-cache volume is filled from: with the volume
// already there, one container in five needed a second exec, 12 to 25 ms
// after the start; with a new volume, every container did, and was ready
// after 0.64 to 7.3 s (25 runs). The ticket's first guess, 2 s, failed one run
// in ten of those. Thirty seconds is four times the slowest seen, and an
// engine that never readies its container still fails the run in half a
// minute, as a retryable E_EXECUTOR.
const ReadyBudget = 30 * time.Second

const (
	// readyFirstBackoff is the first pause between attempts; it doubles
	// after each one, up to readyMaxBackoff, until the budget is spent.
	readyFirstBackoff = 10 * time.Millisecond
	readyMaxBackoff   = 500 * time.Millisecond
	// readyExecTimeout bounds one readiness exec. A hung exec is the engine
	// failing too, not a reason to wait out the job's budget.
	readyExecTimeout = 30 * time.Second
	// readyShellMissing is the engine's answer to an exec it took before the
	// container's filesystem was there. Kiri prints it as
	// `kiri: cannot start container: exec: "/bin/sh": stat /bin/sh: no such file or directory`
	// and runc's own wording ends the same way.
	readyShellMissing = "stat /bin/sh: no such file or directory"
)

// readyArgv is the readiness exec: the run container's own entrypoint shell,
// asked to do nothing. It is this package's and never a step's, which is how
// only it is retried: a step's exit 127 stays the step's.
var readyArgv = []string{"/bin/sh", "-c", "true"}

func isReadyExec(cmd []string) bool { return slices.Equal(cmd, readyArgv) }

// awaitReady waits until the run container takes an exec as the run user.
//
// First the container must be running with a pid, by inspect. Then one exec
// of [readyArgv], as a step would be executed: as the run user, in the
// workspace. That exec, and only that one, is tried again while the engine
// answers [readyShellMissing], after 10 ms, then 20, 40 and so on up to
// 500 ms between tries, until [ReadyBudget] is spent. The same pauses pace the
// inspects. Every other failure, and one that outlasts the
// budget, is E_EXECUTOR: the run never reached its first step, so no step
// failed.
func (e *Executor) awaitReady(ctx context.Context, env *runEnv) error {
	start := time.Now()
	deadline := start.Add(e.readyBudget)
	backoff := readyFirstBackoff
	// pause sleeps before the next attempt, and reports false when the budget
	// would be spent first or ctx ends.
	pause := func() bool {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		d := min(backoff, left)
		backoff = min(backoff*2, readyMaxBackoff)
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}

	for {
		insp, err := e.api.ContainerInspect(ctx, env.containerID)
		if err != nil {
			return wrapErr(protocol.ErrExecutor, err, "inspect container %s after its start", env.name)
		}
		st := insp.State
		if st != nil && st.Running && st.Pid > 0 {
			break
		}
		if st != nil && (st.Status == container.StateExited || st.Status == container.StateDead) {
			return codeErr(protocol.ErrExecutor,
				"container %s %s with code %d before its first step: %s",
				env.name, st.Status, st.ExitCode, orDefault(st.Error, "the engine gave no reason"))
		}
		if !pause() {
			if ctx.Err() != nil {
				return wrapErr(protocol.ErrExecutor, ctx.Err(), "waiting for container %s to run", env.name)
			}
			return codeErr(protocol.ErrExecutor,
				"the engine never readied container %s: not running with a pid %s after its start (%s)",
				env.name, e.readyBudget, describeState(st))
		}
	}

	attempts := 0
	for {
		attempts++
		out := &cappedBuffer{max: 4 << 10}
		execCtx, cancel := context.WithTimeout(ctx, readyExecTimeout)
		code, err := env.exec(execCtx, container.ExecOptions{
			User:         e.user,
			WorkingDir:   WorkspacePath,
			Cmd:          readyArgv,
			AttachStdout: true,
			AttachStderr: true,
		}, out, out)
		cancel()
		if err == nil && code == 0 {
			return nil
		}
		said := strings.TrimSpace(out.String())
		if err != nil {
			said = strings.TrimSpace(said + " " + err.Error())
		}
		if ctx.Err() != nil {
			return wrapErr(protocol.ErrExecutor, ctx.Err(), "waiting for container %s to take an exec", env.name)
		}
		if !strings.Contains(said, readyShellMissing) {
			if err != nil {
				return wrapErr(protocol.ErrExecutor, err, "readiness exec in container %s", env.name)
			}
			return codeErr(protocol.ErrExecutor,
				"readiness exec %q in container %s as user %q exited %d: %s",
				strings.Join(readyArgv, " "), env.name, e.user, code, orDefault(firstLine(said), "no output"))
		}
		if !pause() {
			if ctx.Err() != nil {
				return wrapErr(protocol.ErrExecutor, ctx.Err(), "waiting for container %s to take an exec", env.name)
			}
			return codeErr(protocol.ErrExecutor,
				"the engine never readied container %s: %d execs over %s after its start were answered %q",
				env.name, attempts, time.Since(start).Round(time.Millisecond), firstLine(said))
		}
	}
}

func describeState(st *container.State) string {
	if st == nil {
		return "no state"
	}
	return fmt.Sprintf("status %q, running %t, pid %d", st.Status, st.Running, st.Pid)
}

// cappedBuffer keeps the first max bytes written to it and drops the rest,
// so an exec that prints without end cannot grow the agent's memory.
type cappedBuffer struct {
	b   strings.Builder
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.b.String() }
