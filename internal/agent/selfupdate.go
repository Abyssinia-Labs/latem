package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// Self-update, RUNNER_SPEC §20: check a release feed daily, verify a minisign
// signature, download to a temp path, atomic rename, re-exec only when no run
// is active.
//
// internal/selfupdate owns everything up to the rename and answers for the
// trust chain. What is left here is the part that needs to know what this
// machine is doing, and it is the whole reason this is not a three-line call:
//
//  1. **Never mid-run.** Not "probably not mid-run". Checking that nothing is
//     active and then re-execing is a race with a worker about to claim, and
//     losing it orphans a build — the new image's reconciliation would mark
//     that run interrupted and reap it, which is §6 working correctly on a
//     problem this code caused.
//
//     So the pool stops claiming *first* ([Agent.pauseClaims]), and only then
//     is idleness worth measuring.
//
//  2. **A pause that ends.** `draining` is a one-way latch, because its only
//     caller is shutdown and a machine that is shutting down never accepts
//     again. Reusing it here would leave a machine permanently refusing work
//     after a download failed, which is a worse outcome than never updating.
//     Hence a separate, reversible flag.
//
//  3. **A short wait.** The machine's job is running builds, and refusing them
//     for an hour to install an update that could just as well land tomorrow
//     is the wrong trade. It waits [DefaultUpdateIdleWait] for the in-flight
//     work to finish and then gives up until the next tick.

const (
	// DefaultUpdateInterval is §20's daily feed check.
	DefaultUpdateInterval = 24 * time.Hour

	// DefaultUpdateIdleWait bounds how long the agent will refuse new runs
	// while waiting for the in-flight ones to finish so it can replace itself.
	//
	// Ten minutes, not shutdown_grace's thirty: this is capacity given up for
	// an update rather than for a shutdown, and an update that does not happen
	// today happens tomorrow. A machine that is never idle for ten minutes is
	// a machine whose operator would rather have the builds.
	DefaultUpdateIdleWait = 10 * time.Minute
)

// releaseSource is the seam over internal/selfupdate. A test supplies one that
// touches no network and hands back a binary it wrote itself.
type releaseSource interface {
	Check(ctx context.Context) (selfupdate.Release, error)
	Fetch(ctx context.Context, rel selfupdate.Release, dir string) (string, error)
}

// updateLoop is the daily check. It exists only when auto_update is on.
func (a *Agent) updateLoop(ctx context.Context) {
	if !a.cfg.Agent.AutoUpdate {
		return
	}
	src, err := a.releases()
	if err != nil {
		// The config loader refuses auto_update on a build with no trusted
		// key, so reaching this is either a test or a build whose embedded key
		// is malformed. Say so once rather than daily.
		a.log.Error("self-update is enabled and cannot run", "err", err)
		return
	}

	t := time.NewTicker(a.updateEvery)
	defer t.Stop()
	// Not at startup. A machine that has just been restarted — by a service
	// manager, by a reboot, or by the previous update — is the worst moment to
	// replace its own binary, and the operator watching that restart did not
	// ask for a second one.
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Resolved every cycle rather than once. An agent can outlive a
			// reinstall that moved its binary, and a path cached at startup
			// would rename a new release over a file nothing executes.
			target, err := runningBinary()
			if err != nil {
				a.log.Error("self-update: cannot locate the running binary", "err", err)
				continue
			}
			a.updateOnce(ctx, src, target)
		}
	}
}

