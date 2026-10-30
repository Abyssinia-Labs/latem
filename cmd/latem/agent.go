package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/system"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
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

SIGHUP — or ` + "`latem service reload`" + `, which sends it and prints the answer —
reloads the reloadable half of the config: [repos.*] with their refs, vars and
environments, [label_aliases], [actions], and log retention. Changes to
[[runner]], [machine] id, [broker.*], [secrets], and the [agent] knobs other
than log_retention need a restart, and the agent says which ones it read but
did not apply. A file that does not load is refused and the agent keeps the
config it had.

SIGUSR2 — sent by ` + "`latem update`" + ` after it replaces the binary — moves the agent
onto the binary at its own path if that is a different version. It stops
taking runs, waits up to agent.shutdown_grace for the ones in flight, and
re-executes in place, keeping its pid. If runs are still going it takes work
again and moves at the first moment nothing is running. It never cancels a run
to do it.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Step 1 of §6, and the reason it is here rather than inside
			// internal/agent: a config that does not parse must fail before a
			// lock is taken, a database is opened, or a socket is bound.
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Before anything else can take long: until Notify is in place a
			// SIGHUP takes Go's default action, which is to exit. The agent
			// advertises that it reloads only once it is ready, and nothing
			// that reads that advertisement signals before then.
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			// SIGUSR2 likewise, for `latem update` (A56). Notify with no
			// signal named would relay every signal, hence the check.
			usr2 := make(chan os.Signal, 1)
			if sig, ok := agent.SwitchSignal(); ok {
				signal.Notify(usr2, sig)
				defer signal.Stop(usr2)
			}

			var drain time.Duration
			if drainFor != "" {
				var d protocol.Duration
				if err := d.UnmarshalText([]byte(drainFor)); err != nil {
					return usageErrorf("--drain-after %q: %v", drainFor, err)
				}
				drain = d.Duration()
			}

			a, err := agent.New(agent.Options{
				Config:     cfg,
				Version:    version,
				Logger:     agentLogger(cmd.ErrOrStderr()),
				Once:       once,
				DrainAfter: drain,
				// The agent's own PATH is the one its steps get (A59).
				ProbeToolchains: func(ctx context.Context) system.Toolchains {
					return system.New(system.Options{}).Toolchains(ctx, os.Getenv("PATH"))
				},
			})
			if err != nil {
				return err
			}
			go reloadOnHangup(cmd.Context(), a, hup)
			go switchOnSignal(cmd.Context(), a, usr2)
			// The context is already wired to SIGINT and SIGTERM by main, so
			// §6 step 9 is a plain block on it. Run returns after draining.
			return a.Run(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&once, "once", false,
		"execute a single queued run and exit — for debugging a machine, not for production")
	cmd.Flags().StringVar(&drainFor, "drain-after", "",
		"stop accepting new runs after this duration and exit when they finish (e.g. 8h)")
	return cmd
}

// reloadOnHangup turns each SIGHUP into a config reload (§8, A54). The agent
// logs the outcome and records it for `latem service reload`; a refused file
// is an operator mid-edit, never a reason to stop.
func reloadOnHangup(ctx context.Context, a *agent.Agent, hup <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_, _ = a.Reload()
		}
	}
}

// switchOnSignal turns each SIGUSR2 into a move onto the binary at the
// agent's own path, once nothing is running (A56). `latem update` sends it
// after replacing that file.
func switchOnSignal(ctx context.Context, a *agent.Agent, usr2 <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-usr2:
			a.Switch(ctx)
		}
	}
}

// agentLogger is the daemon's narration.
//
// Text rather than JSON, on stderr rather than stdout, and at Info by default.
// A service wrapper captures stderr into the journal or into a LaunchAgent log
// file, and a person reading either wants sentences. LATEM_LOG=debug turns the
// quieter lines on — the free-disk probe, the log pipeline — without a config
// change, because the machine you need debug output from is usually one you are
// already ssh'd into.
func agentLogger(w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LATEM_LOG")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
