package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// `latem vault …` is latem vault's own command, run with the same arguments
// (A110), as `git foo` runs `git-foo`. The vault is a program of its own,
// latem-vault, released apart from latem; latem finds it where A104 looks for
// it and hands it the terminal, the arguments and the environment as they
// are. What it prints and the status it exits with are its own.

// vaultMissing is said when there is no vault to run.
var vaultMissing = "latem vault is not installed here; install it with " + readiness.VaultInstallLine + "."

// vaultExec runs the vault's command at path with args and environ, in place
// of this process where it can. A test replaces it.
var vaultExec = execVault

// vaultSearchDirs are where the vault's command is looked for (A104). A test
// replaces it, so that what the machine running it has installed is not
// found.
var vaultSearchDirs = secrets.VaultSearchDirs

func newVaultCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "vault [args...]",
		Short: "Run latem vault, the vault that holds this machine's secrets",
		Long: `Run latem vault with these arguments: ` + "`latem vault collections`" + ` is
` + "`latem-vault collections`" + `. latem looks for latem-vault, then openvault, on
PATH, in cargo's bin and in Homebrew's.

Install it with:
  brew trust --formula ` + readiness.VaultFormula + `
  brew install ` + readiness.VaultFormula,
		// Every argument is the vault's, --help and --version included.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVault(args, os.Environ())
		},
	}
}

// runVault finds the vault's command for environ and runs it with args.
func runVault(args, environ []string) error {
	_, path, ok := secrets.FindVaultCommand(vaultSearchDirs(environ))
	if !ok {
		return &ExitError{Code: ExitFailure, Err: errors.New(vaultMissing)}
	}
	return vaultExec(path, args, environ)
}
