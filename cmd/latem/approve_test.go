package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

const heldSHA = "0123456789abcdef0123456789abcdef01234567"

// seedHeldPullRequest is a pull request's job held by A85, as the agent leaves
// it: the commit, and the line its log ends with.
func seedHeldPullRequest(t *testing.T, db *store.DB, id string) {
	t.Helper()
	expires := time.Now().Add(23 * time.Hour)
	r := store.Run{
		ID:            id,
		Repo:          "app",
		Job:           "deploy",
		Ref:           "refs/pull/7/merge",
		RefNormalized: "refs/pull/7/merge",
		SHA:           heldSHA,
		State:         protocol.StateAwaitingApproval,
		ErrorMsg: "a pull request's job asks for secrets DEPLOY_KEY of environment production; " +
			"read its commit, then approve it with `latem approve " + id + "` — this would build 0123456789ab",
		// The control plane names who sent it, and a terminal would act on an
		// escape sequence in the name.
		OriginKind:  "remote",
		OriginActor: "octo\x1b[2J\x1b[Hcat",
		CreatedAt:   time.Now().Add(-time.Minute),
		ExpiresAt:   &expires,
	}
	if err := db.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// terminalSession is a person at a terminal: what they type before the
// command asks anything, and what they answer when the screen shows prompt.
type terminalSession struct {
	typeAhead string
	prompt    string
	answer    string
}

// askedForSHA is the end of the question `latem approve` asks.
const askedForSHA = "type its short sha"

// atTerminal runs latem with a pseudo-terminal as its stdin and stdout, as a
// person's shell would, and plays s on the other side. It returns what the
// screen showed, what went to stderr, and the command's error.
func atTerminal(t *testing.T, s terminalSession, args ...string) (screen, stderr string, err error) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)

	var shown lockedBuffer
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(&shown, master)
	}()
	waitForScreen := func(want string, stop <-chan struct{}) bool {
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(shown.String(), want) {
			select {
			case <-stop:
				return false
			default:
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(5 * time.Millisecond)
		}
		return true
	}

	if s.typeAhead != "" {
		if _, err := master.Write([]byte(s.typeAhead + "\n")); err != nil {
			t.Fatal(err)
		}
		// The terminal echoes a line once its line discipline has queued it,
		// and not before: on Linux a write to the master is queued later, by
		// a worker. Until the echo, the line could arrive after the command
		// has cleared what was typed ahead, and be read as an answer.
		if !waitForScreen(s.typeAhead, nil) {
			t.Fatalf("the terminal never echoed %q", s.typeAhead)
		}
	}

	done := make(chan struct{})
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		if s.prompt == "" || !waitForScreen(s.prompt, done) {
			return
		}
		_, _ = master.Write([]byte(s.answer + "\n"))
	}()

	root := newRootCommand()
	var errBuf bytes.Buffer
	root.SetIn(slave)
	root.SetOut(slave)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())

	close(done)
	<-typed
	_ = master.Close()
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
		t.Error("the screen was still being read five seconds after the command returned")
	}
	return shown.String(), errBuf.String(), err
}

// withStdio runs latem with the given stdin and stdout.
func withStdio(t *testing.T, in io.Reader, out io.Writer, args ...string) (stderr string, err error) {
	t.Helper()
	root := newRootCommand()
	var errBuf bytes.Buffer
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.ExecuteContext(context.Background())
	return errBuf.String(), err
}

// stillHeld fails unless id is held as it was seeded, approved by nobody.
func stillHeld(t *testing.T, db *store.DB, id string) {
	t.Helper()
	r, err := db.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != protocol.StateAwaitingApproval || r.ApprovedSHA != "" || r.ApprovedAt != nil {
		t.Fatalf("%s is %s, approved %q at %v; it should still be held", id, r.State, r.ApprovedSHA, r.ApprovedAt)
	}
}

