package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// WorkspacePath is where the run's workspace is bind-mounted, and the
	// working directory of every step (§13.1). It is fixed rather than
	// configurable so a job's `run` script means the same thing on every
	// runner.
	WorkspacePath = "/workspace"

	// ContainerNamePrefix prefixes the container name, which is
	// latem-run-<run-id>. The run id is validated with protocol.ValidateID
	// before it is used here — see [Executor.Prepare] and amendment A6.
	ContainerNamePrefix = "latem-run-"

	// LabelRunID and LabelManaged are the labels startup reconciliation finds
	// containers by (§13.1). Losing them means a crashed agent cannot tell its
	// own leftovers from the operator's other containers, so they are applied
	// unconditionally and cannot be overridden by [executor.Spec.Labels].
	LabelRunID   = "latem.run-id"
	LabelManaged = "latem.managed"

	// DefaultPidsLimit is §13.1's `--pids-limit 4096`. It is a fork-bomb
	// backstop, not a resource budget: 4096 is far above what a build needs
	// and far below what takes a machine down.
	DefaultPidsLimit = 4096

	// UserImageDefault is the [Options.User] value meaning "run as whatever
	// user the image declares". See [resolveUser] for why this needs a
	// sentinel rather than the empty string.
	UserImageDefault = "image"

	// DefaultShell is the shell a step's `run` is handed to. INV-3: it is
	// handed over as a single argv element, never concatenated.
	DefaultShell = "bash"
)

// Options configures an [Executor]. The zero value is usable: it builds a
// client from the environment and uses safe defaults.
type Options struct {
	// Client is the daemon connection. Nil means "build one from the
	// DOCKER_* environment". Tests inject a fake.
	Client APIClient

	// DefaultImage is used for a run whose [executor.Spec.Image] is empty.
	DefaultImage string

	// Shell is the shell a step's `run` is passed to. Empty means bash, which
	// the base image contract requires anyway.
	Shell string

	// User is passed as the container user. Empty means auto: the invoking
	// uid:gid on Linux so bind-mounted workspace files stay owned by the
	// agent's user, and the image's own user everywhere else, because Docker
	// Desktop virtualises ownership in the VM's filesystem layer and the flag
	// is neither needed nor harmful there (§13.1). Set [UserImageDefault] to
	// force the image's user on Linux too.
	User string

	// PidsLimit overrides [DefaultPidsLimit]. Negative means unlimited.
	PidsLimit int64

	// GPU mirrors `runner.docker.gpu`. When set, every run gets all GPUs and
	// [Executor.Preflight] verifies the NVIDIA Container Toolkit at startup
	// rather than letting a build discover it is missing forty minutes in.
	GPU bool

	// RequireKVM marks a runner that takes Android emulator jobs (§18.2).
	// Preflight then checks /dev/kvm is present and usable by the daemon, and
	// every run gets the device.
	RequireKVM bool

	// ProbeImage is the small image throwaway probe containers use. Empty
	// means busybox.
	ProbeImage string

	// GPUProbeImage is the image `nvidia-smi` is run in during Preflight.
	// Empty means NVIDIA's own CUDA base image, which is what their
	// installation instructions tell an operator to test with. It must be a
	// glibc image: the toolkit injects the host's nvidia-smi binary, and that
	// binary is dynamically linked, so a musl image reports a missing loader
	// and looks like a toolkit failure when it is not.
	GPUProbeImage string

	// Init runs a real init process as PID 1 in the run container. Defaults to
	// on: PID 1 is otherwise the shell holding the container open, which does
	// not reap, so every daemon a build starts and abandons becomes a zombie
	// for the life of the run.
	Init *bool
}

// Executor provisions one container per run. One Executor serves many runs and
// is safe for concurrent use.
type Executor struct {
	api  APIClient
	opts Options

	shell         string
	user          string
	pidsLimit     int64
	probeImage    string
	gpuProbeImage string
	init          bool
}

var _ executor.Executor = (*Executor)(nil)

