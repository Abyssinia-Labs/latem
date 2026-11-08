package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// §20's self-update, from the operator's side.
//
// The check exists because `auto_update = true` is the one setting whose
// failure mode is silence. Everything it needs is off this machine — a release
// feed, assets, a signature — and when any of it is unreachable the agent does
// the correct thing: it warns, once a day, into a log nobody is reading. The
// config loaded, the value was accepted, and nothing happens for months.
//
// That is the shape A27 and A38 are about, one step removed: the control is
// real and the *environment* makes it inert. So doctor asks the question the
// agent will ask, at the moment somebody is watching the answer.
//
// The case that prompted it is this repository's own: a private repo answers
// 404 to `api.github.com/repos/<owner>/<repo>/releases/latest`, because the
// agent sends no token and deliberately holds no credential for the feed. An
// operator who turned auto_update on would have a machine that never updates
// and never says why.
func (d *doctor) checkSelfUpdate(ctx context.Context) []check {
	const id = "update.feed"

	if d.cfg == nil {
		return []check{skip(id, "self-update", "config did not load")}
	}
	if !d.cfg.Agent.AutoUpdate {
		// Nothing to report. Off is the default and a supported choice, and a
		// line saying "you have not enabled the optional thing" on every run
		// is how a report stops being read.
		return nil
	}

	brewed := d.homebrew
	if brewed == nil {
		brewed = homebrewLink
	}
	if link, ok := brewed(); ok {
		// Not a failure: the machine does keep itself current, through
		// Homebrew, and the agent follows what that installs. The setting is
		// what is wrong, because it promises something this install will not
		// do (A78).
		return []check{warn(id, "self-update",
			"agent.auto_update is on, and this latem is Homebrew's, so it does not replace itself",
			brewUpgrade+"   # the agent moves onto what it installs when no run is in flight").
			withNotes(fmt.Sprintf("Homebrew keeps this binary at %s and deletes its keg on the next "+
				"upgrade, so a release written there is one Homebrew would report as something else and "+
				"then remove. agent.auto_update does nothing on this install; remove it.", link))}
	}

	key, err := selfupdate.TrustedKey()
	if err != nil {
		// The config loader refuses this pairing, so reaching it means the
		// config was not loaded through it.
		return []check{fail(id, "self-update",
			"agent.auto_update is on and this build trusts no release signing key",
			"install a signed build: see docs/release.md")}
	}

	// The same question the agent's daily ticker asks, through the same code.
	// A check that reimplemented the probe could pass while the agent failed.
	ask := d.latestRelease
	if ask == nil {
		ask = (&selfupdate.Updater{Current: version, Key: key}).Check
	}
	// Bounded: doctor is something an operator runs and waits for, and a feed
	// that accepts a connection and never answers should cost twenty seconds
	// rather than the whole report.
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rel, feedErr := ask(probe)

	switch {
	case errors.Is(feedErr, selfupdate.ErrUpToDate):
		return []check{pass(id, "self-update",
			fmt.Sprintf("on; %s is current, signed by key %s", version, key.ID()))}

	case feedErr != nil:
		return []check{fail(id, "self-update",
			fmt.Sprintf("agent.auto_update is on and the release feed is unreachable: %v", feedErr),
			"latem version   # the key this build trusts").
			withNotes("The agent sends no credential to the feed, so a private repository " +
				"answers 404 to everyone. Until it is public, this machine will check once a " +
				"day, log a warning, and never update — which is indistinguishable, from the " +
				"outside, from a machine that is already current (§20).")}

	default:
		return []check{warn(id, "self-update",
			fmt.Sprintf("on; %s is available and this machine is on %s", rel.Version, version),
			"# nothing to do: the agent installs it when no run is in flight")}
	}
}
