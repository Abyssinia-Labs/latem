package main

import (
	"github.com/spf13/cobra"
)

func newRegisterCommand() *cobra.Command {
	var (
		url       string
		token     string
		tokenFile string
		name      string
		labels    []string
	)
	cmd := &cobra.Command{
		Use:   "register --url <wss://…> --token <token>",
		Short: "Register this machine with a control plane",
		Long: `Register this machine with a control plane.

Writes [broker.remote] into config.toml, stores the machine token in
~/.latem/agent.key at mode 0600, verifies connectivity, and records the
machine id the control plane assigns.

The token goes in its own file and never inline in config.toml, because
config.toml is the file that gets pasted into an issue when something breaks
(RUNNER_SPEC §8).

--token reads the value from the command line, where it lands in shell
history and in the process table of every other user on the machine. Prefer
--token-file, or pipe it in: --token-file - reads stdin.

A machine that never registers is a fully supported state: the local broker
works without a control plane (§7.3), which is what makes ` + "`latem run`" + ` useful
on a laptop.

Exit codes:
  0  registered and the connection verified
  1  the control plane refused, or the token or config could not be written
  2  usage`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case url == "":
				return usageErrorf("--url is required")
			case token == "" && tokenFile == "":
				return usageErrorf("one of --token or --token-file is required")
			case token != "" && tokenFile != "":
				return usageErrorf("--token and --token-file are mutually exclusive")
			}
			// TODO(WS-14): perform the registration handshake through
			// internal/broker/remote, which owns the WebSocket transport and
			// the token's use. Writing config.toml is this command's job and
			// waits on that call's answer, since a config naming a control
			// plane that rejected us is worse than no config at all.
			return errNotWired("latem register", "WS-14 (internal/broker/remote)")
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "control plane agent endpoint, e.g. wss://api.example.com/agent")
	cmd.Flags().StringVar(&token, "token", "", "machine token (visible in shell history; prefer --token-file)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "read the machine token from this file, or - for stdin")
	cmd.Flags().StringVar(&name, "name", "", "display name for this machine (default: the hostname)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "machine label; repeatable")
	return cmd
}