// New builds an Executor. It does not talk to the daemon — that is
// [Executor.Preflight]'s job, which the agent calls at startup and every five
// minutes after (§20).
func New(opts Options) (*Executor, error) {
	api := opts.Client
	if api == nil {
		var err error
		if api, err = newSDKClient(); err != nil {
			return nil, err
		}
	}

	e := &Executor{
		api:           api,
		opts:          opts,
		shell:         orDefault(opts.Shell, DefaultShell),
		user:          resolveUser(opts.User, runtime.GOOS),
		pidsLimit:     opts.PidsLimit,
		probeImage:    orDefault(opts.ProbeImage, "busybox:latest"),
		gpuProbeImage: orDefault(opts.GPUProbeImage, "nvidia/cuda:12.4.1-base-ubuntu22.04"),
		init:          opts.Init == nil || *opts.Init,
	}
	if e.pidsLimit == 0 {
		e.pidsLimit = DefaultPidsLimit
	}
	return e, nil
}

// Name identifies this executor in config and in `runs_on` resolution.
func (e *Executor) Name() string { return "docker" }

// Close releases the daemon connection. It is not part of [executor.Executor];
// the agent calls it at shutdown.
func (e *Executor) Close() error { return e.api.Close() }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// resolveUser decides the container user (§13.1).
//
// On Linux the agent passes the invoking uid:gid so files a job writes into
// the bind-mounted workspace are owned by the user that will later archive and
// delete them. On macOS Docker Desktop, ownership is virtualised by the VM's
// filesystem layer, so the flag buys nothing and forcing a uid that has no
// /etc/passwd entry actively breaks the image contract's writable $HOME.
//
// The empty string cannot mean "image default" because it already means "not
// set, decide for me", hence [UserImageDefault].
func resolveUser(opt, goos string) string {
	switch {
	case opt == UserImageDefault:
		return ""
	case opt != "":
		return opt
	case goos == "linux":
		return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	default:
		return ""
	}
}

// Prepare provisions the run's container and returns its [executor.Env].
func (e *Executor) Prepare(ctx context.Context, spec executor.Spec) (executor.Env, error) {
	// A6: the run id becomes a container name here and a filesystem path
	// elsewhere. Validate before building either, not after.
	if err := protocol.ValidateID(spec.RunID); err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "run id")
	}
	if !path.IsAbs(spec.Workspace) {
		return nil, codeErr(protocol.ErrExecutor, "workspace %q is not an absolute path", spec.Workspace)
	}
	info, err := os.Stat(spec.Workspace)
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "workspace %q", spec.Workspace)
	}
	if !info.IsDir() {
		return nil, codeErr(protocol.ErrExecutor, "workspace %q is not a directory", spec.Workspace)
	}

	img := orDefault(spec.Image, e.opts.DefaultImage)
	if img == "" {
		return nil, codeErr(protocol.ErrExecutor, "no image: neither the job nor the runner names one")
	}
	if err := validateCacheMounts(spec.Caches); err != nil {
		return nil, err
	}

	memBytes, err := parseMemory(spec.Resources.Memory)
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "resources.memory")
	}

	if err := e.ensureImage(ctx, img); err != nil {
		return nil, err
	}
	mounts, err := e.ensureMounts(ctx, spec)
	if err != nil {
		return nil, err
	}

	cfg := &container.Config{
		Image:      img,
		WorkingDir: WorkspacePath,
		Labels:     buildLabels(spec.RunID, spec.Labels),
		User:       e.user,
		// The container's own process does nothing: it exists to hold the
		// namespaces and the filesystem open while steps exec into it. A `sh`
		// loop rather than `sleep infinity` because busybox's sleep has not
		// always accepted that argument, and rather than `tail -f /dev/null`
		// because not every minimal image ships tail.
		Entrypoint: []string{"/bin/sh", "-c", "while :; do sleep 86400; done"},
		Cmd:        nil,
	}

	hostCfg := &container.HostConfig{
		Mounts: mounts,
		Resources: container.Resources{
			NanoCPUs:  int64(spec.Resources.CPUs * 1e9),
			Memory:    memBytes,
			PidsLimit: pidsLimitPtr(e.pidsLimit),
		},
	}
	if e.init {
		on := true
		hostCfg.Init = &on
	}
	if spec.Resources.GPU || e.opts.GPU {
		hostCfg.DeviceRequests = allGPUs()
	}
	if e.opts.RequireKVM {
		hostCfg.Devices = append(hostCfg.Devices, kvmDevice())
	}

	name := ContainerNamePrefix + spec.RunID
	created, err := e.api.ContainerCreate(ctx, cfg, hostCfg, name)
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "create container %s", name)
	}

	env := &runEnv{
		api:         e.api,
		containerID: created.ID,
		name:        name,
		spec:        spec,
		shell:       e.shell,
		user:        e.user,
		carried:     map[string]string{},
	}

	if err := e.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Do not leave the created-but-never-started container behind: it
		// carries the run's labels and would be mistaken for a live run by
		// reconciliation.
		_ = env.Close(context.WithoutCancel(ctx))
		return nil, wrapErr(protocol.ErrExecutor, err, "start container %s", name)
	}

	// §9's between-step channel has to exist before the first step runs, or
	// `>> "$LATEM_ENV"` fails on a job's very first use of it.
	if err := env.createEnvFile(ctx, EnvFilePath); err != nil {
		_ = env.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return env, nil
}

