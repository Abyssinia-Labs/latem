package native

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/executor"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const (
	// DefaultShell is the shell a step's `run` is handed to. INV-3: it is
	// handed over as a single argv element, never concatenated.
	DefaultShell = "bash"

	// EnvDirPrefix begins the per-run directory holding $LATEM_ENV. The run id
	// completes it, which is why [Executor.Prepare] validates the id before
	// building any path from it (amendment A6).
	EnvDirPrefix = "latem-env-"
)

// Options configures an [Executor]. The zero value is usable.
type Options struct {
	// Shell is the shell a step's `run` is passed to. Empty means bash, which
	// is the only shell §13.3's `bash -lc` contract is written for.
	Shell string

	// EnvFileDir is where the per-run $LATEM_ENV directory is created. Empty
	// means os.TempDir.
	//
	// It is never the workspace: the workspace is a git clone (§12) and a file
	// appearing in it would show up in `git status` and eventually in a commit.
	// On macOS os.TempDir is the user's own $TMPDIR, which is already private
	// to them, so the default needs no further defending.
	EnvFileDir string

	// Runner executes the host probes [Executor.Preflight] uses. Nil uses
	// [ExecRunner]; tests inject a fake so the daemon-context and locked-
	// keychain branches are reachable on a machine in neither state.
	Runner Runner

	// RequireGUISession makes Preflight refuse any context that cannot sign
	// code, not just a LaunchDaemon. Set it on a runner that takes iOS work:
	// it converts "every signing step fails for a reason the log does not
	// mention" into "this runner is unavailable, and here is the sentence that
	// explains why" (§23). See [CheckSession].
	RequireGUISession bool

	// CheckKeychain makes Preflight probe the keychain lock state. It defaults
	// to off, and [CheckKeychain] documents at length why a check this useful
	// is not simply always on: the probe can raise a password dialog that
	// outlives it, and §20 calls Preflight every five minutes.
	CheckKeychain bool

	// Keychain is the keychain [CheckKeychain] probes. Empty means the user's
	// default keychain, which is what a signing build uses.
	Keychain string

	// TerminateGrace overrides [TerminateGrace], the pause between SIGTERM and
	// SIGKILL when a step's process group is stopped.
	TerminateGrace time.Duration

	// ReapGrace overrides [ReapTerminateGrace], the same pause for a process
	// group [Executor.Reap] found abandoned by a crashed agent. It is separate
	// from TerminateGrace because a reap happens at startup with the runner
	// unavailable and nobody watching the log; see [ReapTerminateGrace].
	ReapGrace time.Duration

	// PipeDrainDelay overrides [DefaultPipeDrainDelay], the pause between a
	// step's shell exiting and the agent closing the log pipes that something
	// the step abandoned is still holding open.
	PipeDrainDelay time.Duration
}

// Executor runs every step of a run directly on this host. One Executor serves
// many runs and is safe for concurrent use — though §13.3 requires the agent to
// give it only one run at a time; see the package doc.
type Executor struct {
	opts Options

	shell     string
	shellErr  error
	envDir    string
	grace     time.Duration
	reapGrace time.Duration
	drain     time.Duration

	// goos and uid are fields rather than direct calls so Preflight's
	// platform-specific and root-only branches are testable.
	goos string
	uid  int
}

var _ executor.Executor = (*Executor)(nil)

// New builds an Executor.
//
// It resolves the shell here rather than at each step, for the reason
// source.lookGit resolves git at startup: a machine without bash should fail
// its first preflight, not the eleventh minute of the first build, and every
// later exec is then handed an absolute path that cannot be re-resolved against
// a PATH that changed underneath it.
//
// A missing shell is remembered rather than returned. An agent whose config
// names a native runner should start, mark that runner unavailable through
// [Executor.Preflight] (§20), and recover on its own when the operator installs
// bash — which is exactly what Preflight is for, and is more useful than
// refusing to start at all.
func New(opts Options) (*Executor, error) {
	shell := opts.Shell
	if shell == "" {
		shell = DefaultShell
	}
	envDir := opts.EnvFileDir
	if envDir == "" {
		envDir = os.TempDir()
	}
	grace := opts.TerminateGrace
	if grace <= 0 {
		grace = TerminateGrace
	}
	reapGrace := opts.ReapGrace
	if reapGrace <= 0 {
		reapGrace = ReapTerminateGrace
	}
	drain := opts.PipeDrainDelay
	if drain <= 0 {
		drain = DefaultPipeDrainDelay
	}

	e := &Executor{
		opts:      opts,
		envDir:    envDir,
		grace:     grace,
		reapGrace: reapGrace,
		drain:     drain,
		goos:      runtime.GOOS,
		uid:       currentUID(),
	}
	if resolved, err := exec.LookPath(shell); err != nil {
		e.shell, e.shellErr = shell, err
	} else {
		e.shell = resolved
	}
	return e, nil
}

