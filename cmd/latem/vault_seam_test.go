package main

import "github.com/Abyssinia-Labs/latem/internal/secrets"

// What latem tells a person to type names the vault's command this machine
// has (A104). A test pins latem-vault, whatever the machine running it has.
func init() { vaultCommand = func() string { return secrets.VaultCommand } }
