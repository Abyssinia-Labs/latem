package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// RunnerConfig is what the cache subsystem needs to know about one runner.
//
// It is a projection of the agent's config rather than an import of it: this
// package needs an executor kind and a disk floor, and taking the whole config
// type would make a leaf package depend on the thing that configures it.
type RunnerConfig struct {
	// Executor is "docker", "tart", or "native". It decides whether the
	// runner's caches are Docker volumes or host directories (§14.1, §14.2).
	Executor string
	// MinFreeGB is the runner's disk floor. Zero uses [DefaultMinFreeGB].
	MinFreeGB float64
}

// CommandRunner runs an external command and returns its combined output.
//
// The prune path shells out to `git` and `tart`. Routing that through a
// function makes those sweeps testable on a machine with neither installed,
// which matters because the alternative is a package whose most destructive
// code paths are only ever exercised by hand.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	// #nosec G204 -- name is a binary named in agent config ("git", "tart")
	// and args are built from directory entries under ~/.latem plus constant
	// flags. Nothing here is broker-supplied (INV-3), and the argv is a slice,
	// never a concatenated string.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options configures a [Local].
type Options struct {
	// Root is the agent home, ~/.latem. caches/, work/, mirrors/ and
	// artifacts/ hang off it.
	Root string
	// Runners maps runner name to its configuration. A runner missing here is
	// treated as directory-backed with the strictest disk floor, because the
	// alternative — assuming Docker — would create a volume no executor mounts
	// and hand the run a cache that silently does nothing.
	Runners map[string]RunnerConfig
	// Docker backs docker runners' caches with named volumes and enables build
	// layer pruning. Nil means Acquire fails for a docker runner rather than
	// quietly substituting a host directory the container will never see.
	Docker DockerAPI
	// LockTimeout bounds Acquire. Zero uses [DefaultLockTimeout].
	LockTimeout time.Duration
	// Live reports whether a run is still in flight. Prune consults it before
	// deleting a workspace or reaping a VM. Nil treats everything as finished,
	// which is safe only in combination with the age filters.
	Live func(runID string) bool
	// MirrorBusy reports whether a live run is reading a mirror. Mirrors have
	// gc.auto disabled (§12) precisely so nothing collects the commit a build
	// is checking out; running gc under one would undo that.
	MirrorBusy func(mirror string) bool
	// Git and Tart name the binaries used by the prune sweeps. Empty uses
	// "git" and "tart" from PATH.
	Git, Tart string
	// Exec runs those binaries. Nil uses os/exec.
	Exec CommandRunner
	// Now is the clock, for tests. Nil uses time.Now.
	Now func() time.Time
}

// Local implements [Manager] against this machine.
//
// One Local serves every runner on the agent. Caches are per (runner, cache) —
// never shared across runners — because two runs writing one ~/.gradle corrupt
// each other in ways that surface as intermittent build failures nobody
// attributes to caching (§14.3).
type Local struct {
	opts  Options
	paths paths
	locks *lockSet
}

var _ Manager = (*Local)(nil)

// New builds a cache manager rooted at opts.Root.
//
// It creates nothing. Directories appear when a run first asks for a cache, so
// an agent that never runs a job leaves no litter and a misconfigured root
// fails at the point it is used rather than at startup with a path nobody has
// looked at yet.
func New(opts Options) (*Local, error) {
	if opts.Root == "" {
		return nil, errors.New("cache: Root is required")
	}
	if !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("cache: Root %q must be absolute", opts.Root)
	}
	if opts.LockTimeout <= 0 {
		opts.LockTimeout = DefaultLockTimeout
	}
	if opts.Live == nil {
		opts.Live = func(string) bool { return false }
	}
	if opts.MirrorBusy == nil {
		opts.MirrorBusy = func(string) bool { return false }
	}
	if opts.Exec == nil {
		opts.Exec = execCommand
	}
	if opts.Git == "" {
		opts.Git = "git"
	}
	if opts.Tart == "" {
		opts.Tart = "tart"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Local{opts: opts, paths: paths{root: opts.Root}, locks: newLockSet()}, nil
}

func (l *Local) runner(name string) RunnerConfig {
	return l.opts.Runners[name]
}

// volumeBacked reports whether a runner's caches are Docker volumes.
func (l *Local) volumeBacked(runner string) bool {
	return l.runner(runner).Executor == ExecutorDocker
}

