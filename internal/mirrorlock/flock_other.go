//go:build !darwin && !linux

package mirrorlock

import "os"

// The agent targets Linux and macOS (RUNNER_SPEC §3). Elsewhere the lock is
// always given, which leaves the agent's own in-process lock as the only one:
// what every platform had before A112.
func tryFlock(*os.File, bool) (bool, error) { return true, nil }
