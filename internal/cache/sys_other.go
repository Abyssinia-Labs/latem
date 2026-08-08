//go:build !darwin && !linux

package cache

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"time"
)

// The agent targets Linux and macOS (RUNNER_SPEC §3). These stubs exist so the
// package still compiles elsewhere — a developer on another platform gets a
// clear refusal from the two operations that genuinely need a syscall, rather
// than a build failure in a package they were only trying to import.

func tryFlock(*os.File) (bool, error) {
	return false, fmt.Errorf("cache: file locking is unsupported on %s", runtime.GOOS)
}

func unflock(*os.File) error {
	return fmt.Errorf("cache: file locking is unsupported on %s", runtime.GOOS)
}

func freeBytes(string) (uint64, error) {
	return 0, fmt.Errorf("cache: free space is unsupported on %s", runtime.GOOS)
}

func accessTime(info fs.FileInfo) time.Time { return info.ModTime() }