// Acquire prepares the named caches and returns their mounts.
//
// Each [Request] carries the mount point from the registry's caches[].path at
// the resolved commit (INV-5), which this package deliberately cannot read
// itself. An empty Target is refused rather than mounted nowhere: a cache that
// silently does not mount is invisible at every layer below, and the only
// symptom is a build that stays cold forever (amendment A11).
//
// Names are locked in sorted order. Two runs asking for {gradle, pnpm} and
// {pnpm, gradle} would otherwise deadlock for the full ten minutes and both
// fail, which is the kind of bug that only appears once the machine is busy.
func (l *Local) Acquire(ctx context.Context, runner string, reqs []Request) ([]executor.CacheMount, error) {
	if err := validateName("runner", runner); err != nil {
		return nil, cacheErr(err, "acquiring caches")
	}

	targets := make(map[string]string, len(reqs))
	names := make([]string, 0, len(reqs))
	for _, r := range reqs {
		if r.Target == "" {
			return nil, cacheErr(nil,
				"cache %q has no mount target; a cache that does not mount is a build that never warms", r.Name)
		}
		if prev, dup := targets[r.Name]; dup && prev != r.Target {
			return nil, cacheErr(nil,
				"cache %q requested with two mount targets, %q and %q", r.Name, prev, r.Target)
		}
		targets[r.Name] = r.Target
		names = append(names, r.Name)
	}

	wanted, err := sortedUnique(names)
	if err != nil {
		return nil, cacheErr(err, "acquiring caches for runner %s", runner)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	volumes := l.volumeBacked(runner)
	if volumes && l.opts.Docker == nil {
		return nil, cacheErr(nil,
			"runner %s is docker-backed but no docker client is configured; refusing to substitute a host directory the container cannot see",
			runner)
	}

	mounts := make([]executor.CacheMount, 0, len(wanted))
	held := make([]string, 0, len(wanted))

	// A partial acquisition is a leak: the caches locked so far would stay
	// locked for the life of the agent, and every later run would time out on
	// them. Unwind before returning.
	fail := func(err error) ([]executor.CacheMount, error) {
		for _, path := range held {
			_ = l.locks.release(path)
		}
		return nil, err
	}

	for _, name := range wanted {
		source, err := l.ensure(ctx, runner, name, volumes)
		if err != nil {
			return fail(err)
		}

		lockPath := l.paths.lockPath(runner, name, volumes)
		if err := l.locks.acquire(ctx, lockPath, l.opts.LockTimeout); err != nil {
			return fail(cacheErr(err, "cache %q on runner %s", name, runner))
		}
		held = append(held, lockPath)
		l.touch(lockPath)

		mounts = append(mounts, executor.CacheMount{Name: name, Source: source, Target: targets[name]})
	}
	return mounts, nil
}

// ensure creates the backing store for one cache and returns its mount source.
func (l *Local) ensure(ctx context.Context, runner, name string, volumes bool) (string, error) {
	if !volumes {
		dir := l.paths.cacheDir(runner, name)
		// 0o700: a cache holds a repository's dependency tree and, for
		// CocoaPods and Gradle, occasionally credentials a build wrote there.
		// It is not other users' business on a shared machine.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", cacheErr(err, "creating cache directory %s", dir)
		}
		return dir, nil
	}

	vol := VolumeName(runner, name)
	labels := map[string]string{
		LabelManaged: "true",
		LabelRunner:  runner,
		LabelCache:   name,
	}
	if err := l.opts.Docker.VolumeEnsure(ctx, vol, labels); err != nil {
		return "", cacheErr(err, "preparing cache %q on runner %s", name, runner)
	}
	return vol, nil
}

// touch records that a cache was used now.
//
// The stamp is the lock file's mtime rather than the cache directory's atime.
// Directory atime is not a usable signal — noatime and relatime are the common
// mount options, and a Docker volume's atime is inside the daemon's filesystem
// where the agent cannot read it at all. Writing our own stamp at Acquire
// means "last used" means the same thing for both backends.
func (l *Local) touch(lockPath string) {
	now := l.opts.Now()
	_ = os.Chtimes(lockPath, now, now)
}

// Release drops the locks taken by [Local.Acquire].
func (l *Local) Release(_ context.Context, runner string, names []string) error {
	if err := validateName("runner", runner); err != nil {
		// Nothing can have been locked under an invalid runner name, so there
		// is nothing to release and nothing to report.
		return nil // see comment
	}
	volumes := l.volumeBacked(runner)

	var errs []error
	for _, name := range names {
		if validateName("cache", name) != nil {
			continue
		}
		if err := l.locks.release(l.paths.lockPath(runner, name, volumes)); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return cacheErr(errors.Join(errs...), "releasing caches on runner %s", runner)
	}
	return nil
}

// List reports every known cache, directory-backed and volume-backed.
func (l *Local) List(ctx context.Context) ([]Info, error) {
	var (
		out  []Info
		errs []error
	)

	dirs, err := l.listDirs()
	if err != nil {
		errs = append(errs, err)
	}
	out = append(out, dirs...)

	if l.opts.Docker != nil {
		vols, err := l.listVolumes(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, vols...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Runner != out[j].Runner {
			return out[i].Runner < out[j].Runner
		}
		return out[i].Name < out[j].Name
	})

	if len(errs) > 0 {
		// The partial list is still returned: an operator running
		// `latem cache list` with a dead Docker daemon should see the
		// directory caches and an explanation, not one error.
		return out, cacheErr(errors.Join(errs...), "listing caches")
	}
	return out, nil
}

