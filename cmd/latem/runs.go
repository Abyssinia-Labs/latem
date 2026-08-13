package main

import (
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

// runIDArg validates a run id before anything is done with it.
//
// It is worth doing here, in the client, even though the agent validates
// again. A run id is not a label: the agent builds work/<id>, logs/<id>.log,
// artifacts/<id>/, and the container name latem-run-<id> from it, so an id
// containing a separator or a dot is a path traversal (INV-3, amendment A6).
// The CLI catches a typo before it becomes a support question, and the agent
// catches everything else, because a single validation point that everything
// downstream trusts is the shape of the next incident.
func runIDArg(cmd *cobra.Command, args []string) error {
	if err := exactArgs(1)(cmd, args); err != nil {
		return err
	}
	if err := protocol.ValidateID(args[0]); err != nil {
		return usageErrorf("invalid run id %q: %v", args[0], err)
	}
	return nil
}

func newListCommand() *cobra.Command {
	var (
		state  string
		repo   string
		limit  int
		allRun bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List runs on this machine",
		Long: `List runs on this machine, newest first.

Output is one run per line, tab-separated, so it composes with cut and awk:

  RUN ID    STATE      REPO    JOB           REF               STARTED    DURATION

By default only non-terminal runs are listed — the ones something might still
happen to. Pass --all for the history, which sqlite keeps until log retention
expires it (§22).

--state accepts any state from §11: queued, awaiting_approval, claimed,
preparing, running, collecting, succeeded, failed, timed_out, cancelled,
rejected, interrupted.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if state != "" && !protocol.RunState(state).Valid() {
				return usageErrorf("unknown state %q — see `latem list --help` for the set", state)
			}
			return listRuns(cmd, listFilter{
				state: protocol.RunState(state),
				repo:  repo,
				limit: limit,
				all:   allRun,
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "only runs in this state")
	cmd.Flags().StringVar(&repo, "repo", "", "only runs for this repo")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum runs to print")
	cmd.Flags().BoolVar(&allRun, "all", false, "include runs that have already finished")
	return cmd
}

func newLogsCommand() *cobra.Command {
	var (
		follow bool
		tail   int
	)
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Print a run's logs",
		Long: `Print a run's logs.

Lines come from ~/.latem/logs/<run-id>.log, which is written by the same
pipeline that feeds a broker and has already passed the redactor — a log frame
that exists has been redacted (INV-4, §16). There is no unredacted copy to
ask for.

Output is the raw log lines, so it pipes into grep and less unchanged.

Exit codes:
  0  the log was printed
  1  no such run, or the log has been retained away (agent.log_retention)
  2  usage
  3  --follow was given and no agent is listening`,
		Args: runIDArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return showLogs(cmd, args[0], follow, tail)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they arrive")
	cmd.Flags().IntVarP(&tail, "tail", "n", 0, "print only the last N lines before following")
	return cmd
}

func newCancelCommand() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Stop a run",
		Long: `Stop a run.

Cancellation is idempotent and its effect depends on the run's state (§11): a
queued run is cancelled outright, a running one has its context cancelled and
its process group signalled, and a run already in collecting is left to finish
— artifacts from a cancelled build are still worth having.

--reason is display text. It is capped at 200 runes, never interpolated, never
passed to a shell, and passes through the redactor before it is logged or
persisted (INV-2). It exists so a build stopped from a phone says why.

Exit codes:
  0  the run was cancelled, or had already finished
  1  no such run
  2  usage
  3  no agent is listening`,
		Args: runIDArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cancelRun(cmd, args[0], reason)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, for display (max 200 runes)")
	return cmd
}

func newApproveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a run whose ref falls outside policy",
		Long: `Approve a run parked in awaiting_approval.

A run reaches that state when its ref does not match the repo's allowed_refs
and require_approval_otherwise is set (INV-5). Approval is a local action
only: a broker cannot grant it, and no inbound command kind can express it —
the command set is closed at run and cancel, and a third kind is a security
review rather than a feature (INV-2).

What you approve is a commit, not a ref. The run was parked with the commit
its ref resolved to — ` + "`latem list`" + ` shows the normalized ref, and this command
prints the commit it pinned — and that is the only commit it will build. If
the ref moves before a runner picks the run up, it is parked again rather than
built, and you are asked about the new commit. A branch is something a
contributor can force-push; approving one would be approving whoever pushes
last.

An unapproved run is rejected with E_APPROVAL_EXPIRED after approval_ttl,
24 hours by default, so this is a decision with a deadline.

Exit codes:
  0  approved
  1  no such run, it was not awaiting approval, or it has no resolved commit
  2  usage
  3  no agent is listening`,
		Args: runIDArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return approveRun(cmd, args[0])
		},
	}
	return cmd
}

// listFilter is `latem list`'s flags, after validation.
type listFilter struct {
	state protocol.RunState
	repo  string
	limit int
	all   bool
}

// states resolves the flags into the set the store filters on.
//
// The default is the non-terminal states — the runs something might still
// happen to — because that is the question an operator standing at a build
// machine is asking. History is one flag away and, unlike the default, it
// grows without bound.
func (f listFilter) states() []protocol.RunState {
	switch {
	case f.state != "":
		return []protocol.RunState{f.state}
	case f.all:
		return nil
	default:
		return []protocol.RunState{
			protocol.StateQueued,
			protocol.StateAwaitingApproval,
			protocol.StateClaimed,
			protocol.StatePreparing,
			protocol.StateRunning,
			protocol.StateCollecting,
		}
	}
}

// listRuns prints one run per line, tab separated.
//
// Raw tabs rather than an aligned table, because the help promises output that
// composes with cut and awk and a tabwriter's padding would make every column
// position depend on the longest value in it.
func listRuns(cmd *cobra.Command, f listFilter) error {
	ctx := cmd.Context()
	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()

	runs, err := st.db.List(ctx, store.Filter{
		Repo:   f.repo,
		States: f.states(),
		Limit:  f.limit,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if len(runs) == 0 {
		// Not an error, and not silence either: an empty list and a wrong
		// filter look identical, and one of them is a mistake worth naming.
		printf(cmd.ErrOrStderr(), "no runs match — `latem list --all` includes finished ones\n")
		return nil
	}

	rows := make([][]string, 0, len(runs))
	for _, r := range runs {
		started := "-"
		if r.StartedAt != nil {
			started = formatAge(*r.StartedAt)
		}
		rows = append(rows, []string{
			r.ID, string(r.State), r.Repo, r.Job, refOf(r), started, runDuration(r),
		})
	}
	header := []string{"RUN ID", "STATE", "REPO", "JOB", "REF", "STARTED", "DURATION"}

	st2 := newStyler(out)
	if !st2.color {
		// Piped, redirected, or NO_COLOR. Stay exactly as before: raw tabs, so
		// the output still composes with cut and awk as the help promises. A
		// tabwriter's padding would make every column position depend on the
		// longest value in it, which is the opposite of machine-readable.
		printf(out, "%s\n", strings.Join(header, "\t"))
		for _, row := range rows {
			printf(out, "%s\n", strings.Join(row, "\t"))
		}
		return nil
	}

	// A terminal. Align the columns and colour the state, because a human
	// scanning twenty runs is looking for the failed one and nothing else.
	// State keeps its word as well as its colour: colour is decoration here,
	// never the only carrier of meaning.
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if n := len(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	var b strings.Builder
	for i, h := range header {
		b.WriteString(st2.column(st2.dim(h), widths[i]+2))
	}
	printf(out, "%s\n", strings.TrimRight(b.String(), " "))

	for _, row := range rows {
		b.Reset()
		for i, cell := range row {
			text := cell
			if i == 1 {
				text = st2.paintState(protocol.RunState(cell))
			}
			b.WriteString(st2.column(text, widths[i]+2))
		}
		printf(out, "%s\n", strings.TrimRight(b.String(), " "))
	}
	return nil
}

// refOf prefers the normalized ref, because that is what policy matched and
// what the SHA came from (INV-5). It falls back to the ref as supplied for a
// run that never got as far as gate 7.
func refOf(r store.Run) string {
	if r.RefNormalized != "" {
		return r.RefNormalized
	}
	return r.Ref
}

// showLogs prints a run's log (§16).
func showLogs(cmd *cobra.Command, runID string, follow bool, tailLines int) error {
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

	if !follow {
		if err := printLog(cmd, st, runID, tailLines); errors.Is(err, os.ErrNotExist) {
			return noLogYet(runID)
		} else if err != nil {
			return err
		}
		return nil
	}

	// §7.3's exit 3: following needs something to follow. A terminal run is
	// the exception — its log is complete and there is nothing to wait for, so
	// refusing would be pedantry.
	if !r.State.IsTerminal() {
		if err := st.requireAgent(ctx); err != nil {
			return err
		}
	}

	// A run that is queued has no log file yet, and that is the ordinary case
	// for `latem run … --follow` on a busy machine. Treating the missing file
	// as an error here would make -f useless for exactly the runs somebody
	// wants to watch; the tail reopens on every tick until it appears.
	err = printLog(cmd, st, runID, tailLines)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if r.State.IsTerminal() {
			return noLogYet(runID)
		}
	case err != nil:
		return err
	}
	if r.State.IsTerminal() {
		return nil
	}

	t := newLogTail(st.cfg.Paths.Logs, runID)
	defer t.close()
	if err == nil {
		// Only skip what was already printed. A log that did not exist a
		// moment ago must be streamed from its first byte.
		t.seekEnd()
	}
	return waitForRun(ctx, st, runID, t, cmd.OutOrStdout())
}

// noLogYet is the exit-1 answer for a run with nothing to show.
func noLogYet(runID string) error {
	return fmt.Errorf("no log for %s — it has not started yet, or agent.log_retention has expired it", runID)
}

// printLog writes the log as it stands, honouring --tail. A missing log is
// reported as [os.ErrNotExist] so the caller can decide whether that is a
// failure or simply a run that has not started.
func printLog(cmd *cobra.Command, st *state, runID string, tailLines int) error {
	rc, err := readLog(st.cfg.Paths.Logs, runID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	if tailLines <= 0 {
		_, err = io.Copy(cmd.OutOrStdout(), rc)
		return err
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	printf(cmd.OutOrStdout(), "%s", lastLines(string(data), tailLines))
	return nil
}

// cancelRun stops a run (§11).
func cancelRun(cmd *cobra.Command, runID, reason string) error {
	ctx := cmd.Context()
	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()

	// The help promises exit 1 for an unknown id, and §7.1 promises that a
	// cancel for an unknown run is a no-op reporting success. Both are right
	// for their own caller: a broker must not learn which ids exist by which
	// cancels error, and a person at a terminal who mistyped an id needs to be
	// told. The lookup is local and the distinction stops here.
	if _, err := st.db.Get(ctx, runID); isNotFound(err) {
		return noSuchRun(runID)
	} else if err != nil {
		return err
	}

	req := protocol.CancelRequest{ID: runID, Actor: currentActor(), Reason: reason}
	if err := st.client.Cancel(ctx, req); err != nil {
		return st.agentDown(err)
	}
	printf(cmd.ErrOrStderr(), "cancelled %s\n", runID)
	return nil
}

// approveRun releases a run parked by INV-5's ref policy.
//
// The commit is printed rather than only the run id. Approval pins one commit,
// so the line an operator sees afterwards should be the same fact the security
// property rests on — and if it is not the commit they expected, that is
// exactly when they want to know.
func approveRun(cmd *cobra.Command, runID string) error {
	ctx := cmd.Context()
	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()

	r, err := agent.Approve(ctx, st.db, runID, st.queueTTL(), time.Now())
	switch {
	case isNotFound(err):
		return noSuchRun(runID)
	case errors.Is(err, agent.ErrNoCommit):
		// A run parked before the agent recorded commits, or one whose park
		// never got that far. Nothing can be pinned, so nothing is approved.
		return fmt.Errorf(
			"%s has no resolved commit to approve — cancel it and submit the run again", runID)
	case err != nil:
		return err
	}

	if r.ApprovedSHA == "" {
		// The idempotent case, reached by approving a run that is already in
		// the queue. Saying "approved ... at " with an empty commit would be
		// worse than saying nothing: the commit is the fact this command
		// exists to establish.
		printf(cmd.ErrOrStderr(), "%s is already queued; nothing was waiting on an approval\n", runID)
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

// shortSHA abbreviates a commit for display. Twelve characters is git's own
// habit for a value a person reads and compares by eye.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
