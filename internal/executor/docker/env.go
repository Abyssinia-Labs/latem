package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/internal/workflow/ghcmd"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runEnv is one run's container. It satisfies [executor.Env].
type runEnv struct {
	api         APIClient
	containerID string
	name        string
	spec        executor.Spec
	shell       string
	user        string

	mu      sync.Mutex
	carried map[string]string // accumulated $LATEM_ENV assignments
	// pathAdds is what steps appended to $GITHUB_PATH so far, first entry
	// first on PATH. imagePath is the image's own PATH, captured at Prepare,
	// which the additions go in front of.
	pathAdds  []string
	imagePath string
	closed    bool
	removed   bool

	// A44: what a container step is built from, and the one in flight.
	runVolume     string
	mounts        []mount.Mount
	resources     container.Resources
	init          *bool
	labels        map[string]string
	ensureImage   func(ctx context.Context, ref string) error
	requirePinned bool
	stepSeq       int
	sidecar       string
}

var _ executor.Env = (*runEnv)(nil)

// ContainerID exposes the container this run is using, for diagnostics and for
// reconciliation bookkeeping. Not part of [executor.Env].
func (e *runEnv) ContainerID() string { return e.containerID }

// EnvFile is the container-visible path exported as $LATEM_ENV.
//
// The orchestrator needs this to describe §9's between-step channel, and only
// the executor knows it: a container path here, a guest path for tart, a host
// path for native. Draft 02 put it on [executor.Step] and had the caller fill
// it, which forced the orchestrator to import a concrete executor package to
// get it right — defeating the interface (amendment A9).
func (e *runEnv) EnvFile() string { return EnvFilePath }

// Exec runs one step.
//
// # This is finding F1
//
// Every call is a fresh `docker exec`, which is a fresh process. Nothing the
// previous step did to its own process survives: not `cd`, not `export`, not a
// shell function, not a trap. So this method sets WorkingDir explicitly and
// passes the complete merged environment explicitly, every time, rather than
// relying on anything already in the container. The filesystem is the only
// thing that carries, and it carries for the whole run.
//
// Draft 01 of RUNNER_SPEC said the opposite, and the first job author to write
// `export PATH=...` in one step and read it in the next would have filed the
// bug. The integration test TestExec_ShellStateDoesNotLeakButFilesDo exists to
// stop the claim quietly becoming true again.
//
// # INV-3
//
// [executor.Step.Run] is handed to the shell as a single argv element —
// []string{"bash", "-c", step.Run}. It is never concatenated into a command
// string, because a concatenation is where an injection lives.
//
// A step that exits non-zero is not an error: the exit code is the result, and
// the agent maps a non-zero one to E_STEP. An error means the step could not
// be run or could not be waited for.
func (e *runEnv) Exec(ctx context.Context, step executor.Step, stdout, stderr io.Writer) (executor.Result, error) {
	none := executor.Result{ExitCode: -1}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return none, codeErr(protocol.ErrExecutor, "environment for run %s is closed", e.spec.RunID)
	}
	envFile := EnvFilePath
	env := mergeEnv(e.spec.Env, e.carried, step.Env)
	// Both set after the merge, so that a job which wrote either name into
	// $LATEM_ENV cannot point the next step's bookkeeping somewhere else.
	env["LATEM_ENV"] = envFile
	env[executor.GitHubEnvVar] = envFile
	env[executor.OutputFileVar] = OutputFilePath
	env[executor.PathFileVar] = PathFilePath
	env[executor.SummaryFileVar] = SummaryFilePath
	if len(e.pathAdds) > 0 {
		// In front of whatever PATH the step would otherwise have had: one
		// set explicitly, else the image's. Setting PATH in the exec's env
		// replaces the image's outright, which is why the image's is kept.
		current, set := env["PATH"]
		if !set {
			current = e.imagePath
		}
		env["PATH"] = ghcmd.PrependPath(e.pathAdds, current)
	}
	e.mu.Unlock()

	// A zero Timeout means "whatever remains of the job budget", which is
	// already the deadline on ctx. Only a positive one tightens it.
	execCtx := ctx
	if step.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, step.Timeout)
		defer cancel()
	}

	var (
		code int
		err  error
	)
	if step.Container != nil {
		code, err = e.runContainerStep(execCtx, step, env, stdout, stderr)
	} else {
		code, err = e.exec(execCtx, container.ExecOptions{
			User:         e.user,
			WorkingDir:   WorkspacePath,
			Env:          envSlice(env),
			Cmd:          StepArgv(e.shell, step.Run),
			AttachStdout: true,
			AttachStderr: true,
		}, stdout, stderr)
	}

	// §9's between-step channel. Merged after the step rather than before the
	// next one so that the warnings land in the log of the step that caused
	// them, which is the only place an author will look.
	if mergeErr := e.mergeEnvFile(ctx, envFile, stdout); mergeErr != nil && err == nil {
		return executor.Result{ExitCode: code}, mergeErr
	}
	// Outputs are read back even from a failed step: GitHub keeps them, and a
	// later `if: failure()` step reading `steps.build.outputs.log` is the
	// idiom that depends on it.
	outputs, outErr := e.readOutputs(ctx, stdout)
	if outErr != nil && err == nil {
		return executor.Result{ExitCode: code}, outErr
	}
	if psErr := e.readPathAndSummary(ctx, stdout); psErr != nil && err == nil {
		return executor.Result{ExitCode: code}, psErr
	}

	if err != nil {
		// A step-level deadline that fired while the job budget still had room
		// is E_STEP_TIMEOUT, not E_TIMEOUT (§21).
		if errors.Is(execCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// An exec cannot be killed on its own, so a timed-out command
			// takes the run container with it. A container step already
			// killed and removed its own container, and the run's survives.
			if step.Container == nil {
				e.killContainer(ctx)
			}
			return none, wrapErr(protocol.ErrStepTimeout, err,
				"step %q exceeded its %s timeout", step.Name, step.Timeout)
		}
		return none, wrapErr(protocol.ErrExecutor, err, "step %q", step.Name)
	}
	return executor.Result{ExitCode: code, Outputs: outputs}, nil
}

