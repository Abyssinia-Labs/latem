//go:build unix

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// execVault replaces this process with the vault's command, so that its
// stdin, stdout, stderr, signals and exit status are the vault's own. It
// returns only when the exec fails.
func execVault(path string, args, environ []string) error {
	argv := append([]string{filepath.Base(path)}, args...)
	// #nosec G204 -- path is pinkvault, latem-vault or openvault, found where A104 looks.
	if err := syscall.Exec(path, argv, environ); err != nil {
		return fmt.Errorf("could not run %s: %w", path, err)
	}
	return nil
}
