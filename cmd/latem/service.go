package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/agent"
	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/system"
)

func newServiceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install, remove, and inspect the agent service",
		Long: `Install, remove, and inspect the agent service.

On Linux this is a systemd user unit at ~/.config/systemd/user/latem.service,
plus ` + "`loginctl enable-linger`" + ` so it survives logout. On macOS it is a
LaunchAgent at ~/Library/LaunchAgents/com.latem.agent.plist.

None of it needs root, and that is a design constraint rather than a
convenience. RUNNER_SPEC §23 requires a LaunchAgent specifically: codesign
needs a real user session with keychain access, and a LaunchDaemon has none,
so signing fails there with errors that point nowhere useful.

Three things have to be true for a macOS machine to come back on its own after
a reboot, and ` + "`latem doctor`" + ` checks all three: this is a LaunchAgent, auto-login
is enabled, and FileVault is either off or the operator knows what it costs.`,
		Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(newServiceInstallCommand(), newServiceUninstallCommand(),
		newServiceRestartCommand(), newServiceReloadCommand(), newServiceStatusCommand())
	return cmd
}

func newServiceInstallCommand() *cobra.Command {
	var (
		dryRun     bool
		skipLinger bool
		skipStart  bool
		execPath   string
		print      bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the agent as a user service and start it",
		Long: `Install the agent as a user service and start it.

Writes the unit or plist, registers it with the service manager, and starts
the agent. Prints every command it runs, so a partial failure can be finished
by hand — service management is the part of installation most likely to
half-succeed.

--print renders the service definition to stdout and does nothing else, for
review or for a configuration management tool.
--dry-run lists what would happen without doing any of it.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sys := system.New(system.Options{})
			spec, err := serviceSpec(sys, execPath)
			if err != nil {
				return err
			}

			if print {
				_, content, err := sys.Render(spec)
				if err != nil {
					return err
				}
				_, err = io.WriteString(cmd.OutOrStdout(), content)
				return err
			}

			rep, err := sys.InstallService(cmd.Context(), spec, system.InstallOptions{
				DryRun:     dryRun,
				SkipLinger: skipLinger,
				SkipStart:  skipStart,
			})
			printServiceReport(cmd.OutOrStdout(), rep, dryRun)
			if err != nil {
				return err
			}
			if !dryRun {
				printf(cmd.OutOrStdout(), "\nnext: latem doctor\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without doing it")
	cmd.Flags().BoolVar(&print, "print", false, "render the unit or plist to stdout and exit")
	cmd.Flags().BoolVar(&skipLinger, "skip-linger", false, "do not run `loginctl enable-linger` (Linux)")
	cmd.Flags().BoolVar(&skipStart, "no-start", false, "install without starting the agent")
	cmd.Flags().StringVar(&execPath, "exec-path", "", "path to the latem binary (default: this one)")
	return cmd
}

func newServiceUninstallCommand() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the agent service and remove its definition",
		Long: `Stop the agent service and remove its definition.

Nothing else is touched: the config directory, the caches, the mirrors, the
sqlite database, and the logs all stay. Reinstalling picks them all up again,
and a command that reclaimed 90 GB of Tart image as a side effect of removing
a plist would be a bad surprise.

On Linux, linger is left enabled — it is a property of the user account rather
than of this service, and turning it off could stop somebody else's unit.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sys := system.New(system.Options{})
			rep, err := sys.UninstallService(cmd.Context(), system.InstallOptions{DryRun: dryRun})
			printServiceReport(cmd.OutOrStdout(), rep, dryRun)
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without doing it")
	return cmd
}

func newServiceRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the agent so it re-reads config.toml",
		Long: `Restart the agent so it re-reads config.toml.

The agent loads its configuration once, when it starts, because a bad config
has to be fatal at load rather than forty minutes into somebody's build (§8).
The cost is that editing config.toml does nothing until the agent restarts —
adding a repo by hand and then running it fails with E_UNKNOWN_REPO, on a
machine where "latem doctor" has already read the file and called it good.

This is the command for that. It restarts the running service and nothing
else: the plist or unit on disk is untouched, so it cannot repair a broken
installation. Use "latem service install" for that.

In-flight runs are not drained first — this is a restart, not a shutdown.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sys := system.New(system.Options{})
			rep, err := sys.RestartService(cmd.Context())
			printServiceReport(cmd.OutOrStdout(), rep, false)
			return err
		},
	}
}

func newServiceReloadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Make the running agent re-read config.toml, without a restart",
		Long: `Make the running agent re-read config.toml, without a restart.

Applies [repos.*] — with each repo's allowed_refs, vars and environments —
[label_aliases], [actions], and agent.log_retention to the next run, plan and
heartbeat. Runs already admitted keep what they were admitted under, and
nothing in flight is interrupted.

Everything else the agent consumed when it started — [[runner]], [machine] id,
[broker.*], [secrets], and the other [agent] knobs — still needs
"latem service restart", and this command lists each such change it saw.

The file is checked here first. One that does not load is not sent at all:
the agent would refuse it and keep the config it has, and the problems are
easier to read here than in its log.

This is SIGHUP, delivered to the process holding the config directory's lock
and answered through ~/.latem/agent.reload. An agent too old to answer is not
signalled, because SIGHUP stops one of those instead of reloading it.

Exit codes:
  0  reloaded (any change that needs a restart is listed)
  1  the config does not load, the agent refused it, or it cannot reload
  3  no agent is running against this config directory`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("%w\n\nnothing was sent: the agent would refuse this file and keep the config it has", err)
			}
			return reloadAgent(cmd.OutOrStdout(), cfg.Paths)
		},
	}
}

// signalReload is agent.SignalReload, as a seam: a test cannot hand a SIGHUP
// to an agent it did not start, and must never deliver one to the machine's
// real agent.
var signalReload = agent.SignalReload

// reloadAgent asks the running agent to reload and prints its answer.
func reloadAgent(w io.Writer, paths config.Paths) error {
	st, err := signalReload(paths, agent.DefaultReloadWait)
	switch {
	case errors.Is(err, agent.ErrNoAgent):
		return &ExitError{Code: ExitAgentDown, Err: fmt.Errorf(
			"%w (%s)\n\nthe next agent to start reads config.toml as it is now; start one with:\n  latem service install",
			err, paths.Dir)}
	case errors.Is(err, agent.ErrCannotReload):
		return fmt.Errorf("%w — it predates `latem service reload`, and SIGHUP would stop it.\n"+
			"apply the change with a restart instead:\n  latem service restart", err)
	case err != nil:
		return err
	case st.Error != "":
		return fmt.Errorf("the agent refused the reload and kept the config it had:\n%s", st.Error)
	}
	printReloaded(w, st)
	return nil
}

// printReloaded is the answer to a reload that was applied.
func printReloaded(w io.Writer, st agent.ReloadStatus) {
	printf(w, "the agent (pid %d) reloaded config.toml\n", st.PID)
	if len(st.Deferred) > 0 {
		printf(w, "\nread but not applied — these need `latem service restart`:\n")
		for _, d := range st.Deferred {
			printf(w, "  - %s\n", d)
		}
	}
}

// announceConfigChange tells a running agent about an edit a command has just
// made to config.toml, and says what came of it.
//
// It never fails the command. The edit is on disk and is what the next agent
// reads; whether the one running now could take it is something to report, not
// a reason to call the edit a failure.
func announceConfigChange(w io.Writer, paths config.Paths) {
	st, err := signalReload(paths, agent.DefaultReloadWait)
	switch {
	case errors.Is(err, agent.ErrNoAgent):
		printf(w, "no agent is running; the next one to start reads this\n")
	case errors.Is(err, agent.ErrCannotReload):
		printf(w, "the running agent predates `latem service reload`; apply this with:\n  latem service restart\n")
	case err != nil:
		printf(w, "the running agent was not reloaded: %v\napply this with:\n  latem service restart\n", err)
	case st.Error != "":
		printf(w, "the running agent refused the reload and kept the config it had:\n%s\n", st.Error)
	default:
		printReloaded(w, st)
	}
}

// runningWord reports the three states that matter, rather than folding the
// middle one into "no".
func runningWord(st system.ServiceState) string {
	switch {
	case st.Running:
		return "yes"
	case st.Starting:
		return "starting"
	default:
		return "no"
	}
}

func newServiceStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the agent service is installed and running",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sys := system.New(system.Options{})
			st, err := sys.ServiceStatus(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printf(out, "definition  %s\n", st.Path)
			printf(out, "installed   %s\n", yesNo(st.Installed))
			printf(out, "loaded      %s\n", yesNo(st.Loaded))
			printf(out, "running     %s\n", runningWord(st))
			if st.Detail != "" {
				printf(out, "detail      %s\n", st.Detail)
			}
			if !st.Installed {
				printf(out, "\ninstall it with:\n  latem service install\n")
				return &ExitError{Code: ExitFailure, Err: fmt.Errorf("the agent service is not installed")}
			}
			// Starting is not a failure. `latem service install` returns as
			// soon as the service manager accepts the job, so a status run
			// immediately afterwards catches launchd part-way through exec —
			// and reporting that as "installed but not running", with exit 1,
			// sent an operator to debug an agent that was seconds from ready.
			if !st.Running && !st.Starting {
				return &ExitError{Code: ExitFailure, Err: fmt.Errorf("the agent service is installed but not running")}
			}
			return nil
		},
	}
}

// serviceSpec builds the service definition from this machine and this config.
func serviceSpec(sys *system.System, execPath string) (system.ServiceSpec, error) {
	if execPath == "" {
		// The running binary, resolved through symlinks. Installing a service
		// that points at a symlink an operator later replaces is how a machine
		// silently keeps running an old agent.
		exe, err := os.Executable()
		if err != nil {
			return system.ServiceSpec{}, fmt.Errorf("locating this binary: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		execPath = exe
	}
	abs, err := filepath.Abs(execPath)
	if err != nil {
		return system.ServiceSpec{}, err
	}

	spec := system.ServiceSpec{
		ExecPath: abs,
		Home:     sys.Home(),
		User:     sys.User(),
	}
	// Only pass the config directory through when it is not the default. A
	// unit carrying an explicit LATEM_CONFIG_DIR that happens to equal the
	// default is a unit that stops working when somebody's home directory
	// moves.
	if dir := configDir(); dir != filepath.Join(sys.Home(), config.DirName) {
		spec.ConfigDir = dir
	}
	// The service's stdout and stderr must follow the config directory too.
	//
	// Without this the plist carried LATEM_CONFIG_DIR=<relocated> while pointing
	// StandardOutPath at ~/.latem/logs — two different trees in one file. §8
	// says --config-dir relocates the lock, socket, sqlite, work dir and caches
	// *together*, and the service definition was the one place that did not.
	//
	// DerivePaths rather than a loaded config on purpose (amendment A26): this
	// has to work on the machine whose config does not parse yet, which is
	// every machine at first install.
	spec.LogDir = config.DerivePaths(configDir()).Logs
	// The agent's own shutdown grace plus a margin, when a config exists to
	// read it from. A service manager that kills the agent partway through
	// tearing down a Tart VM leaks 90 GB.
	if cfg, err := loadConfig(); err == nil {
		spec.StopTimeout = system.StopTimeoutFor(cfg.Agent.ShutdownGrace.Duration())
	}
	return spec, nil
}

func printServiceReport(w io.Writer, rep system.ServiceReport, dryRun bool) {
	if dryRun {
		printf(w, "dry run — nothing was written or started\n\n")
	}
	if rep.Path != "" {
		printf(w, "service definition: %s\n\n", rep.Path)
	}
	for _, st := range rep.Steps {
		marker := "ok  "
		switch {
		case dryRun:
			marker = "    "
		case st.Err != nil && st.Optional:
			marker = "skip"
		case st.Err != nil:
			marker = "FAIL"
		}
		printf(w, "  [%s] %s\n", marker, st.Desc)
		if len(st.Argv) > 0 {
			printf(w, "         %s\n", st.String())
		}
		if st.Err != nil && !st.Optional && st.Output != "" {
			printf(w, "         %s\n", st.Output)
		}
	}
	for _, n := range rep.Notes {
		printf(w, "\nnote: %s\n", n)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		printf(w, "\nno service integration on %s; run `latem agent` under your own supervisor\n", runtime.GOOS)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
