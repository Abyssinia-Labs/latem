package agent

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// An agent that Homebrew installed (A78).
//
// `brew upgrade latem` pours the new version into a keg of its own, moves the
// opt link onto it, and deletes the keg this agent is running from. Nothing
// tells the agent. Left alone it runs the old version until something
// restarts it, and on Linux it can no longer even find its own file: the
// kernel names a deleted executable by the path it had.
//
// So an agent running from a keg watches the link, and when it leads somewhere
// new, moves there the way SIGUSR2 does (A56): ask the binary its version,
// wait for an idle moment, re-exec in place. One readlink a minute, and
// nothing at all on an install that is not Homebrew's. It never holds work
// back for this. An upgrade the operator ran is not one they asked to take
// effect at the cost of a build, so it moves at the first moment no run is in
// flight, however long that takes.

// DefaultHomebrewInterval is how often an agent in a keg looks at the link.
const DefaultHomebrewInterval = time.Minute

// homebrew is the link Homebrew keeps at the installed version, when this
// binary is in a keg.
func (a *Agent) homebrew() (string, bool) {
	if a.opts.Homebrew != nil {
		return a.opts.Homebrew()
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return selfupdate.Homebrew(exe)
}

// selfPath is the binary to move onto: for Homebrew's, whatever its link
// leads to now; for any other, the file this process runs, resolved.
func (a *Agent) selfPath() (string, error) {
	if link, ok := a.homebrew(); ok {
		return filepath.EvalSymlinks(link)
	}
	return runningBinary()
}

// homebrewLoop moves the agent onto what `brew upgrade` installed. It exists
// only for a binary in a keg.
func (a *Agent) homebrewLoop(ctx context.Context) {
	link, ok := a.homebrew()
	if !ok {
		return
	}
	a.log.Info("this binary is Homebrew's; the agent moves onto what `brew upgrade latem` installs, "+
		"the first moment no run is in flight", "link", link)

	// Where the link led when it was last acted on, starting from the keg
	// this process runs. Acting only when it moves is what keeps a version
	// that cannot be run from being asked again every minute.
	seen, _ := runningBinary()
	t := time.NewTicker(a.homebrewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		target, err := filepath.EvalSymlinks(link)
		if err != nil || target == seen {
			// Unchanged, or mid-upgrade: brew unlinks the old keg before
			// it links the new one.
			continue
		}
		if a.followHomebrew(ctx, target) {
			seen = target
		}
	}
}

// followHomebrew moves onto the binary at target, which the link has just
// started leading to. It reports false when another move is already waiting,
// so the next look tries again: that move is to whatever the link led to
// when it was asked for, which may be a keg this upgrade has deleted.
func (a *Agent) followHomebrew(ctx context.Context, target string) bool {
	if a.switching.Load() {
		return false
	}
	to, err := a.binaryVersion(ctx, target)
	if err != nil {
		a.log.Error("Homebrew installed a latem that will not say its version; still running this one",
			"version", a.version, "path", target, "err", err)
		return true
	}
	if to == a.version {
		// `brew reinstall`: the same version in a new keg. The service
		// starts it next time, and there is nothing to move for.
		a.log.Info("the link leads to the version this agent runs; nothing to move for", "version", to, "path", target)
		return true
	}
	if a.switching.Swap(true) {
		return false
	}
	a.log.Info("Homebrew installed another latem; moving onto it the first moment no run is in flight",
		"running", a.version, "to", to, "path", target)
	go a.switchWhenIdle(ctx, target, to, false, 0)
	return true
}
