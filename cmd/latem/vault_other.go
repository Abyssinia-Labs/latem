//go:build !unix

package main

import "fmt"

// execVault is for macOS and Linux, where PinkVault is released.
func execVault(path string, _, _ []string) error {
	return fmt.Errorf("PinkVault runs on macOS and Linux; run %s itself here", path)
}