// updateOnce performs one check-and-maybe-replace cycle against the binary at
// target.
func (a *Agent) updateOnce(ctx context.Context, src releaseSource, target string) {
	if a.switching.Load() {
		// A SIGUSR2 is already moving this agent onto the binary on disk,
		// and a second pause-and-swap would race it (A56).
		a.log.Debug("self-update: a move onto the binary on disk is pending; skipping this check")
		return
	}
	rel, err := src.Check(ctx)
	switch {
	case errors.Is(err, selfupdate.ErrUpToDate):
		a.log.Debug("self-update: already current", "version", a.version)
		return
	case err != nil:
		// A feed that is unreachable, rate-limited, or answering nonsense is
		// not a failure of this machine, and a daily Error would train an
		// operator to ignore the log. It is still worth saying.
		a.log.Warn("self-update: could not check for a release", "err", err)
		return
	}

	a.log.Info("self-update: a newer release is available",
		"release", rel.Version, "running", a.version, "target", target)

	// Staged beside the binary it replaces, because the rename that makes the
	// swap atomic cannot cross a filesystem — and ~/.latem is frequently not
	// on the same one as ~/.local/bin.
	staged, err := src.Fetch(ctx, rel, filepath.Dir(target))
	if err != nil {
		a.log.Error("self-update: refusing this release", "release", rel.Version, "err", err)
		return
	}

	// Everything above is reversible. From here it is not, so the ordering is
	// the point: stop claiming, confirm idle, swap, re-exec.
	a.pauseClaims()
	defer a.resumeClaims()

	if !a.awaitIdle(ctx, a.updateIdleWait) {
		_ = os.Remove(staged)
		a.log.Info("self-update: still busy, leaving this release for the next check",
			"release", rel.Version, "waited", a.updateIdleWait)
		return
	}

	if err := selfupdate.Install(staged, target); err != nil {
		_ = os.Remove(staged)
		a.log.Error("self-update: could not replace the binary", "err", err)
		return
	}
	a.log.Info("self-update: installed", "release", rel.Version, "path", target)

	// The last thing this process does as itself.
	if err := a.reexec(target); err != nil {
		// The new binary is on disk and will be what the service manager
		// starts next; only this process is stale. Loud, because an operator
		// reading `latem version` will now be told something that disagrees
		// with the file on disk until something restarts it.
		a.log.Error("self-update: installed but could not re-exec; "+
			"this process keeps running the old binary until it restarts",
			"version", a.version, "installed", rel.Version, "err", err)
	}
}

// releases builds the real release source.
func (a *Agent) releases() (releaseSource, error) {
	if a.opts.Releases != nil {
		return a.opts.Releases, nil
	}
	key, err := selfupdate.TrustedKey()
	if err != nil {
		return nil, err
	}
	return &selfupdate.Updater{
		Current: a.version,
		Key:     key,
		Log:     a.log,
	}, nil
}

// pauseClaims stops the pool taking new work, reversibly.
func (a *Agent) pauseClaims() { a.paused.Store(true) }

// resumeClaims lets it take work again, and rings the doorbell: a run that
// arrived during the pause was not claimed, and without the nudge it waits
// for the next poll.
func (a *Agent) resumeClaims() {
	if a.paused.Swap(false) {
		a.ring()
	}
}

// awaitIdle waits until no run is in flight, or until the bound elapses.
//
// Polled rather than signalled. A condition variable here would need every
// run's completion path to know about self-update, and the thing being waited
// for takes minutes: a quarter-second poll is a handful of integer comparisons.
//
// The bound is wall-clock — a context timeout rather than the agent's injected
// clock — because it is the length of a wait rather than a fact about a run. A
// test with a frozen clock would otherwise wait here forever.
func (a *Agent) awaitIdle(ctx context.Context, bound time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	for {
		if a.activeRuns() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// activeRuns is how many runs this agent is executing.
func (a *Agent) activeRuns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.activeRepos)
}

// runningBinary is the file this process is executing, with symlinks
// resolved.
//
// Resolved on purpose: `~/.local/bin/latem` is sometimes a symlink into a
// versioned directory, and replacing the symlink rather than its target would
// install the new binary somewhere nothing looks.
func runningBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A binary that has been deleted from under a running process — which
		// is what `go test` does with its own temporary binary — has no path
		// to replace.
		return "", err
	}
	return resolved, nil
}

// reexec replaces this process image with the new binary.
//
// exec rather than exit-and-let-the-service-manager-restart, for two reasons
// that are both about not depending on how the agent was started: it keeps the
// pid, so launchd and systemd see a running service throughout rather than a
// flap, and it works for an agent somebody started in a terminal, which has no
// KeepAlive to fall back on.
//
// The flock on the config directory is released by the exec — Go opens it
// with O_CLOEXEC — and reacquired by the new image during §6 step 2. The gap
// is the width of one execve.
func (a *Agent) reexec(path string) error {
	if a.opts.Exec != nil {
		return a.opts.Exec(path, os.Args, os.Environ())
	}
	a.log.Info("re-executing", "path", path, "args", os.Args)
	// Only returns on failure.
	return syscall.Exec(path, os.Args, os.Environ()) //nolint:gosec // the path this agent just verified and installed
}
