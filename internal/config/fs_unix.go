//go:build unix

package config

import (
	"os"
	"syscall"
)

// sameFilesystem reports whether two existing directories share a device.
//
// ok is false when the comparison could not be made — either directory
// missing, or a FileInfo whose Sys() is not a stat structure. A check that
// cannot be performed must not become a startup failure: on a fresh install
// neither directory exists yet, and both will be created adjacent under the
// config directory anyway.
func sameFilesystem(a, b string) (same, ok bool, err error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, false, nil //nolint:nilerr // a missing directory is "cannot compare", not a failure
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, false, nil //nolint:nilerr // as above
	}
	sa, aok := fa.Sys().(*syscall.Stat_t)
	sb, bok := fb.Sys().(*syscall.Stat_t)
	if !aok || !bok {
		return false, false, nil
	}
	return sa.Dev == sb.Dev, true, nil
}
