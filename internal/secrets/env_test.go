package secrets

import (
	"errors"
	"testing"
)

func TestEnvProvider_Resolve(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"LATEM_SECRET_MATCH_PASSWORD=hunter2",
		"LATEM_SECRET_EMPTY=",
		"LATEM_SECRET_WITH_EQUALS=a=b=c",
		"LATEM_SECRET_-BAD=ignored",
		"LATEM_SECRET_",
		"MATCH_PASSWORD=unprefixed",
	}
	p := NewEnvProvider(environ)

	tests := []struct {
		name    string
		secret  string
		want    string
		wantErr error
	}{
		{"a set variable", "MATCH_PASSWORD", "hunter2", nil},
		// Set-but-empty is a hit. Falling through would mean an operator who
		// exported LATEM_SECRET_X= to override a stale keychain entry silently
		// got the keychain entry anyway.
		{"set but empty", "EMPTY", "", nil},
		{"a value containing =", "WITH_EQUALS", "a=b=c", nil},
		{"an unset variable", "ABSENT", "", ErrNotFound},
		// A name that cannot be an env var was never in the snapshot.
		{"a name that is an option", "BAD", "", ErrNotFound},
		{"an unprefixed variable is not a secret", "PATH", "", ErrNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Resolve(t.Context(), tc.secret)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnvProvider_RejectsABadName(t *testing.T) {
	p := NewEnvProvider(nil)
	if _, err := p.Resolve(t.Context(), "-w"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure rather than a miss", err)
	}
	if p.Name() != "env" {
		t.Errorf("Name() = %q", p.Name())
	}
}

// The caller owns what it gets and will zero it, so the snapshot has to survive
// a second resolution of the same name.
func TestEnvProvider_ZeroingAResultDoesNotDamageTheSnapshot(t *testing.T) {
	p := NewEnvProvider([]string{"LATEM_SECRET_A=hunter2"})

	first, err := p.Resolve(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	first.Zero()

	second, err := p.Resolve(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "hunter2" {
		t.Errorf("second resolution = %q; the first one damaged the snapshot", second)
	}
}