// pidsLimitPtr converts the option to the SDK's tri-state pointer: nil leaves
// the daemon default alone, and a negative value means unlimited.
func pidsLimitPtr(n int64) *int64 {
	if n < 0 {
		unlimited := int64(-1)
		return &unlimited
	}
	v := n
	return &v
}

// allGPUs is the API equivalent of `--gpus all`.
func allGPUs() []container.DeviceRequest {
	return []container.DeviceRequest{{
		Driver:       "nvidia",
		Count:        -1,
		Capabilities: [][]string{{"gpu"}},
	}}
}

// kvmDevice is `--device /dev/kvm`, which an Android emulator needs to run
// with hardware acceleration (§18.2). Without it the emulator either refuses
// to boot or falls back to software rendering and times out.
func kvmDevice() container.DeviceMapping {
	return container.DeviceMapping{
		PathOnHost:        "/dev/kvm",
		PathInContainer:   "/dev/kvm",
		CgroupPermissions: "rwm",
	}
}

// buildLabels merges the spec's labels with the two this package guarantees.
//
// Ours are applied last and win. A run whose labels could shadow
// latem.managed=true would be invisible to startup reconciliation, which is
// the one thing standing between a crashed agent and a machine full of
// abandoned containers.
func buildLabels(runID string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(extra)+2)
	for k, v := range extra {
		out[k] = v
	}
	out[LabelRunID] = runID
	out[LabelManaged] = "true"
	return out
}

// validateCacheMounts rejects cache targets that would break the run rather
// than help it.
//
// A cache mounted at or inside /workspace shadows the git clone §12 went to
// some trouble to produce, and the resulting "my repo is empty" failure points
// nowhere near the cache declaration that caused it. A relative target is a
// registry authoring mistake the daemon would reject with a less helpful
// message.
func validateCacheMounts(caches []executor.CacheMount) error {
	seen := make(map[string]string, len(caches))
	for _, c := range caches {
		if c.Source == "" {
			return codeErr(protocol.ErrCache, "cache %q has no source", c.Name)
		}
		if !path.IsAbs(c.Target) {
			return codeErr(protocol.ErrCache, "cache %q target %q is not an absolute path", c.Name, c.Target)
		}
		target := path.Clean(c.Target)
		if target == "/" {
			return codeErr(protocol.ErrCache, "cache %q would mount over /", c.Name)
		}
		if target == WorkspacePath || strings.HasPrefix(target, WorkspacePath+"/") {
			return codeErr(protocol.ErrCache,
				"cache %q target %q is inside %s and would shadow the workspace", c.Name, c.Target, WorkspacePath)
		}
		if other, dup := seen[target]; dup {
			return codeErr(protocol.ErrCache, "caches %q and %q both mount at %q", other, c.Name, c.Target)
		}
		seen[target] = c.Name
	}
	return nil
}

