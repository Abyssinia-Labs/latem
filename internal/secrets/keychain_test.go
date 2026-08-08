package secrets

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestKeychainProvider_Resolve(t *testing.T) {
	runner := answering("hunter2\n")
	p, err := NewKeychainProvider("", runner)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.Resolve(t.Context(), "MATCH_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hunter2" {
		t.Errorf("value = %q", got)
	}

	// The exact invocation RUNNER_SPEC §15 documents, as an argv slice.
	want := []string{"security", "find-generic-password", "-s", "latem", "-a", "MATCH_PASSWORD", "-w"}
	if call := runner.lastCall(); !slices.Equal(call, want) {
		t.Errorf("ran %q, want %q", call, want)
	}
}

func TestKeychainProvider_Outcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// miss means the chain should move on rather than stop.
		miss bool
	}{
		// The item is simply not on this Mac.
		{"errSecItemNotFound", exited("security", keychainNotFound), true},
		// `security` does not exist, so this is not a Mac.
		{"security is absent", missingBinary("security"), true},
		// A Mac whose keychain will not unlock must say so at admission, not
		// quietly resolve the secret from somewhere the operator did not mean.
		{"keychain is locked", exited("security", 51), false},
		{"security hung on a prompt", timedOut("security"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKeychainProvider("", failing(tc.err))
			if err != nil {
				t.Fatal(err)
			}
			_, resolveErr := p.Resolve(t.Context(), "A")
			if resolveErr == nil {
				t.Fatal("a failing `security` was reported as success")
			}
			if got := errors.Is(resolveErr, ErrNotFound); got != tc.miss {
				t.Errorf("errors.Is(err, ErrNotFound) = %v, want %v (err = %v)", got, tc.miss, resolveErr)
			}
			// Either way the underlying diagnosis survives for `latem doctor`.
			var ce *CommandError
			if !errors.As(resolveErr, &ce) {
				t.Error("the CommandError was dropped, so doctor has nothing to print")
			}
		})
	}
}

// `security -w` prints the password and then a newline, so the last newline is
// the tool's and any before it are the secret's. Trimming greedily would
// silently corrupt a PEM.
func TestKeychainProvider_TrimsExactlyOneTrailingNewline(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"single line", "hunter2\n", "hunter2"},
		{"crlf", "hunter2\r\n", "hunter2"},
		{"no trailing newline at all", "hunter2", "hunter2"},
		{"a value that genuinely ends in a newline", "hunter2\n\n", "hunter2\n"},
		{"a PEM keeps every internal newline", pemFixture + "\n", pemFixture},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKeychainProvider("", answering(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Resolve(t.Context(), "A")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewKeychainProvider_RejectsAServiceThatWouldBecomeAnOption(t *testing.T) {
	for _, service := range []string{"-w", "a b", "", "with/slash", strings.Repeat("x", 100)} {
		if service == "" {
			continue // empty means "use the default"
		}
		if _, err := NewKeychainProvider(service, answering("")); err == nil {
			t.Errorf("service %q was accepted", service)
		}
	}
}

func TestKeychainProvider_RejectsABadName(t *testing.T) {
	p, err := NewKeychainProvider("", answering("x"))
	if err != nil {
		t.Fatal(err)
	}
	// A name beginning with `-` would stop being data and become an option to
	// `security`.
	if _, err := p.Resolve(t.Context(), "-w"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
	if p.Name() != "keychain" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestNewKeychainProvider_DefaultsToTheRealRunner(t *testing.T) {
	p, err := NewKeychainProvider("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.runner.(ExecRunner); !ok {
		t.Errorf("runner = %T, want ExecRunner", p.runner)
	}
	if p.service != KeychainService {
		t.Errorf("service = %q, want %q", p.service, KeychainService)
	}
}