// Name identifies this executor in config and in `runs_on` resolution.
func (e *Executor) Name() string { return "native" }

// Preflight verifies the host can actually execute the work this runner accepts
// (§20).
//
// The point is the timing, not the checks. A signing runner installed as a
// LaunchDaemon fails every iOS job identically, forty minutes in, with an error
// that names neither launchd nor the keychain — §13.3 calls it the single most
// common install failure for a reason. Failing here instead marks the runner
// unavailable, names the fix, and recovers automatically when the next
// Preflight passes.
//
// Disk floors (§14.4's 60 GB for native) are deliberately not checked here, for
// the same reason the docker executor does not check its 40: they are a config
// and pruning concern, and admission already refuses a run below the floor with
// E_DISK.
func (e *Executor) Preflight(ctx context.Context) error {
	if e.shellErr != nil {
		return wrapErr(protocol.ErrExecutor, e.shellErr,
			"native executor: the %q login shell is not on PATH, so no step could run", e.opts.Shell)
	}
	// launchctl and security exist only on macOS. §13.3 is a macOS section and
	// this is a macOS executor, but running steps on a Linux host through it is
	// a legitimate thing an operator may do, and failing preflight over a
	// missing `launchctl` would make that impossible for no benefit.
	if e.goos != "darwin" {
		return nil
	}
	if _, err := CheckSession(ctx, e.opts.Runner, e.uid, e.opts.RequireGUISession); err != nil {
		return err
	}
	if e.opts.CheckKeychain {
		if err := CheckKeychain(ctx, e.opts.Runner, e.opts.Keychain); err != nil {
			return err
		}
	}
	return nil
}

// Session reports the launchd context this executor is running in, without
// deciding whether it is acceptable. `latem doctor` renders
// [Session.Explain]; Preflight is what acts on it.
func (e *Executor) Session(ctx context.Context) Session {
	return DetectSession(ctx, e.opts.Runner, e.uid)
}

