package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/store"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// minTypedSHA is the shortest abbreviation of a commit `latem approve` takes:
// git's own default, which `git log --oneline` and a pull request's page show.
const minTypedSHA = 7

// approveRun releases a run held for approval (INV-5, A85), once a person at
// this machine's terminal has read its commit and typed it (A89).
//
// The commit is printed rather than only the run id. Approval pins one commit,
// so the line an operator sees afterwards should be the same fact the security
// property rests on — and if it is not the commit they expected, that is
// exactly when they want to know.
func approveRun(cmd *cobra.Command, runID string) error {
	// First, before the config or the store is read: a caller with no
	// terminal learns that it cannot approve, and nothing about the run.
	in, out, err := personAtTerminal(cmd, runID)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()

	r, err := st.db.Get(ctx, runID)
	if isNotFound(err) {
		return noSuchRun(runID)
	}
	if err != nil {
		return err
	}
	switch {
	case r.State == protocol.StateQueued:
		// Approving twice is something a person does. There is nothing to
		// ask about a run that is no longer held, and the second answer
		// should not read as an error.
		alreadyQueued(cmd.ErrOrStderr(), r)
		return nil
	case r.State != protocol.StateAwaitingApproval:
		return fmt.Errorf("%s is %s, not awaiting approval: there is nothing to approve", runID, r.State)
	case r.SHA == "":
		return noCommitToApprove(runID)
	}

	showHeld(out, newStyler(out), r)
	typed, err := askForCommit(ctx, in, out)
	if err != nil {
		return err
	}
	if err := matchCommit(typed, r.SHA); err != nil {
		return fmt.Errorf("not approved: %w. %s is still held, and `latem approve %s` asks again", err, runID, runID)
	}

	// The commit the person typed, not whatever the row holds by now: the
	// store pins this one or nothing, in one transaction.
	r, err = agent.Approve(ctx, st.db, runID, r.SHA, st.queueTTL(), time.Now())
	switch {
	case isNotFound(err):
		return noSuchRun(runID)
	case errors.Is(err, agent.ErrNoCommit):
		return noCommitToApprove(runID)
	case errors.Is(err, agent.ErrCommitChanged):
		return fmt.Errorf("not approved: while you answered, %s was approved elsewhere, built, and held again "+
			"at another commit. `latem approve %s` shows that one", runID, runID)
	case errors.Is(err, agent.ErrNotAwaitingApproval):
		return fmt.Errorf("not approved: while you answered, %s stopped waiting. It was approved elsewhere, "+
			"cancelled, or its approval expired (`latem list --all`)", runID)
	case err != nil:
		return err
	}

	if r.ApprovedSHA == "" {
		alreadyQueued(cmd.ErrOrStderr(), r)
		return nil
	}
	printf(cmd.ErrOrStderr(), "approved %s at %s (%s) — it is queued\n",
		runID, shortSHA(r.ApprovedSHA), refOf(r))
	if !st.client.AgentRunning(ctx) {
		// Not an error. Approval is a store transition, so it holds whether or
		// not an agent is up; saying nothing would leave the user watching a
		// queue that nothing is draining.
		printf(cmd.ErrOrStderr(), "no agent is running, so it starts when one does: `latem agent`\n")
	}
	return nil
}

// personAtTerminal returns cmd's stdin and stdout when both are terminals,
// and otherwise refuses with [ExitNoTerminal] (A89).
//
// It stops what an agent runs without a terminal: Claude Code's shell, `latem
// mcp`'s ssh, a script, a pipe. It does not stop one that makes a terminal
// for itself, with `ssh -tt` or script(1). It is a guard that keeps an agent
// from approving in the course of its work, not a boundary against someone
// who holds this account and means to.
func personAtTerminal(cmd *cobra.Command, runID string) (in, out *os.File, err error) {
	in, out, which := terminalPair(cmd)
	if which == "" {
		return in, out, nil
	}
	return nil, nil, &ExitError{Code: ExitNoTerminal, Err: fmt.Errorf(
		"approving needs a person at a terminal, and %s (A89).\n\n"+
			"A held run is released by the person who runs this machine, after reading the commit it would "+
			"build and typing it here. If that is you, run `latem approve %s` in a terminal on this machine. "+
			"An agent cannot approve a run: ask that person to", which, runID)}
}

// terminalPair returns cmd's stdin and stdout when both are terminals, the
// question isatty(3) asks (A89), and otherwise says which is not. `latem
// approve` and the two settings `latem setup apply` writes only for a person
// (A94) ask it.
func terminalPair(cmd *cobra.Command) (in, out *os.File, notOne string) {
	in, _ = cmd.InOrStdin().(*os.File)
	out, _ = cmd.OutOrStdout().(*os.File)
	inOK, outOK := isTTY(in), isTTY(out)
	switch {
	case inOK && outOK:
		return in, out, ""
	case inOK:
		return nil, nil, "stdout is not one"
	case outOK:
		return nil, nil, "stdin is not one"
	}
	return nil, nil, "neither stdin nor stdout is one"
}