// ensureMounts builds the mount set and creates any named cache volume that
// does not exist yet.
//
// Caches are mounted, never archived (§14): there is no save step and no
// restore step, so a volume that does not exist simply gets created empty and
// the first run that uses it warms it.
func (e *Executor) ensureMounts(ctx context.Context, spec executor.Spec) ([]mount.Mount, error) {
	mounts := []mount.Mount{{
		Type:   mount.TypeBind,
		Source: spec.Workspace,
		Target: WorkspacePath,
	}}

	for _, c := range spec.Caches {
		// §14: CacheMount.Source is "the host volume name or directory". An
		// absolute path is a directory (the tart/native shape, §14.2);
		// anything else names a Docker volume (§14.1).
		if path.IsAbs(c.Source) {
			mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: c.Source, Target: c.Target})
			continue
		}
		if _, err := e.api.VolumeCreate(ctx, volume.CreateOptions{
			Name:   c.Source,
			Labels: map[string]string{LabelManaged: "true"},
		}); err != nil {
			return nil, wrapErr(protocol.ErrCache, err, "cache volume %q", c.Source)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: c.Source, Target: c.Target})
	}
	return mounts, nil
}

// ensureImage pulls the image if the daemon does not already have it.
//
// It deliberately does not pull when the image is present, even for a moving
// tag. A run that silently picks up a new base image mid-afternoon is a
// reproducibility problem, and §8's operator owns the image lifecycle.
func (e *Executor) ensureImage(ctx context.Context, ref string) error {
	if _, err := e.api.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !isNotFound(err) {
		return wrapErr(protocol.ErrExecutor, err, "inspect image %q", ref)
	}

	rc, err := e.api.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return wrapErr(protocol.ErrExecutor, err, "pull image %q", ref)
	}
	defer func() { _ = rc.Close() }()
	// The pull is only complete when the progress stream ends; discarding it
	// without draining it cancels the pull.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "pull image %q", ref)
	}
	return nil
}

// Container is one container this package created, as reported by
// [Executor.Discover].
type Container struct {
	ID    string
	Name  string
	RunID string
	State string
}

// Discover lists every container this package created, running or not.
//
// This is the reconciliation entry point (§13.1, §11): an agent that died
// mid-run has no memory of the containers it started, and the labels are the
// only durable record. A run found here with no live state is E_INTERRUPTED.
func (e *Executor) Discover(ctx context.Context) ([]Container, error) {
	summaries, err := e.api.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelManaged+"=true")),
	})
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "list containers")
	}
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		out = append(out, Container{ID: s.ID, Name: name, RunID: s.Labels[LabelRunID], State: s.State})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// Remove force-removes a container by id. Reconciliation uses it to clean up
// after a crash; a live run uses [runEnv.Close].
func (e *Executor) Remove(ctx context.Context, id string) error {
	if err := e.api.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return wrapErr(protocol.ErrExecutor, err, "remove container %s", id)
	}
	return nil
}

