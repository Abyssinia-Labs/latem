// Command latem is the Latem runner agent and its command line.
//
// One binary is both the daemon and the client, which is deliberate: the thing
// an operator installs, the thing a service manager starts, and the thing they
// type to diagnose a problem should be the same file at the same version. A
// client that can be a different version from its agent is a support question
// nobody enjoys.
//
// The subcommand set is RUNNER_SPEC §19 and is closed. Exit codes are §7.3 and
// §21; see [ExitOK] and its neighbours.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// version is stamped at build time by the Makefile's -ldflags. "dev" means
// somebody ran `go build` directly, which is worth being able to tell apart
// from a release when a bug report arrives.
var version = "dev"

func main() {
	// os.Exit is called exactly once, here, after run() has returned and every
	// deferred cleanup in it has executed. os.Exit skips defers, and the one
	// that matters is the signal handler's — a Ctrl-C that leaves a probe
	// container behind is exactly the litter the agent's reconciliation exists
	// to avoid.
	os.Exit(run())
}

func run() int {
	// SIGINT and SIGTERM cancel the root context rather than killing the
	// process outright. `latem run --follow` and `latem doctor` both hold
	// external processes open.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCommand()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}

	// Cobra has already printed nothing (SilenceErrors), so this is the only
	// place an error reaches the operator.
	printf(os.Stderr, "latem: %v\n", err)
	if isUsage(err) {
		printf(os.Stderr, "\nRun `latem %s --help` for usage.\n", commandPath(root))
	}
	return exitCode(err)
}

// globalFlags are the options every subcommand honours.
type globalFlags struct {
	// configDir is --config-dir. It relocates the lock, the socket, the
	// sqlite database, the work directory, and the caches together (§8), which
	// is why it is one flag rather than five.
	configDir string
	// noColor suppresses the terminal markers, for a log or a CI transcript.
	noColor bool
}

var global globalFlags

const rootLong = `Latem runs your CI on your own machines — including Xcode and mobile builds.

The broker supplies a repo, a job name, a ref, and inputs. Everything else —
commands, images, caches, timeouts — comes from .latem/jobs.yaml in your own
repository at the resolved commit. See RUNNER_SPEC.md §5 for the invariants
that make that a contract rather than a convention.

Exit codes:
  0  success
  1  failure — a check failed, a run failed, an operation did not complete
  2  usage — an unknown flag, a missing argument, an unknown subcommand
  3  no agent is listening (E_AGENT_DOWN); see ` + "`latem agent`" + `

Start here:
  latem doctor            check this machine and name the fix for anything wrong
  latem service install   run the agent at boot
  latem run <repo> <job>  trigger a build locally`

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "latem",
		Short: "Self-hosted CI runner with first-class mobile builds",
		Long:  rootLong,
		// Errors and usage are printed by main, once, in one format. Cobra's
		// defaults print usage for every error, which buries a real failure
		// under sixty lines of flag documentation.
		SilenceErrors: true,
		SilenceUsage:  true,
		// A bare `latem` should say what to do next rather than fail.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErrorf("unknown command %q", args[0])
			}
			return cmd.Help()
		},
		Version: version,
	}

	root.PersistentFlags().StringVar(&global.configDir, "config-dir", "",
		"config directory (default ~/.latem, or $LATEM_CONFIG_DIR)")
	root.PersistentFlags().BoolVar(&global.noColor, "no-color", false,
		"plain output with no terminal markers")

	// A malformed flag is a usage error, not a generic failure. Without this
	// every typo exits 1 and looks like the command ran and failed.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &ExitError{Code: ExitUsage, Err: err}
	})

	root.AddCommand(
		newAgentCommand(),
		newRunCommand(),
		newListCommand(),
		newLogsCommand(),
		newCancelCommand(),
		newApproveCommand(),
		newRegisterCommand(),
		newDoctorCommand(),
		newCacheCommand(),
		newServiceCommand(),
		newVersionCommand(),
	)
	return root
}

// commandPath finds the deepest command cobra was executing, so the "run
// `latem X --help`" hint names the subcommand rather than the root.
func commandPath(root *cobra.Command) string {
	cmd, _, err := root.Find(os.Args[1:])
	if err != nil || cmd == nil || cmd == root {
		return ""
	}
	return cmd.Name()
}

// exactArgs is cobra.ExactArgs with a usage exit code attached.
//
// Cobra's own validators return a plain error, which would exit 1 — the code
// that means "the command ran and failed". A missing argument never ran
// anything.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("%s takes exactly %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

// rangeArgs bounds the positional argument count.
func rangeArgs(minArgs, maxArgs int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < minArgs || len(args) > maxArgs {
			return usageErrorf("%s takes between %d and %d arguments, got %d",
				cmd.CommandPath(), minArgs, maxArgs, len(args))
		}
		return nil
	}
}

// noArgs rejects positional arguments.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErrorf("%s takes no arguments, got %q", cmd.CommandPath(), args[0])
	}
	return nil
}

// errNotWired is what an unimplemented subcommand returns.
//
// It is a hard refusal rather than a plausible-looking no-op on purpose. A
// command that prints an empty list and exits 0 is indistinguishable from a
// machine with nothing queued, and somebody will build a script on top of that
// answer before the real implementation lands.
func errNotWired(what, workstream string) error {
	return errors.New(what + " is not wired up yet — it needs " + workstream +
		"\n\nthe flags, output shape, and exit codes above are final; the wiring is not")
}
