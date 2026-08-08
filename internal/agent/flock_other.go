//go:build !unix

package agent

import (
	"errors"
	"os"
)

// errWouldBlock exists so lock.go compiles everywhere. Nothing returns it
// here.
var errWouldBlock = errors.New("agent: lock is held")

// lockExclusive refuses on a platform with no flock.
//
// Failing is the honest answer rather than a no-op. The lock is what makes
// "exactly one agent per config directory" true, and an agent that pretended
// to hold one would let two processes share a work tree, a socket, and a
// queue — which is the corruption §6 step 2 exists to prevent, arriving
// silently. RUNNER_SPEC targets macOS and Linux; this file is here so the tree
// still builds elsewhere, not so it runs there.
func lockExclusive(*os.File) error {
	return errors.New("agent: the agent lock needs flock, which this platform does not provide")
}

func unlock(*os.File) error { return nil }