// Prepare readies the host for one run and returns its [executor.Env].
//
// There is almost nothing to provision, which is the whole point of this
// executor: the workspace is already a self-contained clone (§12) and the host
// is already the environment. What is left is the three things that would
// otherwise fail later and less legibly — a run id that is about to become a
// path, caches whose target must resolve to the runner's cache directory, and
// §9's $LATEM_ENV, which has to exist before the first step so that
// `>> "$LATEM_ENV"` works on a job's very first use of it.
//
// [executor.Spec.Image] is ignored. §9's field reference says so — "Docker
// executors only … Ignored by tart/native" — and it is worth being explicit
// that this is a documented no-op rather than an oversight, because a job
// declaring an image and running on a native runner gets whatever is installed
// on the host and no warning that its declaration meant nothing.
//
// [executor.Spec.Resources] is ignored too, and that one is a real limitation
// rather than a shrug: see the package doc.
func (e *Executor) Prepare(_ context.Context, spec executor.Spec) (executor.Env, error) {
	if e.shellErr != nil {
		return nil, wrapErr(protocol.ErrExecutor, e.shellErr,
			"native executor: the %q login shell is not on PATH", e.opts.Shell)
	}
	// A6: the run id becomes a filesystem path here. Validate before building
	// one, not after.
	if err := protocol.ValidateID(spec.RunID); err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "run id")
	}
	if !filepath.IsAbs(spec.Workspace) {
		return nil, codeErr(protocol.ErrExecutor, "workspace %q is not an absolute path", spec.Workspace)
	}
	info, err := os.Stat(spec.Workspace)
	if err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "workspace %q", spec.Workspace)
	}
	if !info.IsDir() {
		return nil, codeErr(protocol.ErrExecutor, "workspace %q is not a directory", spec.Workspace)
	}

	// Expand ~ ONCE, here, and use the result for both validation and linking.
	//
	// The first version of amendment A38 expanded inside validateCacheMounts,
	// which takes its mounts by value — so validation saw an absolute path and
	// passed, while linkCaches still received "~/Library/...". filepath.Clean
	// leaves that relative, so MkdirAll created a literal "~" directory under
	// the AGENT'S WORKING DIRECTORY and symlinked into it. Every run reported
	// "mounted 1 cache(s)" and no cache was ever mounted; the real
	// ~/Library/Developer/Xcode/DerivedData was never touched, which is also
	// why nothing looked wrong.
	caches, err := expandCacheTargets(spec.Caches)
	if err != nil {
		return nil, err
	}
	if err := validateCacheMounts(caches, spec.Workspace); err != nil {
		return nil, err
	}
	if err := linkCaches(caches); err != nil {
		return nil, err
	}

	dir := filepath.Join(e.envDir, EnvDirPrefix+spec.RunID)
	env := &runEnv{
		spec:        spec,
		workspace:   spec.Workspace,
		shell:       e.shell,
		envDir:      dir,
		envFile:     filepath.Join(dir, EnvFileName),
		outFile:     filepath.Join(dir, OutputFileName),
		pathFile:    filepath.Join(dir, PathFileName),
		summaryFile: filepath.Join(dir, SummaryFileName),
		tempDir:     filepath.Join(dir, TempDirName),
		grace:       e.grace,
		drain:       e.drain,
		carried:     map[string]string{},
		active:      map[*stepProc]struct{}{},
	}
	for f, label := range map[string]string{
		env.outFile: "$GITHUB_OUTPUT", env.pathFile: "$GITHUB_PATH", env.summaryFile: "$GITHUB_STEP_SUMMARY",
	} {
		if err := env.createEmpty(f, label); err != nil {
			return nil, err
		}
	}
	if err := env.createEnvFile(); err != nil {
		return nil, err
	}
	// 0o700 like the files beside it: the directory is the run's, and a step
	// is the only thing that should read what it put there.
	if err := os.Mkdir(env.tempDir, 0o700); err != nil {
		return nil, wrapErr(protocol.ErrExecutor, err, "create $RUNNER_TEMP %s", env.tempDir)
	}
	return env, nil
}

// validateCacheMounts rejects cache targets that would break the run rather
// than help it.
//
// A cache mounted at or inside the workspace shadows the git clone §12 went to
// some trouble to produce, and the resulting "my repo is empty" failure points
// nowhere near the cache declaration that caused it. A relative target is a
// registry authoring mistake. A relative *source* is a docker volume name
// (§14.1) reaching an executor that has no volumes, which would otherwise
// become a directory named "latem-cache-…" in whatever the process's working
// directory happened to be.
func validateCacheMounts(caches []executor.CacheMount, workspace string) error {
	// Both spellings of the workspace, because either one may be what the
	// registry's caches[].path was written against. On macOS the agent's work
	// directory is commonly reached through /var, which is a symlink to
	// /private/var, and comparing only the resolved form would let a target
	// spelled the other way through the check it exists to fail.
	roots := []string{filepath.Clean(workspace)}
	if real, err := filepath.EvalSymlinks(workspace); err == nil && real != roots[0] {
		roots = append(roots, real)
	}

	seen := make(map[string]string, len(caches))
	for _, c := range caches {
		if c.Source == "" {
			return codeErr(protocol.ErrCache, "cache %q has no source", c.Name)
		}
		if !filepath.IsAbs(c.Source) {
			return codeErr(protocol.ErrCache,
				"cache %q source %q is not an absolute path; the native executor has no volumes, "+
					"so a cache source must be the host directory §14.2 names", c.Name, c.Source)
		}
		// Targets arrive already expanded, from expandCacheTargets in Prepare.
		if !filepath.IsAbs(c.Target) {
			return codeErr(protocol.ErrCache, "cache %q target %q is not an absolute path", c.Name, c.Target)
		}
		target := filepath.Clean(c.Target)
		if target == string(filepath.Separator) {
			return codeErr(protocol.ErrCache, "cache %q would take over /", c.Name)
		}
		for _, root := range roots {
			if within(root, target) {
				return codeErr(protocol.ErrCache,
					"cache %q target %q is inside the workspace and would shadow the run's clone", c.Name, c.Target)
			}
		}
		if other, dup := seen[target]; dup {
			return codeErr(protocol.ErrCache, "caches %q and %q both resolve to %q", other, c.Name, c.Target)
		}
		seen[target] = c.Name
	}
	return nil
}

