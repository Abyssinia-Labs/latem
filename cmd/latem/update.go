package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// `latem update` (A56): install the latest release, verified exactly as §20's
// self-update verifies one, and move the running agent onto it.

func newUpdateCommand() *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Install the latest release, and move the running agent onto it",
		Long: `Install the latest release of latem, and move the running agent onto it.

The release is checked as the agent's own self-update checks one (§20): the
release feed supplies a version and nothing else, every byte written is covered
by a signature from the key compiled into this binary, the version must be
newer than this one, and the downloaded binary is asked what it is before it
replaces anything. It replaces the file this command is running from, with
symlinks resolved, in one rename.

The running agent is then asked to move onto it. It stops taking runs, waits
up to agent.shutdown_grace for the ones in flight, and re-executes in place —
same pid, so the service manager sees no restart. If runs are still going by
then it takes work again and moves at the first idle moment. It never cancels a
run to do it.

Every other command says, at most once a day and only on a terminal, when a
newer release exists. LATEM_NO_UPDATE_NOTIFIER=1 or CI=1 turns that off.

It also puts ltm, latem's short name, beside the binary as a link to it, when
nothing called ltm is there yet (A79).

A latem that Homebrew installed is Homebrew's to update: this command refuses
to write into its keg and says to run ` + "`brew upgrade latem`" + `. The agent
moves onto what that installs by itself, the first moment no run is in flight
(A78). --check still says whether a newer release exists.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd.Context(), cmd.OutOrStdout(), checkOnly)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "say whether a newer release exists, and change nothing")
	return cmd
}

// releaser is the part of [selfupdate.Updater] this command uses.
type releaser interface {
	Check(ctx context.Context) (selfupdate.Release, error)
	Fetch(ctx context.Context, rel selfupdate.Release, dir string) (string, error)
}

// Seams. A test must never download a release, replace its own binary, or
// signal the machine's real agent.
var (
	newReleaser = func() (releaser, error) {
		key, err := selfupdate.TrustedKey()
		if err != nil {
			return nil, err
		}
		return &selfupdate.Updater{Current: version, Key: key}, nil
	}
	selfBinary = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}
	signalSwitch = agent.SignalSwitch
	// homebrewLink is the link Homebrew keeps at the installed version, when
	// this binary is in a Homebrew keg (A78).
	homebrewLink = func() (string, bool) {
		exe, err := os.Executable()
		if err != nil {
			return "", false
		}
		return selfupdate.Homebrew(exe)
	}
)

// brewUpgrade is how a Homebrew install updates.
const brewUpgrade = "brew upgrade latem"

// upgradeCommand is what this binary's operator runs to install a newer
// release.
func upgradeCommand() string {
	if _, ok := homebrewLink(); ok {
		return brewUpgrade
	}
	return "latem update"
}

func runUpdate(ctx context.Context, out io.Writer, checkOnly bool) error {
	if version == "" || version == selfupdate.DevVersion {
		return fmt.Errorf("this is a development build (version %q), with no version to compare a release with; "+
			"install a release with install.sh, or build the one you want with `make install`", version)
	}
	link, brewed := homebrewLink()
	if brewed && !checkOnly {
		// Homebrew records which version a keg holds and deletes the keg on
		// the next upgrade, so a release written into it would be one brew
		// reports as something else and then removes.
		return fmt.Errorf("this latem is Homebrew's (%s), so Homebrew updates it: run `%s`; "+
			"the agent moves onto what that installs by itself, the first moment no run is in flight", link, brewUpgrade)
	}
	r, err := newReleaser()
	if err != nil {
		return fmt.Errorf("%w, so it cannot verify a release; install one with install.sh", err)
	}

	rel, err := r.Check(ctx)
	switch {
	case errors.Is(err, selfupdate.ErrUpToDate):
		printf(out, "latem %s is up to date\n", version)
		if !checkOnly {
			// The file may be newer than the agent: replaced by hand, or by
			// an update whose agent was busy and has since restarted on the
			// old one. Asking costs a read when there is nothing to do.
			if target, err := selfBinary(); err == nil {
				linkShortName(out, target)
				moveAgent(ctx, out, target, version)
			}
		}
		return nil
	case err != nil:
		if strings.Contains(err.Error(), "404") {
			err = fmt.Errorf("%w (the repository's releases are not public, or there are none yet)", err)
		}
		return fmt.Errorf("checking for a release: %w", err)
	}

	if checkOnly {
		printf(out, "latem %s is available; this is %s\n%s\n\nrun `%s` to install it\n",
			rel.Version, version, selfupdate.ReleasePage(rel.Version), upgradeCommand())
		return nil
	}

	target, err := selfBinary()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	printf(out, "downloading latem %s\n", rel.Version)
	staged, err := r.Fetch(ctx, rel, filepath.Dir(target))
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%w\n\n%s is not writable by this user; run `latem update` as its owner, "+
				"or reinstall with install.sh", err, filepath.Dir(target))
		}
		return fmt.Errorf("refusing latem %s: %w", rel.Version, err)
	}
	if err := selfupdate.Install(staged, target); err != nil {
		_ = os.Remove(staged)
		return err
	}
	printf(out, "installed latem %s at %s (was %s)\n", rel.Version, target, version)
	linkShortName(out, target)
	moveAgent(ctx, out, target, rel.Version)
	return nil
}

// linkShortName puts ltm beside the binary at target, as a link to it, when
// nothing of that name is there (A79): a machine installed before the short
// name existed gets it from its first update run by a binary that has this.
// The run that installs that binary is still the old one's code, so from
// v0.3.x it is the second. A file called ltm that is anything else is
// somebody else's, and stays.
func linkShortName(out io.Writer, target string) {
	link := filepath.Join(filepath.Dir(target), shortName)
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err := os.Symlink(filepath.Base(target), link); err != nil {
		printf(out, "could not add %s beside %s: %v\n", shortName, target, err)
		return
	}
	printf(out, "added %s, a link to %s\n", link, filepath.Base(target))
}

// moveAgent asks the running agent to move onto want, and says what came of
// it. It never fails the command: the binary is installed and is what the next
// agent runs, and whether the one running now has moved is a report.
func moveAgent(_ context.Context, out io.Writer, target, want string) {
	paths := config.DerivePaths(configDir())
	before, _ := agent.ReadReloadStatus(paths.ReloadStatus)
	st, err := signalSwitch(paths, want, agent.DefaultSwitchWait)
	restart := "  latem service restart\n"
	switch {
	case errors.Is(err, agent.ErrNoAgent):
		// Nothing to say when nothing was asked: an up-to-date check on a
		// machine with no agent is the ordinary case.
		if want != version {
			printf(out, "no agent is running; the next one to start runs %s\n", want)
		}
	case errors.Is(err, agent.ErrCannotSwitch):
		printf(out, "the running agent predates `latem update`; it runs its old version until it restarts:\n%s", restart)
	case err != nil:
		printf(out, "the running agent has not moved onto %s: %v\nrestart it to:\n%s", want, err, restart)
	case st.Version == want:
		switch {
		case before.Version != want:
			printf(out, "the agent (pid %d) is now running %s\n", st.PID, want)
		case want != version:
			printf(out, "the agent (pid %d) is running %s\n", st.PID, want)
		}
	case st.Switch == nil:
		printf(out, "the agent (pid %d) did not answer; restart it to run %s:\n%s", st.PID, want, restart)
	case st.Switch.State == agent.SwitchWaiting:
		printf(out, "the agent (pid %d) moves onto %s once its %d run(s) in flight finish; "+
			"it takes no new work meanwhile, for up to agent.shutdown_grace\n", st.PID, want, st.Switch.Runs)
	case st.Switch.State == agent.SwitchCurrent && st.Binary != "" && st.Binary != target:
		printf(out, "the agent (pid %d) runs %s, not %s, and that file is still %s;\n"+
			"update it, or point the service at this binary with:\n  latem service install\n",
			st.PID, st.Binary, target, st.Switch.To)
	case st.Switch.State == agent.SwitchFailed:
		printf(out, "the agent (pid %d) could not move onto %s: %s\nrestart it to:\n%s", st.PID, want, st.Switch.Error, restart)
	default:
		printf(out, "the agent (pid %d) is running %s; restart it to run %s:\n%s", st.PID, st.Version, want, restart)
	}
}