// With no terminal there is nobody to ask, so `latem approve` refuses, with
// exit 4, before it reads anything (A89). That is the shell Claude Code runs
// commands in, `latem mcp` over ssh, and a script.
func TestApprove_RefusesWithoutATerminal(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedHeldPullRequest(t, db, "r_pr")

	refused := func(t *testing.T, err error, which string) {
		t.Helper()
		if got := exitCode(err); got != ExitNoTerminal {
			t.Fatalf("exit code = %d (%v), want %d", got, err, ExitNoTerminal)
		}
		for _, want := range []string{"needs a person at a terminal", which, "An agent cannot approve"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal should say %q:\n%v", want, err)
			}
		}
		stillHeld(t, db, "r_pr")
	}

	t.Run("pipes", func(t *testing.T) {
		stdout, stderr, err := execCommand(t, "approve", "r_pr")
		refused(t, err, "neither stdin nor stdout is one")
		if stdout != "" || stderr != "" {
			t.Errorf("a refusal should show nothing of the run:\nstdout %q\nstderr %q", stdout, stderr)
		}
	})

	// /dev/null and /dev/zero are character devices, which is all isTerminal
	// asks of a file. Neither is a terminal, and nobody types into either.
	for _, dev := range []string{os.DevNull, "/dev/zero"} {
		t.Run("stdin "+dev, func(t *testing.T) {
			in, err := os.Open(dev)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = in.Close() }()
			_, slave := openPTY(t)
			_, err = withStdio(t, in, slave, "approve", "r_pr")
			refused(t, err, "stdin is not one")
		})
	}

	t.Run("stdout a pipe", func(t *testing.T) {
		_, slave := openPTY(t)
		var out bytes.Buffer
		_, err := withStdio(t, slave, &out, "approve", "r_pr")
		refused(t, err, "stdout is not one")
	})

	// Before the config: an agent with no terminal learns it cannot approve,
	// not whether this machine has a config or the run exists.
	t.Run("no config", func(t *testing.T) {
		t.Setenv("LATEM_CONFIG_DIR", filepath.Join(t.TempDir(), "nothing-here"))
		_, _, err := execCommand(t, "approve", "r_pr")
		refused(t, err, "neither stdin nor stdout is one")
	})
}

// At a terminal it shows the commit and why the run is held, and a sha that
// is not the commit's approves nothing.
func TestApprove_AtATerminal_WrongSHA(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedHeldPullRequest(t, db, "r_pr")

	for _, tc := range []struct {
		name, answer, want string
	}{
		{"another commit", "fedcba987654", "is not the start of commit 0123456789ab"},
		{"too short", heldSHA[:6], "at least 7 characters"},
		{"nothing", "", "nothing was typed"},
		{"the run id", "r_pr", "is not the start of commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			screen, stderr, err := atTerminal(t,
				terminalSession{prompt: askedForSHA, answer: tc.answer}, "approve", "r_pr")
			if got := exitCode(err); got != ExitFailure {
				t.Fatalf("exit code = %d (%v), want %d", got, err, ExitFailure)
			}
			for _, want := range []string{"not approved", tc.want, "still held"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error should say %q: %v", want, err)
				}
			}
			if strings.Contains(stderr, "approved r_pr") {
				t.Errorf("stderr claims an approval:\n%s", stderr)
			}
			if !strings.Contains(screen, heldSHA) {
				t.Errorf("the screen should show the whole commit before asking:\n%s", screen)
			}
			stillHeld(t, db, "r_pr")
		})
	}
}

// The commit's short sha, typed at the terminal, approves it: that commit and
// no other, as `latem approve` always has (A27).
func TestApprove_AtATerminal_RightSHA(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)

	for _, tc := range []struct{ name, id, answer string }{
		{"as git log --oneline shows it", "r_seven", heldSHA[:7]},
		{"as latem shows it, in capitals", "r_twelve", strings.ToUpper(heldSHA[:12])},
		{"all of it", "r_whole", "  " + heldSHA + " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			seedHeldPullRequest(t, db, id)

			screen, stderr, err := atTerminal(t,
				terminalSession{prompt: askedForSHA, answer: tc.answer}, "approve", id)
			if err != nil {
				t.Fatalf("approve: %v\nscreen:\n%s", err, screen)
			}
			for _, want := range []string{
				"commit    " + heldSHA,
				"ref       refs/pull/7/merge",
				"asks for secrets DEPLOY_KEY of environment production",
				"from      remote by octo[2J[Hcat",
				"latem logs " + id,
			} {
				if !strings.Contains(screen, want) {
					t.Errorf("the screen should show %q before asking:\n%s", want, screen)
				}
			}
			// The pull request and the control plane wrote some of that, and a
			// terminal would act on an escape sequence in it.
			if strings.Contains(screen, "\x1b") {
				t.Errorf("the screen carries an escape sequence from the run's own text:\n%q", screen)
			}
			if !strings.Contains(stderr, "approved "+id+" at "+heldSHA[:12]) {
				t.Errorf("stderr should name the commit it pinned:\n%s", stderr)
			}

			r, err := db.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if r.State != protocol.StateQueued || r.ApprovedSHA != heldSHA || r.ApprovedAt == nil {
				t.Errorf("after approval: state %s, approved %q at %v", r.State, r.ApprovedSHA, r.ApprovedAt)
			}
		})
	}
}

