package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

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
as silence — and the first question is which two versions are talking.`,
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
