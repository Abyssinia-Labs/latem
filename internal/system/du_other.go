//go:build !darwin && !linux

package system

import (
	"io/fs"
	"path/filepath"
)

// diskUsage falls back to apparent sizes off the two supported platforms.
//
// The agent targets Linux and macOS (RUNNER_SPEC §3). This exists so the
// package compiles elsewhere for a developer who only wanted to import it.
func diskUsage(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
