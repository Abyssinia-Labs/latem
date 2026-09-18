package system

import (
	"strings"
	"testing"
)

// TestLaunchdDetail: `launchctl print`'s first line is the opening of a brace
// block, so `latem service status` ended with "detail  gui/501/com.latem.agent
// = {" — a dangling brace and no content, on the last line of the command the
// README tells an operator to trust before building (F7).
func TestLaunchdDetail(t *testing.T) {
	const running = "gui/501/com.latem.agent = {\n" +
		"\tactive count = 1\n" +
		"\tstate = running\n" +
		"\tpid = 4242\n" +
		"\tprogram = /Users/me/.local/bin/latem\n" +
		"}\n"
	const stopped = "gui/501/com.latem.agent = {\n" +
		"\tstate = not running\n" +
		"\tlast exit code = 2\n" +
		"}\n"

	tests := []struct {
		name, in, want string
	}{
		{"a live agent names its pid", running, "state = running, pid 4242"},
		{"a stopped one names its last exit", stopped, "state = not running, last exit code 2"},
		{"an unrecognised format falls back rather than going blank", "something else\n", "something else"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := launchdDetail(tc.in)
			if got != tc.want {
				t.Errorf("launchdDetail = %q, want %q", got, tc.want)
			}
			if strings.HasSuffix(got, "{") {
				t.Errorf("the detail is a dangling brace again: %q", got)
			}
		})
	}
}
