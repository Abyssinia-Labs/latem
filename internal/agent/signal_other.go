//go:build !unix

package agent

import "errors"

// sendReload refuses where there is no SIGHUP. The agent cannot run here
// either; see lockExclusive.
func sendReload(int) error {
	return errors.New("agent: reloading needs SIGHUP, which this platform does not provide")
}
