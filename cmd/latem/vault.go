package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// `latem vault …` is PinkVault's own command, run with the same arguments
// (A110, A113), as `git foo` runs `git-foo`. The vault is a program of its
// own, pinkvault, released apart from latem; latem finds it where A104 looks
// for it, under its old names latem-vault and openvault too, and hands it the
// terminal, the arguments and the environment as they are. What it prints
// and the status it exits with are its own.

// vaultMissing is what is said when there is no vault to run, with the
// formula that installs it here.
func vaultMissing(formula string) string {
	return "PinkVault is not installed here; install it with " + readiness.VaultInstallLine(formula) + "."
}

// vaultExec runs the vault's command at path with args and environ, in place
// of this process where it can. A test replaces it.
var vaultExec = execVault

// vaultSearchDirs are where the vault's command is looked for (A104). A test
// replaces it, so that what the machine running it has installed is not
// found.
var vaultSearchDirs = secrets.VaultSearchDirs

// vaultBrew is the brew command where the vault is looked for, "" when there
// is none. A test replaces it.
var vaultBrew = func(environ []string) string {
	for _, d := range vaultSearchDirs(environ) {
		p := filepath.Join(d, "brew")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// vaultFormulaFor is [vaultFormula], which reads Homebrew's tap. A test
// replaces it, so that what the machine running it has tapped is not read.
var vaultFormulaFor = vaultFormula

func newVaultCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "vault [args...]",
		Short: "Run PinkVault, the vault that holds this machine's secrets",
		Long: `Run PinkVault with these arguments: ` + "`latem vault collections`" + ` is
` + "`pinkvault collections`" + ` (or ` + "`pink collections`" + `). latem looks for pinkvault,
then its old names latem-vault and openvault, on PATH, in cargo's bin and in
Homebrew's.

Install it with:
  brew trust --formula ` + readiness.VaultFormula + `
  brew install ` + readiness.VaultFormula + `

Until PinkVault 0.7.0 is in the tap, the formula is
` + readiness.LatemVaultFormula + `, and latem setup installs that one.`,
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
		return &ExitError{Code: ExitFailure, Err: errors.New(vaultMissing(vaultFormulaFor(vaultBrew(environ))))}
	}
	return vaultExec(path, args, environ)
}

// vaultFormula is the formula that installs PinkVault with the Homebrew whose
// brew command is brew (A113): pinkvault when Abyssinia Labs' tap here has
// it, else latem-vault, which installs it both before PinkVault 0.7.0 and,
// as its alias, for one release after. It reads the tap's checkout and runs
// nothing, so it neither taps nor updates.
func vaultFormula(brew string) string {
	if brew == "" {
		return readiness.LatemVaultFormula
	}
	if real, err := filepath.EvalSymlinks(brew); err == nil {
		brew = real
	}
	// brew is <repository>/bin/brew; the prefix's bin/brew links to it
	// where the two differ, as on Intel macOS and Linux.
	tap := filepath.Join(filepath.Dir(filepath.Dir(brew)), "Library", "Taps", "abyssinia-labs", "homebrew-tap")
	for _, f := range []string{filepath.Join(tap, "Formula", "pinkvault.rb"), filepath.Join(tap, "pinkvault.rb")} {
		if _, err := os.Stat(f); err == nil {
			return readiness.VaultFormula
		}
	}
	return readiness.LatemVaultFormula
}
