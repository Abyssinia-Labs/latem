package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// The check exists for one situation — auto_update on, feed unreachable — so
// that is the case with the most assertions. The others are here to keep it
// from becoming noise.

func updateDoctor(t *testing.T, autoUpdate bool) *doctor {
	t.Helper()
	dir := privateDir(t)
	cfg := testConfig(dir)
	cfg.Agent.AutoUpdate = autoUpdate
	return newTestDoctor("linux", dir, healthyRunner("linux"), cfg, nil)
}

// A machine that did not opt in gets no line at all. §20 is explicit that
// self-update is opt-in, and a report that says "you have not enabled the
// optional thing" on every run is a report that stops being read.
func TestDoctorSelfUpdate_SaysNothingWhenItIsOff(t *testing.T) {
	d := updateDoctor(t, false)
	d.latestRelease = func(context.Context) (selfupdate.Release, error) {
		t.Fatal("doctor probed the release feed on a machine with auto_update off")
		return selfupdate.Release{}, nil
	}
	if got := d.checkSelfUpdate(context.Background()); len(got) != 0 {
		t.Errorf("auto_update is off and doctor reported %d check(s): %+v", len(got), got)
	}
}

// The case this check was written for, and the case this repository is in: a
// private repository answers 404 to a feed request carrying no credential, so
// the agent checks daily and never updates. That has to be a failure with a
// reason, not a pass and not a warning — the operator asked for a machine that
// keeps itself current and does not have one.
func TestDoctorSelfUpdate_AnUnreachableFeedIsAFailure(t *testing.T) {
	d := updateDoctor(t, true)
	d.latestRelease = func(context.Context) (selfupdate.Release, error) {
		return selfupdate.Release{}, errors.New("the release feed answered 404 Not Found")
	}

	got := d.checkSelfUpdate(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if got[0].Level != levelFail {
		t.Errorf("level = %v, want fail: the operator asked for a machine that updates itself "+
			"and this one cannot", got[0].Level)
	}
	// The feed's own words, because "unreachable" does not distinguish a
	// private repository from a rate limit from a DNS failure, and the fix
	// differs for each.
	if !strings.Contains(got[0].Detail, "404") {
		t.Errorf("the detail does not carry what the feed said: %q", got[0].Detail)
	}
	if !strings.Contains(strings.Join(got[0].Notes, " "), "private") {
		t.Errorf("the notes do not name the likeliest cause: %q", got[0].Notes)
	}
}

func TestDoctorSelfUpdate_CurrentAndBehind(t *testing.T) {
	t.Run("already current", func(t *testing.T) {
		d := updateDoctor(t, true)
		d.latestRelease = func(context.Context) (selfupdate.Release, error) {
			return selfupdate.Release{}, selfupdate.ErrUpToDate
		}
		got := d.checkSelfUpdate(context.Background())
		if len(got) != 1 || got[0].Level != levelPass {
			t.Fatalf("an up-to-date machine gave %+v, want one pass", got)
		}
	})

	t.Run("a release is waiting", func(t *testing.T) {
		d := updateDoctor(t, true)
		d.latestRelease = func(context.Context) (selfupdate.Release, error) {
			return selfupdate.Release{Version: "v9.9.9", Bare: "9.9.9"}, nil
		}
		got := d.checkSelfUpdate(context.Background())
		if len(got) != 1 {
			t.Fatalf("got %d checks, want 1", len(got))
		}
		// A warning rather than a failure: the machine is working as
		// configured, and the agent installs it when no run is in flight. A
		// failure here would be doctor reporting a red machine for doing
		// exactly what it was asked to do.
		if got[0].Level != levelWarn {
			t.Errorf("level = %v, want warn", got[0].Level)
		}
		if !strings.Contains(got[0].Detail, "v9.9.9") {
			t.Errorf("the detail does not name the available release: %q", got[0].Detail)
		}
	})
}

// A config that did not load is a different answer from a machine that opted
// out, and conflating them would report "self-update is off" about a machine
// whose config nobody could read.
func TestDoctorSelfUpdate_NoConfig(t *testing.T) {
	dir := privateDir(t)
	d := newTestDoctor("linux", dir, healthyRunner("linux"), nil, errors.New("broken"))
	got := d.checkSelfUpdate(context.Background())
	if len(got) != 1 || got[0].Level != levelSkip {
		t.Fatalf("a failed config load gave %+v, want one skip", got)
	}
}
