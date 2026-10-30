//go:build !unix

package agent

import (
	"errors"
	"os"
)

// sendReload refuses where there is no SIGHUP. The agent cannot run here
// either; see lockExclusive.
func sendReload(int) error {
	return errors.New("agent: reloading needs SIGHUP, which this platform does not provide")
}

// sendSwitch refuses where there is no SIGUSR2, for the same reason.
func sendSwitch(int) error {
	return errors.New("agent: moving onto a new binary needs SIGUSR2, which this platform does not provide")
}

// SwitchSignal reports that there is no signal to treat as [Agent.Switch].
func SwitchSignal() (os.Signal, bool) { return nil, false }
