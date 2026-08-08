package secrets

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Value.String is a safety net, not a convenience: a stray %v, a log line
// written in haste, or an error wrapping a Value must not be able to leak a
// signing key. These are the verbs fmt routes through String and GoString.
func TestValue_NeverFormatsAsItself(t *testing.T) {
	const secret = "hunter2-do-not-print"
	v := Value(secret)

	tests := []struct {
		verb string
		want string
	}{
		{"%v", "***"},
		{"%s", "***"},
		{"%q", `"***"`},
		{"%#v", "secrets.Value(***)"},
	}

	for _, tc := range tests {
		t.Run(tc.verb, func(t *testing.T) {
			got := fmt.Sprintf(tc.verb, v)
			if strings.Contains(got, secret) {
				t.Fatalf("%s printed the value: %q", tc.verb, got)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.verb, got, tc.want)
			}
		})
	}
}

// The realistic leak: a value ends up inside an error, and the error is
// formatted somewhere that reaches a broker.
func TestValue_DoesNotLeakThroughAWrappedError(t *testing.T) {
	const secret = "hunter2-do-not-print"
	err := fmt.Errorf("resolving MATCH_PASSWORD: %v: %w", Value(secret), errors.New("boom"))
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err = %q leaks the value", err)
	}
}

// The whole reason Value is []byte and not string (finding F10): a string
// cannot be zeroed, and this can.
func TestValue_ZeroOverwritesInPlace(t *testing.T) {
	v := Value("hunter2")
	alias := v

	v.Zero()

	for i, b := range alias {
		if b != 0 {
			t.Fatalf("byte %d survived Zero: %q", i, alias)
		}
	}
	if len(alias) != len("hunter2") {
		t.Errorf("Zero changed the length to %d; it must overwrite, not reslice", len(alias))
	}
}

func TestValue_ZeroOnNilIsSafe(t *testing.T) {
	var v Value
	v.Zero()
}
