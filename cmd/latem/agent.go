package main

import (
	"github.com/spf13/cobra"
)

func newAgentCommand() *cobra.Command {
	var (
		once     bool
		drainFor string
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the agent daemon in the foreground",
		Long: `Run the agent daemon in the foreground.

This is the process a service wrapper starts. Install it as one with
` + "`latem service install`" + `, which writes a systemd user unit on Linux or a
LaunchAgent on macOS — never a LaunchDaemon, because codesign needs a real
user session with keychain access and a daemon context fails with errors that
point nowhere useful (RUNNER_SPEC §23).

Startup order (§6):
  1. load config.toml and fail fast on anything invalid
  2. take the flock on the config directory — exactly one agent per directory
  3. open sqlite and run migrations
  4. preflight every runner: docker reachable, tart present, disk headroom
  5. reconcile: mark crashed runs interrupted, reap orphaned containers,
     VMs, and workspaces, sweep expired queued and awaiting_approval runs
  6. start the runner pool, the brokers, and the heartbeat

On SIGTERM the agent stops accepting runs, reports itself draining, and lets
in-flight runs finish for up to agent.shutdown_grace before signalling their
process groups. Containers and VMs are torn down even on the kill path — a
leaked Tart VM carrying an Xcode image costs about 90 GB.

SIGHUP reloads the reloadable half of the config. Changes to [[runner]],
machine.id, agent.work_dir, agent.metrics_addr, and broker.local.socket need a
restart, and the agent says which ones it read but did not apply.`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// TODO(WS-11): call agent.Run(ctx, cfg) once internal/agent exists.
			// This command owns only the flags, the help, and the exit codes;
			// the lifecycle above belongs to WS-11, which also owns the local
			// broker the flags below feed.
			return errNotWired("latem agent", "WS-11 (internal/agent, internal/broker/local)")
		},
	}
	cmd.Flags().BoolVar(&once, "once", false,
		"execute a single queued run and exit — for debugging a machine, not for production")
	cmd.Flags().StringVar(&drainFor, "drain-after", "",
		"stop accepting new runs after this duration and exit when they finish (e.g. 8h)")
	return cmd
}
