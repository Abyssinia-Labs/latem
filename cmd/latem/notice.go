package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// The notice that a newer release exists (A56), after gh's: at most one feed
// request a day, made while the command runs, and three lines on stderr after
// it, only on a terminal.
//
// It never fails a command and never says why it said nothing. A machine that
// is offline, rate-limited, or reading a feed that is not public yet prints
// exactly what it printed before this existed.

const (
	// noticeEvery is how stale the remembered answer may be before the feed
	// is asked again.
	noticeEvery = 24 * time.Hour
	// noticeWait is how long a finished command waits for a feed answer
	// still on its way. Past it the command exits, and the next one asks.
	noticeWait = time.Second
	// noticeFetch bounds the request itself.
	noticeFetch = 10 * time.Second
	// envNoNotice turns the notice off, by gh's convention.
	envNoNotice = "LATEM_NO_UPDATE_NOTIFIER"
)

// noticeQuiet is the commands that never show it: the daemon writes a log,
// not a terminal session; update reports releases itself; completion output
// is read by a shell.
var noticeQuiet = map[string]bool{
	"agent": true, "update": true, "completion": true,
	cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// releaseCheck is the remembered answer, in the user's cache directory.
type releaseCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest,omitempty"`
}

// Seams: a test must not ask the real feed or write the real cache.
var (
	latestRelease = func(ctx context.Context) (string, error) {
		rel, err := (&selfupdate.Updater{Current: version}).Latest(ctx)
		return rel.Version, err
	}
	noticeCache = func() (string, error) {
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "latem", "release.json"), nil
	}
	noticeNow     = time.Now
	noticeAllowed = func(stderr *os.File) bool {
		if os.Getenv(envNoNotice) != "" || os.Getenv("CI") != "" {
			return false
		}
		fi, err := stderr.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

// startNotice decides, before the command runs, whether this invocation may
// show the notice, and starts asking the feed if what it remembers is stale.
// The returned function prints the notice, if there is one, after the command.
func startNotice(root *cobra.Command, args []string, stderr *os.File) func() {
	none := func() {}
	if _, err := selfupdate.Compare(version, version); err != nil {
		// A dev build, or one whose version is not a release's: nothing to
		// compare a release with.
		return none
	}
	// args[0] as well as what Find resolves: cobra adds its completion
	// commands during Execute, so Find cannot see them yet.
	if !noticeAllowed(stderr) || noticeQuiet[topCommand(root, args)] || (len(args) > 0 && noticeQuiet[args[0]]) {
		return none
	}
	path, err := noticeCache()
	if err != nil {
		return none
	}

	known := readReleaseCheck(path)
	if noticeNow().Sub(known.CheckedAt) < noticeEvery {
		return func() { printNotice(stderr, known.Latest) }
	}

	answer := make(chan string, 1)
	fetch, now := latestRelease, noticeNow
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), noticeFetch)
		defer cancel()
		latest, err := fetch(ctx)
		next := releaseCheck{CheckedAt: now(), Latest: known.Latest}
		if err == nil {
			next.Latest = latest
		}
		// Written on failure too, so an offline machine asks once a day
		// rather than on every command.
		writeReleaseCheck(path, next)
		answer <- next.Latest
	}()
	return func() {
		latest := known.Latest
		select {
		case latest = <-answer:
		case <-time.After(noticeWait):
		}
		printNotice(stderr, latest)
	}
}

// printNotice says that latest is newer than this binary, if it is.
func printNotice(w io.Writer, latest string) {
	if latest == "" {
		return
	}
	if cmp, err := selfupdate.Compare(latest, version); err != nil || cmp <= 0 {
		return
	}
	st := newStyler(w)
	printf(w, "\n%s %s → %s\n%s\n%s\n",
		st.warn("A new release of latem is available:"), st.accent(version), st.accent(latest),
		"To upgrade, run: "+upgradeCommand(),
		st.dim(selfupdate.ReleasePage(latest)))
}

// topCommand names the subcommand of root that args run, or "" for root.
func topCommand(root *cobra.Command, args []string) string {
	cmd, _, err := root.Find(args)
	if err != nil || cmd == nil {
		return ""
	}
	for cmd.HasParent() && cmd.Parent() != root {
		cmd = cmd.Parent()
	}
	if cmd == root {
		return ""
	}
	return cmd.Name()
}

// readReleaseCheck reads the remembered answer; anything unreadable is no
// answer, and the next write replaces it.
func readReleaseCheck(path string) releaseCheck {
	var c releaseCheck
	// #nosec G304 -- path is under the user's own cache directory.
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return releaseCheck{}
	}
	if json.Unmarshal(data, &c) != nil {
		return releaseCheck{}
	}
	return c
}

// writeReleaseCheck replaces the remembered answer through a rename, so two
// commands finishing together leave one whole file.
func writeReleaseCheck(path string, c releaseCheck) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return
	}
	if tmp.Close() != nil {
		return
	}
	_ = os.Rename(name, path)
}
