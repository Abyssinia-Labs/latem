package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func newVersionCommand() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the agent version",
		Long: `Print the agent version, the protocol version, and the build it came from.

The protocol version matters when a control plane refuses a connection.
Inbound decoding is strict (RUNNER_SPEC §7, amendment A4), so even an additive
change on the far side reaches an older agent as a decode failure rather than
as silence — and the first question is which two versions are talking.

The release key line is the answer to the other version question: which key
this binary will accept an update from (§20). A build with none says so, and
that is also why it refuses agent.auto_update at load.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if short {
				_, err := fmt.Fprintln(out, version)
				return err
			}
			printf(out, "latem %s\n", version)
			printf(out, "protocol %s\n", protocol.Version)
			printf(out, "go %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			// Which key this binary trusts, in the form minisign prints an
			// id. It is what a key rotation is diagnosed with: an agent that
			// has stopped updating is either on the old key or on the new one,
			// and nothing else in the output distinguishes those.
			if key, err := selfupdate.TrustedKey(); err == nil {
				printf(out, "release key %s\n", key.ID())
			} else {
				printf(out, "release key none (self-update unavailable)\n")
			}
			if rev, dirty, ok := buildRevision(); ok {
				suffix := ""
				if dirty {
					// A dirty build is worth stating: half the confusing bug
					// reports about a self-hosted runner come from a binary
					// nobody can reproduce.
					suffix = " (dirty)"
				}
				printf(out, "revision %s%s\n", rev, suffix)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print just the version string")
	return cmd
}

// buildRevision reads the VCS stamp the Go toolchain embeds.
func buildRevision() (rev string, dirty, ok bool) {
	info, available := debug.ReadBuildInfo()
	if !available {
		return "", false, false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev, ok = s.Value, true
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return rev, dirty, ok
}
