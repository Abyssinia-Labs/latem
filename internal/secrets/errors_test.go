package secrets

import (
	"errors"
	"strings"
	"testing"
)

// Secret names become environment variable keys and argv elements. The class
// is re-checked here even though internal/registry already applied it, for the
// reason RUNNER_SPEC §5 gives about run ids: one validation point that
// everything downstream trusts is the shape of the next incident.
func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"conventional", "MATCH_PASSWORD", true},
		{"leading underscore", "_PRIVATE", true},
		{"digits after the first character", "KEY_2", true},
		{"lower case", "match_password", true},
		{"empty", "", false},
		// Would stop being data and become an option to `security`.
		{"an option", "-w", false},
		{"leading digit", "2KEY", false},
		{"a dash inside", "MATCH-PASSWORD", false},
		{"a dot inside", "MATCH.PASSWORD", false},
		{"a slash inside", "../../etc/passwd", false},
		{"a space inside", "MATCH PASSWORD", false},
		{"a newline inside", "MATCH\nPASSWORD", false},
		{"a null byte inside", "MATCH\x00PASSWORD", false},
		{"a dollar inside", "$MATCH", false},
		// A zero-width space is invisible in a config file and would otherwise
		// make two secret names that look identical resolve differently.
		{"invisible unicode", "MATCH_PASSWORD\u200b", false},
		{"past the length bound", strings.Repeat("A", MaxNameLength+1), false},
		{"at the length bound", strings.Repeat("A", MaxNameLength), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validName(tc.in)
			if (err == nil) != tc.ok {
				t.Errorf("validName(%q) = %v, want ok = %v", tc.in, err, tc.ok)
			}
		})
	}
}

// Both helpers classify errors that may not be CommandErrors at all, because
// a Runner is an interface anyone can implement.
func TestCommandErrorHelpers_ToleratePlainErrors(t *testing.T) {
	plain := errors.New("something else")
	if got := exitCodeOf(plain); got != -1 {
		t.Errorf("exitCodeOf = %d, want -1", got)
	}
	if notInstalled(plain) {
		t.Error("notInstalled = true for a plain error")
	}
}