func (l *Local) listDirs() ([]Info, error) {
	runners, err := os.ReadDir(l.paths.caches())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cache: reading %s: %w", l.paths.caches(), err)
	}

	var out []Info
	for _, r := range runners {
		if !r.IsDir() || isDotName(r.Name()) {
			continue
		}
		runner := r.Name()
		if l.volumeBacked(runner) {
			continue // its caches are volumes; this directory holds only locks
		}
		caches, err := os.ReadDir(filepath.Join(l.paths.caches(), runner))
		if err != nil {
			return out, fmt.Errorf("cache: reading caches for runner %s: %w", runner, err)
		}
		for _, c := range caches {
			if !c.IsDir() || isDotName(c.Name()) {
				continue
			}
			dir := l.paths.cacheDir(runner, c.Name())
			out = append(out, Info{
				Name:       c.Name(),
				Runner:     runner,
				Source:     dir,
				SizeBytes:  treeSize(dir),
				LastUsedAt: l.lastUsed(l.paths.lockPath(runner, c.Name(), false)),
			})
		}
	}
	return out, nil
}

func (l *Local) listVolumes(ctx context.Context) ([]Info, error) {
	vols, err := l.opts.Docker.VolumeList(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(vols))
	for _, v := range vols {
		out = append(out, Info{
			Name:       v.Cache,
			Runner:     v.Runner,
			Source:     v.Name,
			SizeBytes:  v.SizeBytes,
			LastUsedAt: l.lastUsed(l.paths.lockPath(v.Runner, v.Cache, true)),
		})
	}
	return out, nil
}

func (l *Local) lastUsed(lockPath string) time.Time {
	info, err := os.Stat(lockPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// FreeGB reports free space on the filesystem holding the work directory, for
// the disk gates at RUNNER_SPEC §10 steps 5 and 17.
func (l *Local) FreeGB(context.Context) (float64, error) {
	// statfs needs an existing path, and work/ does not exist until the first
	// run. Walking up to the nearest existing ancestor answers the question
	// that was actually asked — how much room is on that filesystem — instead
	// of failing the gate on a fresh install.
	path, err := nearestExisting(l.paths.work())
	if err != nil {
		return 0, &Error{Code: protocol.ErrDisk, Msg: "locating the work directory", Err: err}
	}
	free, err := freeBytes(path)
	if err != nil {
		return 0, &Error{Code: protocol.ErrDisk, Msg: fmt.Sprintf("reading free space on %s", path), Err: err}
	}
	return float64(free) / (1 << 30), nil
}

// MinFreeGB is a runner's disk floor.
func (l *Local) MinFreeGB(runner string) float64 {
	cfg := l.runner(runner)
	if cfg.MinFreeGB > 0 {
		return cfg.MinFreeGB
	}
	return DefaultMinFreeGB(cfg.Executor)
}

// AgentFloorGB is the weakest floor across configured runners, for §10 gate 5.
//
// Gate 5 runs before a runner is selected, so the strictest floor would reject
// a Docker job on a machine that could run it perfectly well. Gate 17 re-checks
// the selected runner's own floor, which is where the Tart runner's 150 GB
// belongs.
func (l *Local) AgentFloorGB() float64 {
	floor := 0.0
	for name := range l.opts.Runners {
		f := l.MinFreeGB(name)
		if floor == 0 || f < floor {
			floor = f
		}
	}
	return floor
}

// CheckFree enforces a runner's floor, returning an E_DISK error below it.
//
// runner may be empty, which checks the agent-wide floor for §10 gate 5.
func (l *Local) CheckFree(ctx context.Context, runner string) error {
	free, err := l.FreeGB(ctx)
	if err != nil {
		return err
	}
	floor := l.AgentFloorGB()
	who := "the agent"
	if runner != "" {
		floor = l.MinFreeGB(runner)
		who = "runner " + runner
	}
	if free < floor {
		return diskErr("%.1f GB free, %s needs %.1f GB", free, who, floor)
	}
	return nil
}

// WithTargets fills each mount's Target from the registry's caches[].path.
//
// [Local.Acquire] cannot do this itself — see its doc comment — and a mount
// with an empty Target is a cache that is provisioned, locked, and mounted
// nowhere. That failure is silent at every layer below this one: the build
// simply runs cold, every time, and nobody notices for a month.
func WithTargets(mounts []executor.CacheMount, targets map[string]string) ([]executor.CacheMount, error) {
	out := make([]executor.CacheMount, 0, len(mounts))
	for _, m := range mounts {
		target := targets[m.Name]
		if target == "" {
			return nil, cacheErr(nil, "cache %q has no declared mount path", m.Name)
		}
		m.Target = target
		out = append(out, m)
	}
	return out, nil
}

func sortedUnique(names []string) ([]string, error) {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if err := validateName("cache", n); err != nil {
			return nil, err
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// nearestExisting walks up from path to the first directory that exists.
func nearestExisting(path string) (string, error) {
	for {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("cache: no existing ancestor of %s", path)
		}
		path = parent
	}
}
