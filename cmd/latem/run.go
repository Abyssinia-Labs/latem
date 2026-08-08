package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newRunCommand() *cobra.Command {
	var (
		ref    string
		inputs []string
		follow bool
		wait   bool
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
  by default, the run id on stdout and nothing else, so it can be captured
  --wait     block until the run reaches a terminal state; exit non-zero if it
             did not succeed
  --follow   stream log lines to stdout as they are produced, then behave
             as --wait

Exit codes:
  0  the run succeeded, or was accepted when neither --wait nor --follow was given
  1  the run did not succeed
  2  usage
  3  no agent is listening and --wait or --follow was given (E_AGENT_DOWN, §7.3)

Without --wait or --follow the run is enqueued directly in sqlite and this
exits 0 even when no agent is running; it starts when one does. With either
flag there is nothing to wait for, so the answer is exit 3 rather than a
process that blocks forever.

Examples:
  latem run myapp test
  latem run myapp ios-release --ref release/1.4 --input channel=beta --follow`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsed, err := parseInputs(inputs)
			if err != nil {
				return err
			}
			_ = parsed

			// TODO(WS-10/WS-11): build a protocol.RunRequest, submit it
			// through internal/broker/local, and stream updates from
			// internal/run. Argument parsing and validation are done here;
			// everything after admission belongs to those workstreams.
			return errNotWired(
				fmt.Sprintf("latem run %s %s", args[0], args[1]),
				"WS-10 (internal/run) and WS-11 (internal/broker/local)")
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag, or fully-qualified ref (default: the repo's default branch)")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "job input as key=value; repeatable")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream logs until the run finishes")
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the run finishes")
	cmd.Flags().StringVar(&actor, "actor", "", "who to record as having requested this (default: the current user)")
	return cmd
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
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok {
			return nil, usageErrorf("--input %q is not key=value", p)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, usageErrorf("--input %q has an empty key", p)
		}
		if _, dup := out[key]; dup {
			return nil, usageErrorf("--input %s given more than once", key)
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