// linkCaches makes each cache target resolve to the runner's cache directory.
func linkCaches(caches []executor.CacheMount) error {
	for _, c := range caches {
		// 0o700 matches what cache.Local creates: a cache holds a repository's
		// dependency tree and, for CocoaPods and Gradle, occasionally
		// credentials a build wrote there.
		if err := os.MkdirAll(c.Source, 0o700); err != nil {
			return wrapErr(protocol.ErrCache, err, "cache %q directory %s", c.Name, c.Source)
		}
		if err := linkCache(c); err != nil {
			return err
		}
	}
	return nil
}

// linkCache bridges one cache's target path to its source directory.
//
// # Why this needs a paragraph
//
// §14.2 says tart and native caches are "host directories, shared into the VM
// with --dir, or used in place for native". "Used in place" is doing a lot of
// work in that sentence: tart has a share and docker has a mount, and this
// executor has neither, so something has to make the path in the registry's
// caches[].path — ~/.gradle, ~/Library/Caches/CocoaPods — actually be the
// directory under ~/.latem/caches that WS-08 locked and prunes. A symlink is
// the only mechanism the host offers.
//
// The rules below are conservative in exactly one direction: this function will
// create a link where nothing exists, and will never replace, move, or delete
// anything that does.
//
//   - Target already is the source. Nothing to do; the operator pointed the
//     registry straight at the cache directory.
//   - Target does not exist. Create the parent and the symlink. It survives the
//     run deliberately — caches are mounted, not archived (§14), and the next
//     run finds the link already there.
//   - Target is a symlink to the source. Already linked, by an earlier run.
//   - Target is a real directory. Left completely alone, and not an error. On
//     this executor the machine is not ephemeral, so a real ~/.gradle already
//     persists between runs and the cache declaration is satisfied by the host
//     itself. Replacing it with a symlink would strand however many gigabytes
//     of somebody's existing dependency tree, which is not a decision an
//     executor gets to make silently in the middle of Prepare.
//   - Target is a symlink somewhere else, or is a file. E_CACHE. This is a
//     genuine misconfiguration — most likely two runners sharing one target,
//     which §14.3 exists to prevent — and the alternative to failing is two
//     runs corrupting one cache with symptoms nobody attributes to caching.
func linkCache(c executor.CacheMount) error {
	source := filepath.Clean(c.Source)
	target := filepath.Clean(c.Target)
	if source == target {
		return nil
	}

	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// 0o750 on the parent: a cache target's parent is usually $HOME or a
		// directory beneath it, and this executor has no business widening it.
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return wrapErr(protocol.ErrCache, err, "cache %q target parent %s", c.Name, filepath.Dir(target))
		}
		if err := os.Symlink(source, target); err != nil {
			return wrapErr(protocol.ErrCache, err, "cache %q link %s -> %s", c.Name, target, source)
		}
		return nil

	case err != nil:
		return wrapErr(protocol.ErrCache, err, "cache %q target %s", c.Name, target)

	case info.Mode()&fs.ModeSymlink != 0:
		if sameDir(target, source) {
			return nil
		}
		dest, _ := os.Readlink(target)
		return codeErr(protocol.ErrCache,
			"cache %q target %s is a symlink to %s, not to this runner's cache directory %s — "+
				"two runners sharing one cache directory corrupt each other (§14.3)",
			c.Name, target, dest, source)

	case info.IsDir():
		// Used in place, in the most literal sense available: the host's own
		// directory persists across runs because the host is not ephemeral.
		return nil

	default:
		return codeErr(protocol.ErrCache,
			"cache %q target %s exists and is not a directory", c.Name, target)
	}
}

// sameDir reports whether two paths resolve to the same directory.
func sameDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}