// What was typed before the question is thrown away. Here it is the right
// sha, typed before the commit was on the screen, and the Enter typed after
// it leaves the run held.
func TestApprove_AtATerminal_IgnoresTypeAhead(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedHeldPullRequest(t, db, "r_pr")

	_, _, err := atTerminal(t,
		terminalSession{typeAhead: heldSHA[:12], prompt: askedForSHA, answer: ""}, "approve", "r_pr")
	if err == nil || !strings.Contains(err.Error(), "nothing was typed") {
		t.Fatalf("approve = %v, want the type-ahead discarded and nothing typed", err)
	}
	stillHeld(t, db, "r_pr")
}

// What `latem approve` did before A89 it still does, at a terminal. A run not
// held asks nothing.
func TestApprove_AtATerminal_RunsNotHeld(t *testing.T) {
	dir := newClientConfigDir(t)
	db := clientStore(t, dir)
	seedParkedCLIRun(t, db, "r_parked", heldSHA)

	if _, _, err := atTerminal(t,
		terminalSession{prompt: askedForSHA, answer: heldSHA[:12]}, "approve", "r_parked"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Approving twice is something a person does. The second one succeeds,
	// asks nothing, and repeats the commit that is pinned.
	screen, stderr, err := atTerminal(t, terminalSession{}, "approve", "r_parked")
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if !strings.Contains(stderr, "already approved at "+heldSHA[:12]) {
		t.Errorf("a second approve should name the pinned commit:\n%s", stderr)
	}
	if strings.Contains(screen, askedForSHA) {
		t.Errorf("a second approve asked again:\n%s", screen)
	}

	// An ordinary queued run was never waiting on anybody, and no commit was
	// approved. Printing "approved ... at " with nothing after it would
	// misrepresent the one fact this command exists to establish.
	seedCLIRun(t, db, "r_plain", protocol.StateQueued)
	_, stderr, err = atTerminal(t, terminalSession{}, "approve", "r_plain")
	if err != nil {
		t.Fatalf("approving an already-queued run: %v", err)
	}
	if !strings.Contains(stderr, "already queued") {
		t.Errorf("approving a queued run should say so:\n%s", stderr)
	}

	seedCLIRun(t, db, "r_running", protocol.StateRunning)
	if _, _, err := atTerminal(t, terminalSession{}, "approve", "r_running"); err == nil ||
		!strings.Contains(err.Error(), "not awaiting approval") {
		t.Errorf("approving a running run = %v, want it refused", err)
	}

	// A run with no resolved commit has nothing to pin, and the message has to
	// say so rather than reading as a database error.
	seedCLIRun(t, db, "r_nocommit", protocol.StateAwaitingApproval)
	if _, _, err := atTerminal(t, terminalSession{}, "approve", "r_nocommit"); err == nil ||
		!strings.Contains(err.Error(), "no resolved commit") {
		t.Errorf("approving a run with no commit = %v, want it to say what is missing", err)
	}

	if _, _, err := atTerminal(t, terminalSession{}, "approve", "r_nosuchrun"); exitCode(err) != ExitFailure {
		t.Errorf("approving an unknown run = %v, want exit 1", err)
	}
}

func TestMatchCommit(t *testing.T) {
	for _, tc := range []struct {
		typed string
		ok    bool
	}{
		{heldSHA[:7], true},
		{heldSHA[:12], true},
		{heldSHA, true},
		{strings.ToUpper(heldSHA[:9]), true},
		{" " + heldSHA[:8] + "\t", true},
		{heldSHA[:6], false},
		{"", false},
		{heldSHA + "0", false},
		{"123456789abc", false},
		{"y", false},
	} {
		if err := matchCommit(tc.typed, heldSHA); (err == nil) != tc.ok {
			t.Errorf("matchCommit(%q) = %v, want ok %v", tc.typed, err, tc.ok)
		}
	}
}
