package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/result"
	"github.com/Abyssinia-Labs/latem/internal/store"
)

func newResultCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "result <run-id>",
		Short: "Print how a run ended: its steps, and the errors their output named",
		Long: `Print how a run ended: each step and how it went, and what the steps' output
named as broken, each with the file and line the tool put on it
(RUNNER_SPEC A92).

A log is everything a run printed. This is what a program that started the
run wants out of it: did it pass, which step did not, and what broke, where.
It is read off the run's log as the run goes — a compiler's errors, a linter's
findings, a failed test and where it failed, and anything a step wrote itself
as ` + "`::error file=…,line=…::message`" + ` — after the redactor, so it holds
nothing the log does not. A suggestion is there when the tool made one, and is
the tool's own.

It is written as each step ends, so a run still going shows the steps so far.
It is kept beside the log, for as long as the log is.

  --json   the whole result as one JSON document, for a program

Exit codes:
  0  the result was printed, whatever the run's state: the state is in the
     output, as with latem logs
  1  no such run
  2  usage`,
		Args: runIDArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, closeState, err := openState(ctx)
			if err != nil {
				return err
			}
			defer closeState()
			r, err := st.db.Get(ctx, args[0])
			if isNotFound(err) {
				return noSuchRun(args[0])
			}
			if err != nil {
				return err
			}
			res, err := runResult(st.cfg.Paths.Logs, r)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			printResult(cmd.OutOrStdout(), res)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

// runResult is a run's result: the one its agent wrote, brought up to date
// with the run's row, or, for a run that has none, as much as the row says.
//
// The row outranks the file where they differ. The file is written by the
// run's own goroutine, and a run that was stopped from outside it — the agent
// killed, the machine restarted — settled in the store and never wrote again.
func runResult(logDir string, r store.Run) (*result.Result, error) {
	res, err := result.Read(logDir, r.ID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		res = &result.Result{
			Version: result.Version, RunID: r.ID, Repo: r.Repo, Job: r.Job, Ref: refOf(r), SHA: r.SHA,
			Base: r.ChangesBase(), Inputs: r.Inputs, Runner: r.Runner,
			Steps: []result.Step{}, Problems: []result.Problem{},
			Note: "no result is stored for this run: it has not started a step, or its log's retention removed it",
		}
	case err != nil:
		return nil, err
	}
	// A file from before a list was always written reads back as nil.
	if res.Steps == nil {
		res.Steps = []result.Step{}
	}
	if res.Problems == nil {
		res.Problems = []result.Problem{}
	}
	if res.Settled && res.State == r.State {
		return res, nil
	}
	wasSettled := res.Settled
	res.State, res.Settled = r.State, r.State.IsTerminal()
	res.ErrorCode, res.Error, res.ExitCode = string(r.ErrorCode), r.ErrorMsg, r.ExitCode
	res.StartedAt, res.FinishedAt = r.StartedAt, r.FinishedAt
	if r.StartedAt != nil && r.FinishedAt != nil {
		res.DurationMs = r.FinishedAt.Sub(*r.StartedAt).Milliseconds()
	}
	if res.SHA == "" && r.Workflow != nil {
		res.SHA = r.Workflow.SHA
	}
	if res.Settled && !wasSettled && res.Note == "" {
		res.Note = "the run settled without its agent writing the result's last word: the steps and problems are as far as it got"
	}
	return res, nil
}

func printResult(w io.Writer, res *result.Result) {
	st := newStyler(w)
	head := fmt.Sprintf("%s %s", res.RunID, res.State)
	if res.ErrorCode != "" {
		head += " (" + res.ErrorCode + ")"
	}
	if res.Error != "" {
		head += ": " + res.Error
	}
	printf(w, "%s\n", st.heading(head))
	what := "job " + res.Job + " of " + res.Repo
	if res.SHA != "" {
		what += fmt.Sprintf(" at %.12s", res.SHA)
	}
	about := []string{what}
	if res.Base != "" && res.Base != res.SHA {
		about = append(about, fmt.Sprintf("changes on %.12s", res.Base))
	}
	if res.Runner != "" {
		about = append(about, "on "+res.Runner)
	}
	if res.Workspace != "" {
		about = append(about, res.Workspace+" workspace")
	}
	if res.DurationMs > 0 {
		about = append(about, shortMs(res.DurationMs))
	}
	printf(w, "  %s\n", st.dim(strings.Join(about, ", ")))
	if res.Note != "" {
		printf(w, "  %s\n", st.dim(res.Note))
	}

	if len(res.Steps) > 0 {
		printf(w, "\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		printf(tw, "STEP\tOUTCOME\tTIME\tERRORS\tWARNINGS\n")
		for _, s := range res.Steps {
			outcome := s.Outcome
			if s.Forgiven {
				outcome += " (continued)"
			}
			if s.ExitCode != nil && *s.ExitCode != 0 {
				outcome += fmt.Sprintf(", exit %d", *s.ExitCode)
			}
			printf(tw, "%s\t%s\t%s\t%d\t%d\n", s.Name, outcome, shortMs(s.DurationMs), s.Errors, s.Warnings)
		}
		_ = tw.Flush()
	}

	if len(res.Problems) == 0 {
		return
	}
	summary := fmt.Sprintf("%d error(s), %d warning(s)", res.Errors, res.Warnings)
	if res.Dropped > 0 {
		summary += fmt.Sprintf("; the first %d are shown", len(res.Problems))
	}
	printf(w, "\n%s\n", summary)
	for _, p := range res.Problems {
		where := p.File
		if p.Line > 0 {
			where += fmt.Sprintf(":%d", p.Line)
			if p.Column > 0 {
				where += fmt.Sprintf(":%d", p.Column)
			}
		}
		line := p.Severity
		if p.Code != "" {
			line += "[" + p.Code + "]"
		}
		if where != "" {
			line += " " + where
		}
		if p.Test != "" {
			line += "  " + p.Test
		}
		printf(w, "%s  %s\n", line, st.dim("("+p.Tool+", step "+p.Step+")"))
		for _, l := range strings.Split(p.Message, "\n") {
			printf(w, "    %s\n", l)
		}
		if p.Help != "" {
			printf(w, "    %s\n", st.dim("help: "+p.Help))
		}
	}
}

// shortMs renders a duration in milliseconds the way a person reads one.
func shortMs(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