// StepArgv builds the argv a step is executed with.
//
// INV-3 in one function: `run` is the third element of the slice, whole and
// untouched. There is no place in this package where a step's text is joined
// to anything, and the unit test asserts exactly that, because the failure
// this prevents — a job whose `run` contains a quote reaching a shell as
// syntax — is silent until it is a breach.
func StepArgv(shell, run string) []string {
	if shell == "" {
		shell = DefaultShell
	}
	return []string{shell, "-c", run}
}

// exec creates, attaches to, streams and reaps one exec.
func (e *runEnv) exec(ctx context.Context, opts container.ExecOptions, stdout, stderr io.Writer) (int, error) {
	created, err := e.api.ContainerExecCreate(ctx, e.containerID, opts)
	if err != nil {
		return -1, fmt.Errorf("create exec: %w", err)
	}
	attached, err := e.api.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, fmt.Errorf("attach exec: %w", err)
	}
	defer attached.Close()

	// The hijacked connection is not bound to ctx once the upgrade completes,
	// so a cancelled context does not by itself unblock the read. Closing the
	// response does, which is why the copy runs in its own goroutine.
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		copyDone <- copyErr
	}()

	select {
	case copyErr := <-copyDone:
		if copyErr != nil {
			return -1, fmt.Errorf("stream step output: %w", copyErr)
		}
	case <-ctx.Done():
		attached.Close()
		<-copyDone
		return -1, ctx.Err()
	}

	code, err := e.waitExec(ctx, created.ID)
	if err != nil {
		return -1, err
	}
	return code, nil
}

// waitExec reads the exec's exit code.
//
// The output stream closing does not strictly guarantee the daemon has reaped
// the process, so a short poll covers the window where Running is still true.
// Returning a wrong exit code here would report a failed build as green, which
// is the worst failure this package could have.
func (e *runEnv) waitExec(ctx context.Context, execID string) (int, error) {
	const (
		attempts = 100
		interval = 20 * time.Millisecond
	)
	for i := 0; i < attempts; i++ {
		inspect, err := e.api.ContainerExecInspect(ctx, execID)
		if err != nil {
			return -1, fmt.Errorf("inspect exec: %w", err)
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(interval):
		}
	}
	return -1, fmt.Errorf("exec %s still running after its output stream closed", execID)
}

// killContainer stops everything the timed-out step started.
//
// The Docker API has no "kill this exec" call, and the pid an exec inspect
// reports is in the daemon's namespace, not the container's, so there is
// nothing precise to signal. Killing the container is the only way to reach a
// step's whole process tree — and it is safe here, because E_STEP_TIMEOUT is
// terminal (§21) and artifacts live on the bind-mounted workspace, which
// outlives the container.
func (e *runEnv) killContainer(ctx context.Context) {
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = e.api.ContainerKill(killCtx, e.containerID, "KILL")
}

// mergeEnv layers the three environment sources in the order §9 gives them.
//
// Spec.Env is the run environment including resolved secrets; the accumulated
// $LATEM_ENV assignments sit on top of it, because that is what "merges them
// into the run environment for every subsequent step" means; Step.Env is last
// because it is declared in the job for this step specifically, so it must be
// able to override both.
//
// Keys that cannot survive being rendered as KEY=value are dropped rather than
// silently corrupting the whole environment block.
func mergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, layer := range layers {
		for k, v := range layer {
			if !usableEnvKey(k) {
				continue
			}
			out[k] = v
		}
	}
	return out
}

func usableEnvKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}

