//go:build !unix

package config

// sameFilesystem has no portable implementation. RUNNER_SPEC targets Linux and
// macOS (§13), and `latem doctor` reports the same condition on any platform
// where it can be measured, so reporting "cannot compare" here loses nothing
// the agent depends on.
func sameFilesystem(_, _ string) (same, ok bool, err error) {
	return false, false, nil
}