// showHeld prints what a person approving r is asked about: the commit, and
// why it is held, which is the line its log ends with. For a pull request's
// job that names the secrets, the environment and the runner it asks for
// (A85); for a ref outside the policy, the ref.
//
// Every value goes through displayText. A pull request writes its own secret
// names and environment, and the control plane names the event and who sent
// it. An escape sequence in any of them could redraw the screen a person is
// reading to decide.
func showHeld(w io.Writer, sty styler, r store.Run) {
	rows := [][2]string{
		{"run", r.ID + "  " + r.Repo + " / " + r.Job},
		{"ref", refOf(r)},
		{"commit", r.SHA},
	}
	if wf := r.Workflow; wf != nil {
		rows = append(rows, [2]string{"workflow", wf.Workflow + ", on " + wf.Event.Name})
	}
	from := r.OriginKind
	if r.OriginActor != "" {
		from += " by " + r.OriginActor
	}
	rows = append(rows, [2]string{"from", from})
	if r.ErrorMsg != "" {
		rows = append(rows, [2]string{"held", r.ErrorMsg})
	}
	if r.ExpiresAt != nil {
		rows = append(rows, [2]string{"expires", expiresIn(*r.ExpiresAt)})
	}

	printf(w, "%s\n\n", sty.heading("A run is waiting for your approval"))
	for _, row := range rows {
		v := displayText(row[1])
		if row[0] == "commit" {
			v = sty.bold(v)
		}
		printf(w, "  %-9s %s\n", row[0], v)
	}
	printf(w, "\n%s\n\n", sty.dim("Its log so far: latem logs "+r.ID))
}

// expiresIn says when an unapproved run is rejected with E_APPROVAL_EXPIRED.
func expiresIn(t time.Time) string {
	at := t.Local().Format("2006-01-02 15:04")
	d := time.Until(t)
	switch {
	case d <= 0:
		return at + ", already passed"
	case d < time.Hour:
		return fmt.Sprintf("%s, in %dm", at, int(d.Minutes()))
	default:
		return fmt.Sprintf("%s, in %dh", at, int(d.Hours()))
	}
}

// askForCommit asks for the commit's short sha and reads the line typed.
//
// What was typed before the question is thrown away first, so the answer is
// one typed after the commit was on the screen. Ctrl-C cancels the context,
// which ends the wait; the read itself cannot be interrupted, and is left to
// end with the process.
func askForCommit(ctx context.Context, in, out *os.File) (string, error) {
	if err := discardTypeAhead(in); err != nil {
		return "", fmt.Errorf("clearing what was typed ahead: %w", err)
	}
	printf(out, "To approve this commit, type its short sha (Enter leaves it held): ")

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		printf(out, "\n")
		return "", fmt.Errorf("not approved: %w", ctx.Err())
	case r := <-done:
		if r.err != nil && !errors.Is(r.err, io.EOF) {
			return "", r.err
		}
		if errors.Is(r.err, io.EOF) {
			printf(out, "\n")
		}
		return strings.TrimSpace(r.line), nil
	}
}

// matchCommit reports whether typed names sha: at least [minTypedSHA] of its
// characters, from the start, in either case.
func matchCommit(typed, sha string) error {
	typed = strings.ToLower(strings.TrimSpace(typed))
	switch {
	case typed == "":
		return errors.New("nothing was typed")
	case !strings.HasPrefix(strings.ToLower(sha), typed):
		return fmt.Errorf("%q is not the start of commit %s", displayText(typed), shortSHA(sha))
	case len(typed) < minTypedSHA:
		return fmt.Errorf("type at least %d characters of the commit", minTypedSHA)
	}
	return nil
}

// alreadyQueued is the answer for a run no longer held: approved before, or
// never waiting on anybody.
func alreadyQueued(w io.Writer, r store.Run) {
	if r.ApprovedSHA == "" {
		// Saying "approved ... at " with an empty commit would be worse than
		// saying nothing: the commit is the fact this command exists to
		// establish.
		printf(w, "%s is already queued; nothing was waiting on an approval\n", r.ID)
		return
	}
	printf(w, "%s is already approved at %s (%s) and queued\n", r.ID, shortSHA(r.ApprovedSHA), refOf(r))
}

// noCommitToApprove is a run parked before the agent recorded commits, or one
// whose park never got that far. Nothing can be pinned, so nothing is
// approved.
func noCommitToApprove(runID string) error {
	return fmt.Errorf("%s has no resolved commit to approve — cancel it and submit the run again", runID)
}
