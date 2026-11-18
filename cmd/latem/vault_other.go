//go:build !unix

package main

import "fmt"

// execVault is for macOS and Linux, where latem vault is released.
func execVault(path string, _, _ []string) error {
	return fmt.Errorf("latem vault runs on macOS and Linux; run %s itself here", path)
}
