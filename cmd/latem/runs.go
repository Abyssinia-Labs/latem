package main

import (
	"github.com/spf13/cobra"

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
		RunE: func(_ *cobra.Command, _ []string) error {
			if state != "" && !protocol.RunState(state).Valid() {
				return usageErrorf("unknown state %q — see `latem list --help` for the set", state)
			}
			// TODO(WS-11): read from internal/store through the local broker.
			// Reading sqlite directly from the CLI would take a second
			// connection to a database the agent holds in WAL mode, and the
			// agent is the process that owns reconciliation of what is in it.
			return errNotWired("latem list", "WS-11 (internal/broker/local)")
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
		RunE: func(_ *cobra.Command, args []string) error {
			// TODO(WS-11): tail ~/.latem/logs/<id>.log for the static case and
			// subscribe through the local broker for --follow, so a run being
			// written to and a run already finished behave identically.
			return errNotWired("latem logs "+args[0], "WS-11 (internal/broker/local)")
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
		RunE: func(_ *cobra.Command, args []string) error {
			// TODO(WS-11): send a protocol.CancelRequest over the local
			// socket. The reason is normalized by protocol.NewCancelCommand,
			// so no path here can produce an over-long one.
			return errNotWired("latem cancel "+args[0], "WS-11 (internal/broker/local)")
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

An unapproved run is rejected with E_APPROVAL_EXPIRED after approval_ttl,
24 hours by default, so this is a decision with a deadline.

Exit codes:
  0  approved
  1  no such run, or it was not awaiting approval
  2  usage
  3  no agent is listening`,
		Args: runIDArg,
		RunE: func(_ *cobra.Command, args []string) error {
			// TODO(WS-11): transition awaiting_approval -> queued through the
			// store, over the local socket. It must stay local: an approval
			// path reachable from a broker would undo INV-5.
			return errNotWired("latem approve "+args[0], "WS-11 (internal/broker/local)")
		},
	}
	return cmd
}