// Preflight verifies the machine can actually execute the work this runner
// accepts (§20).
//
// The point is the timing, not the checks: a runner that discovers at minute
// forty of an iOS build that the NVIDIA toolkit is missing has already wasted
// forty minutes and produced a failure that looks like the job's fault. A
// failed Preflight marks the runner unavailable instead, and recovery is
// automatic when the next one passes.
func (e *Executor) Preflight(ctx context.Context) error {
	if _, err := e.api.Ping(ctx); err != nil {
		return wrapErr(protocol.ErrExecutor, err, "docker daemon unreachable")
	}
	if e.opts.GPU {
		if err := e.preflightGPU(ctx); err != nil {
			return err
		}
	}
	if e.opts.RequireKVM {
		if err := e.preflightKVM(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) preflightGPU(ctx context.Context) error {
	res, err := e.runProbe(ctx, probeSpec{
		image: e.gpuProbeImage,
		cmd:   []string{"nvidia-smi"},
		mutate: func(hc *container.HostConfig) {
			hc.DeviceRequests = allGPUs()
		},
	})
	if err != nil {
		return wrapErr(protocol.ErrExecutor, err, "gpu preflight: nvidia-smi in %s", e.gpuProbeImage)
	}
	if res.exitCode != 0 {
		return codeErr(protocol.ErrExecutor,
			"gpu preflight: nvidia-smi exited %d — is the NVIDIA Container Toolkit installed and the daemon restarted? output: %s",
			res.exitCode, firstLine(res.output))
	}
	return nil
}

func (e *Executor) preflightKVM(ctx context.Context) error {
	res, err := e.runProbe(ctx, probeSpec{
		image: e.probeImage,
		// Readable and writable: the emulator opens /dev/kvm read-write, and a
		// read-only check would pass on a machine where it still fails.
		cmd: []string{"/bin/sh", "-c", "test -r /dev/kvm && test -w /dev/kvm"},
		mutate: func(hc *container.HostConfig) {
			hc.Devices = append(hc.Devices, kvmDevice())
		},
	})
	if err != nil {
		return wrapErr(protocol.ErrExecutor, err, "kvm preflight: /dev/kvm")
	}
	if res.exitCode != 0 {
		return codeErr(protocol.ErrExecutor,
			"kvm preflight: /dev/kvm is not readable and writable by the daemon (exit %d) — "+
				"an Android emulator job on this runner would fall back to software rendering and time out",
			res.exitCode)
	}
	return nil
}

// probeSpec describes a throwaway container run to completion.
type probeSpec struct {
	image  string
	cmd    []string
	user   string
	mutate func(*container.HostConfig)
}

type probeResult struct {
	exitCode int
	output   string
}

// runProbe runs one short-lived container and returns its exit code and
// combined output.
//
// Probe containers carry latem.managed=true like any other, so if the agent is
// killed between create and remove, reconciliation still finds them. They
// carry no run id, which is how a reaper tells them apart from a real run.
func (e *Executor) runProbe(ctx context.Context, p probeSpec) (probeResult, error) {
	if err := e.ensureImage(ctx, p.image); err != nil {
		return probeResult{}, err
	}

	hostCfg := &container.HostConfig{}
	if p.mutate != nil {
		p.mutate(hostCfg)
	}
	created, err := e.api.ContainerCreate(ctx, &container.Config{
		Image:  p.image,
		Cmd:    p.cmd,
		User:   p.user,
		Labels: map[string]string{LabelManaged: "true", "latem.probe": "true"},
	}, hostCfg, "")
	if err != nil {
		return probeResult{}, fmt.Errorf("create probe container: %w", err)
	}
	defer func() {
		_ = e.api.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}()

	if err := e.api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return probeResult{}, fmt.Errorf("start probe container: %w", err)
	}

	statusCh, errCh := e.api.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var exitCode int
	select {
	case err := <-errCh:
		if err != nil {
			return probeResult{}, fmt.Errorf("wait for probe container: %w", err)
		}
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
	case <-ctx.Done():
		return probeResult{}, ctx.Err()
	}

	logs, err := e.api.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return probeResult{}, fmt.Errorf("probe container logs: %w", err)
	}
	defer func() { _ = logs.Close() }()
	out, err := demux(logs)
	if err != nil {
		return probeResult{}, fmt.Errorf("probe container logs: %w", err)
	}
	return probeResult{exitCode: exitCode, output: out}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseMemory converts §8's memory strings ("8g", "512m") to bytes.
//
// Written here rather than pulled from docker/go-units because it is fifteen
// lines, it is on the path that decides whether a run is admitted at all, and
// a unit test of it does not need a daemon. Zero and empty mean "no limit",
// matching Docker's own reading of an unset --memory.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	// Accept both "8g" and "8gb"; the reference config writes the short form
	// but operators type both.
	lower = strings.TrimSuffix(lower, "b")

	mult := int64(1)
	if len(lower) > 0 {
		switch lower[len(lower)-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
		if mult != 1 {
			lower = lower[:len(lower)-1]
		}
	}
	if lower == "" {
		return 0, fmt.Errorf("%q has a unit but no number", s)
	}
	n, err := strconv.ParseFloat(lower, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a memory size like 8g or 512m", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return int64(n * float64(mult)), nil
}
