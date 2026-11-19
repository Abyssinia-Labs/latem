package main

import (
	"github.com/Abyssinia-Labs/latem/internal/readiness"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// What latem tells a person to type names the vault's command this machine
// has (A104). A test pins pinkvault, whatever the machine running it has, and
// the formula that installs it from a tap that does not have pinkvault yet
// (A113), whatever this machine's Homebrew has tapped.
func init() {
	vaultCommand = func() string { return secrets.VaultCommand }
	vaultBrew = func([]string) string { return "" }
	vaultFormulaFor = func(string) string { return readiness.LatemVaultFormula }
}
