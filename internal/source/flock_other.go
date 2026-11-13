//go:build !darwin && !linux

package source

import (
	"fmt"
	"os"
	"runtime"
)

// The agent targets Linux and macOS (RUNNER_SPEC §3). This stub keeps the
// package compiling elsewhere, and a warm workspace refused there.
func tryFlock(*os.File) (bool, error) {
	return false, fmt.Errorf("source: file locking is unsupported on %s", runtime.GOOS)
}