// envSlice renders an environment as the KEY=value slice the API wants,
// sorted so that two identical runs produce identical exec configurations and
// a test can assert on them.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Close removes the run's container.
//
// Idempotent, and it retries after a failure: the second call is not a no-op
// unless the first one actually removed something. §13.1 deliberately does not
// use `--rm`, so this is the only thing standing between a busy machine and an
// accumulation of stopped containers, and "we already tried" is not a reason
// to stop trying.
func (e *runEnv) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	alreadyRemoved := e.removed
	e.mu.Unlock()

	if alreadyRemoved {
		return nil
	}

	e.mu.Lock()
	sidecar := e.sidecar
	e.mu.Unlock()
	if sidecar != "" {
		_ = e.api.ContainerRemove(ctx, sidecar, container.RemoveOptions{Force: true})
	}

	err := e.api.ContainerRemove(ctx, e.containerID, container.RemoveOptions{Force: true})
	if err != nil && !isNotFound(err) {
		return wrapErr(protocol.ErrExecutor, err, "remove container %s", e.name)
	}
	// After the containers, because a volume in use cannot be removed. One
	// that is already gone is success, as it is for the container.
	if e.runVolume != "" {
		if err := e.api.VolumeRemove(ctx, e.runVolume, true); err != nil && !isNotFound(err) {
			return wrapErr(protocol.ErrExecutor, err, "remove run volume %s", e.runVolume)
		}
	}

	e.mu.Lock()
	e.removed = true
	e.mu.Unlock()
	return nil
}

// demux converts a multiplexed Docker stream into a plain string.
func demux(r io.Reader) (string, error) {
	var buf strings.Builder
	if _, err := stdcopy.StdCopy(&buf, &buf, r); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// runContainerStep runs a `uses: docker://…` step (A44): one container from
// the step's image, a sibling of the run container rather than a stranger to
// it. It gets the same workspace, the same caches and the per-run volume, so
// $GITHUB_OUTPUT and the rest are the same files; the same resource limits;
// and the same labels, so §6's crash reaping finds it like any other.
//
// It runs as the image's own user, not the step user. The image is third-party
// and was built expecting that; and what it can reach is the workspace and one
// volume, which is a good deal narrower than what a step on the native
// executor can.
func (e *runEnv) runContainerStep(
	ctx context.Context, step executor.Step, env map[string]string, stdout, stderr io.Writer,
) (int, error) {
	c := step.Container
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	if e.requirePinned && !strings.Contains(c.Image, "@sha256:") {
		return -1, codeErr(protocol.ErrExecutor,
			"step %q: image %q is named by tag, and this runner sets require_pinned_images; "+
				"name it by digest (image@sha256:…)", step.Name, c.Image)
	}
	if e.ensureImage != nil {
		if err := e.ensureImage(ctx, c.Image); err != nil {
			return -1, err
		}
	}
	// A tag is a mutable reference, so the run says what it resolved to. This
	// is the record INV-5 cannot give for anything but a digest.
	if insp, err := e.api.ImageInspect(ctx, c.Image); err == nil {
		resolved := insp.ID
		if len(insp.RepoDigests) > 0 {
			resolved = insp.RepoDigests[0]
		}
		if resolved != "" {
			writeNote(stdout, fmt.Sprintf("image %s is %s", c.Image, resolved))
		}
	}

	labels := make(map[string]string, len(e.labels)+1)
	for k, v := range e.labels {
		labels[k] = v
	}
	labels["latem.step"] = step.Name

	cfg := &container.Config{
		Image:      c.Image,
		Env:        envSlice(env),
		WorkingDir: WorkspacePath,
		Labels:     labels,
		// An argv, element for element (INV-3). Nothing here is joined.
		Cmd: c.Args,
	}
	if c.Entrypoint != "" {
		cfg.Entrypoint = []string{c.Entrypoint}
	}
	host := &container.HostConfig{Mounts: e.mounts, Resources: e.resources, Init: e.init}

	e.mu.Lock()
	e.stepSeq++
	name := fmt.Sprintf("%s-step-%d", e.name, e.stepSeq)
	e.mu.Unlock()

	created, err := e.api.ContainerCreate(ctx, cfg, host, name)
	if err != nil {
		return -1, fmt.Errorf("create container %s: %w", name, err)
	}
	e.mu.Lock()
	e.sidecar = created.ID
	e.mu.Unlock()
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = e.api.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true})
		e.mu.Lock()
		e.sidecar = ""
		e.mu.Unlock()
	}()

	if err := e.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("start container %s: %w", name, err)
	}

	logs, err := e.api.ContainerLogs(ctx, created.ID, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if err != nil {
		return -1, fmt.Errorf("attach logs of %s: %w", name, err)
	}
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = stdcopy.StdCopy(stdout, stderr, logs)
	}()
	defer func() { _ = logs.Close() }()

	statusCh, errCh := e.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case st := <-statusCh:
		// The container has exited, so the log stream ends on its own; wait
		// for it, bounded, so the last lines are in the log before the step's
		// closing note.
		select {
		case <-copyDone:
		case <-time.After(5 * time.Second):
		}
		if st.Error != nil && st.Error.Message != "" {
			return -1, fmt.Errorf("container %s: %s", name, st.Error.Message)
		}
		return int(st.StatusCode), nil
	case err := <-errCh:
		return -1, fmt.Errorf("wait for %s: %w", name, err)
	case <-ctx.Done():
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		_ = e.api.ContainerKill(killCtx, created.ID, "KILL")
		return -1, ctx.Err()
	}
}
