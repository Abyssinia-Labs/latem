package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/broker/local"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func newRunCommand() *cobra.Command {
	var (
		ref    string
		inputs []string
		follow bool
		wait   bool
		detach bool
		actor  string
	)
	cmd := &cobra.Command{
		Use:   "run <repo> <job>",
		Short: "Trigger a run on this machine",
		Long: `Trigger a run on this machine.

<repo> must be a key under [repos] in config.toml; a repo absent from that
table cannot be built, which is what makes the config an allowlist rather than
a hint (RUNNER_SPEC §8, gate 1). <job> must exist in .latem/jobs.yaml at the
resolved commit — the registry is read from the checked-out workspace, never
from a broker, so a run is reproducible from the commit alone (INV-5).

Inputs are declared in the job's inputs: block. An undeclared key is a hard
rejection with E_BAD_INPUT, not a warning.

Output:
  on a terminal, the run id, then the run's log as it is produced, until the
  run finishes: as --follow. Ctrl-C stops following and leaves the run going.
  When stdout is not a terminal, the run id and nothing else, so it can be
  captured: id=$(latem run myapp test)
  --wait     block until the run reaches a terminal state; exit non-zero if it
             did not succeed
  --follow   stream log lines to stdout as they are produced, then behave
             as --wait
  --detach   queue it, print the run id, and exit, on a terminal too

Exit codes:
  0  the run succeeded, or was queued: with --detach, or when stdout is not
     a terminal and neither --wait nor --follow was given
  1  the run did not succeed
  2  usage
  3  no agent is listening, and the run was being followed or waited for
     (E_AGENT_DOWN, §7.3)

A run that is only queued is queued directly in sqlite, and this exits 0 even
when no agent is running; it starts when one does. A run that is followed or
waited for has nothing to wait for on a machine with no agent, so the answer
is exit 3 rather than a process that blocks forever, and nothing is queued.

Examples:
  latem run myapp test
  latem run myapp ios-release --ref release/1.4 --input channel=beta
  latem run myapp nightly --detach`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach && (wait || follow) {
				return usageErrorf("--detach queues the run and exits; it cannot also --wait or --follow")
			}
			parsed, err := parseInputs(inputs)
			if err != nil {
				return err
			}
			// A person at a terminal is waiting for an answer, and "queued"
			// is not one (A81). A script's stdout is a pipe or a file, and it
			// gets the id and exit 0, as before.
			byDefault := !detach && !wait && !follow && stdoutIsTerminal(cmd)
			if byDefault {
				follow = true
			}
			return submitRun(cmd, submission{
				repo:      args[0],
				job:       args[1],
				ref:       ref,
				inputs:    parsed,
				actor:     actor,
				follow:    follow,
				wait:      wait || follow,
				byDefault: byDefault,
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag, or fully-qualified ref (default: the repo's default branch)")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "job input as key=value; repeatable")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream logs until the run finishes")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run finishes")
	cmd.Flags().BoolVar(&detach, "detach", false, "queue the run and exit without following it, on a terminal too")
	cmd.Flags().StringVar(&actor, "actor", "", "who to record as having requested this (default: the current user)")
	return cmd
}

// DefaultRef is what `latem run` uses when --ref is not given.
//
// The help text promises "the repo's default branch" and this is not that, and
// the difference is worth stating rather than hiding. Learning a remote's HEAD
// means `git ls-remote --symref`, which internal/source does not expose and
// which would put a network round trip in front of a command whose whole point
// is that it queues without one. So the CLI guesses the overwhelmingly common
// answer and the run's own gates give a precise error when the guess is wrong:
// a repo whose default branch is `master` gets E_INVALID_REF naming the ref it
// tried, not a mysterious failure.
const DefaultRef = "main"

// stdoutIsTerminal reports whether cmd's stdout is a terminal, which is how
// `latem run` knows a person is watching it (A81). A seam: under `go test`
// stdout is a terminal at a desk and a pipe in CI, so a test says which.
var stdoutIsTerminal = func(cmd *cobra.Command) bool {
	f, ok := cmd.OutOrStdout().(*os.File)
	return ok && isTerminal(f)
}

// submission is the shape of one `latem run`, after flag parsing.
type submission struct {
	repo, job, ref string
	inputs         map[string]string
	actor          string
	follow, wait   bool
	// byDefault is a follow nobody asked for: stdout is a terminal and no
	// flag said otherwise (A81).
	byDefault bool
}

// submitRun queues a run and, depending on the flags, watches it.
//
// The two paths through here are §7.3's, and the asymmetry is deliberate.
// A run that is only queued (--detach, or a script's bare `latem run`) is
// queued whether or not an agent is listening, and the command exits 0 — the
// work is durable and starts when an agent does. A run that is followed or
// waited for has nothing to wait for on a machine with no agent, so the
// answer is exit 3 and no queue write at all: queueing a run that will begin
// hours later, unwatched, after the user has moved on, is worse than refusing.
func submitRun(cmd *cobra.Command, s submission) error {
	ctx := cmd.Context()
	st, closeState, err := openState(ctx)
	if err != nil {
		return err
	}
	defer closeState()

	id, err := agent.NewRunID()
	if err != nil {
		return err
	}
	if s.ref == "" {
		s.ref = DefaultRef
	}
	if s.actor == "" {
		s.actor = currentActor()
	}

	req := protocol.RunRequest{
		ID:     id,
		Repo:   s.repo,
		Job:    s.job,
		Ref:    s.ref,
		Inputs: s.inputs,
		Origin: protocol.Origin{
			Kind:        "cli",
			Actor:       s.actor,
			RequestedAt: time.Now(),
		},
	}

	sub, err := st.client.Submit(ctx, req, local.SubmitOptions{
		ExpiresAt:    time.Now().Add(st.queueTTL()),
		RequireAgent: s.wait,
	})
	if err != nil {
		err = st.agentDown(err)
		if s.byDefault && exitCode(err) == ExitAgentDown {
			// Before A81 this command queued and said so. Whoever expects
			// that is told how to get it.
			err = &ExitError{Code: ExitAgentDown, Err: fmt.Errorf("%w\nor queue it to start when one does:\n  the same command with --detach", err)}
		}
		return err
	}

	// The id goes to stdout on its own so `id=$(latem run myapp test)` works.
	// Everything else is commentary and belongs on stderr.
	printf(cmd.OutOrStdout(), "%s\n", sub.RunID)
	if !sub.AgentRunning {
		printf(cmd.ErrOrStderr(), "queued (agent not running) — it starts when one does: `latem agent`\n")
	}
	if !s.wait {
		return nil
	}
	err = watchRun(cmd, st, sub.RunID, s.follow)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		// Ctrl-C ends the watching, not the run, which an agent is running
		// and a broker may be showing; "context canceled" would say otherwise.
		return fmt.Errorf("stopped watching %s, which goes on\n  latem logs -f %[1]s   follows it again\n  latem cancel %[1]s    stops it", sub.RunID)
	}
	return err
}

// parseInputs turns repeated --input key=value flags into a map.
//
// The checks here are shape only. Whether a key is declared by the job, and
// whether its value satisfies the declared type and bounds, is decided by
// internal/registry against the registry at the resolved commit (§10) — which
// is not on disk until the run checks it out, so the CLI genuinely cannot
// answer it and should not pretend to.
//
// What it can catch is the two mistakes people make at a shell prompt: an
// input with no `=` at all, and the same key given twice, where silently
// keeping the last one is how somebody spends an afternoon on a build that
// used a value they thought they had overridden.
func parseInputs(pairs []string) (map[string]string, error) {
	return parsePairs("--input", pairs)
}

// parsePairs is [parseInputs] for any repeated key=value flag.
func parsePairs(flag string, pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok {
			return nil, usageErrorf("%s %q is not key=value", flag, p)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, usageErrorf("%s %q has an empty key", flag, p)
		}
		if _, dup := out[key]; dup {
			return nil, usageErrorf("%s %s given more than once", flag, key)
		}
		out[key] = value
	}
	return out, nil
}

// sortedKeys is used by the output formatters so two invocations against the
// same state print the same thing.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
