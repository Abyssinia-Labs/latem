//go:build !darwin && !(linux && (amd64 || arm64))

package main

import (
	"os"
	"testing"
)

// openPTY has no implementation where isTTY has none: nothing is a terminal
// there, so `latem approve` always refuses, and no test can type at it.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	t.Skip("no pseudo-terminal on this platform: latem approve refuses here (A89)")
	return nil, nil
}