// Reap collects what runs that ended without a Close left on this host — an
// agent killed mid-run (§11's E_INTERRUPTED).
//
// This is the native counterpart of the docker executor's label-based
// reconciliation. Two things per interrupted run, in this order:
//
//  1. The step's process group, if one is still running. This is the part that
//     matters, and the part §6 step 5 did not originally have words for: with no
//     container and no VM, killing the agent leaves the step's `bash -lc` and
//     everything under it reparented to launchd and still running. An abandoned
//     `xcodebuild` holds DerivedData and a keychain handle and competes with the
//     next build for the machine, which is exactly §13.3's class of "failures
//     that look like code bugs". [stepGroup] is the record that makes this
//     possible, and — more importantly — makes it safe to do by number.
//  2. The $LATEM_ENV directory, which is otherwise litter in $TMPDIR that
//     nothing else ever collects.
//
// Only directories directly under this executor's own [Options.EnvFileDir],
// named with [EnvDirPrefix], are considered. Nothing here follows a path from a
// broker, a job or another agent, which is the outer half of the guard;
// [signalPgid] is the inner half.
//
// A group that could not be verified or could not be signalled is reported as an
// error, and its directory is removed anyway. That is deliberate: keeping the
// record for a later attempt means keeping a number that is one pid rollover
// away from naming something else entirely.
//
// live reports whether a run is still in flight; a nil live reaps everything,
// which is only safe at startup before any run has been claimed.
func (e *Executor) Reap(_ context.Context, live func(runID string) bool) ([]executor.Reaped, error) {
	entries, err := os.ReadDir(e.envDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, wrapErr(protocol.ErrExecutor, err, "read %s", e.envDir)
	}

	var (
		reaped   []executor.Reaped
		firstErr error
	)
	for _, ent := range entries {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), EnvDirPrefix) {
			continue
		}
		runID := strings.TrimPrefix(ent.Name(), EnvDirPrefix)
		if live != nil && live(runID) {
			continue
		}
		dir := filepath.Join(e.envDir, ent.Name())

		// Groups first: the directory removal below takes the records with it,
		// and a record removed before its group was stopped is a process nobody
		// can name any more.
		groups, groupErr := reapGroups(dir, e.reapGrace)
		for _, pgid := range groups {
			reaped = append(reaped, executor.Reaped{
				RunID: runID, Kind: ReapedProcessGroup, Ref: strconv.Itoa(pgid),
			})
		}
		if groupErr != nil && firstErr == nil {
			firstErr = wrapErr(protocol.ErrExecutor, groupErr, "run %s: abandoned process group", runID)
		}

		if err := os.RemoveAll(dir); err != nil {
			if firstErr == nil {
				firstErr = wrapErr(protocol.ErrExecutor, err, "remove %s", ent.Name())
			}
			continue
		}
		reaped = append(reaped, executor.Reaped{RunID: runID, Kind: "workspace", Ref: ent.Name()})
	}
	return reaped, firstErr
}

// expandHome resolves a leading "~/" to the current user's home directory.
//
// Only a leading "~/" (or a bare "~") is special. "~user/..." is deliberately
// NOT supported: on the native executor a step already runs as one specific
// user, so naming a different one would produce a path that user cannot write,
// and the failure would arrive as a permission error on a cache mount rather
// than as anything mentioning ~.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand ~: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf("cannot expand ~: $HOME is empty")
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// expandCacheTargets resolves a leading ~ in every cache target, returning a
// new slice so no caller can accidentally work from the unexpanded originals.
//
// A cache target cannot be written absolutely in a committed jobs.yaml:
// "/Users/someone/Library/..." is correct on exactly one machine, and §12 hands
// the same commit to every runner. ~ is the only portable spelling, which is
// why §9's own example uses it (amendment A38).
//
// Expansion is per-executor rather than central because ~ denotes a different
// directory in each — the agent user's home natively, the container's runner
// home under docker (§13.1) — so resolving it once in internal/cache would have
// to pick one and be wrong in the other.
func expandCacheTargets(in []executor.CacheMount) ([]executor.CacheMount, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]executor.CacheMount, len(in))
	copy(out, in)
	for i := range out {
		expanded, err := expandHome(out[i].Target)
		if err != nil {
			return nil, codeErr(protocol.ErrCache, "cache %q target %q: %v", out[i].Name, out[i].Target, err)
		}
		out[i].Target = expanded
	}
	return out, nil
}
