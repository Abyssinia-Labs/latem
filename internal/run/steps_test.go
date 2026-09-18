package run

import (
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// TestStepEnding: a killed step used to close with "finished in 3.854s".
//
// In a long job that line is the one an operator reads to find where the run
// stopped, and it said the opposite of what happened — the run-level line
// reported "cancelled" while the step above it claimed to have finished.
func TestStepEnding(t *testing.T) {
	const d = 1500 * time.Millisecond

	tests := []struct {
		name  string
		state protocol.RunState
		want  string
	}{
		{"a step that succeeded", protocol.StateSucceeded, "finished in 1.5s"},
		// It did finish: the process ran to completion and chose its status.
		{"a step that exited non-zero", protocol.StateFailed, "finished in 1.5s"},
		{"a step killed by cancel", protocol.StateCancelled, "cancelled after 1.5s"},
		{"a step killed by a timeout", protocol.StateTimedOut, "timed out after 1.5s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stepEnding(outcome{state: tc.state}, d)
			if got != tc.want {
				t.Errorf("stepEnding(%s) = %q, want %q", tc.state, got, tc.want)
			}
			if tc.state != protocol.StateSucceeded && tc.state != protocol.StateFailed &&
				strings.Contains(got, "finished") {
				t.Errorf("a killed step still reads as finished: %q", got)
			}
		})
	}
}
