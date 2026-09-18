package secrets

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestKeychainProvider_Resolve(t *testing.T) {
	runner := answeringStderr(`password: "hunter2"` + "\n")
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

	// -g rather than -w: only -g marks a hex-encoded value as such.
	want := []string{"security", "find-generic-password", "-s", "latem", "-a", "MATCH_PASSWORD", "-g"}
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

// `security -w` prints a value containing any non-printable byte as bare hex
// with nothing marking it, so a PEM read back through -w is indistinguishable
// from a secret whose literal value is that hex string. -g marks it `0x`, and
// these are the forms this provider has to read.
func TestKeychainProvider_DecodesWhatSecurityActuallyPrints(t *testing.T) {
	// The bytes of pemFixture, as `security -g` renders them.
	pemHex := strings.ToUpper(hex.EncodeToString([]byte(pemFixture)))

	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			"a plain value comes back quoted",
			`password: "hunter2"` + "\n",
			"hunter2",
		},
		{
			// The regression: a PEM in the keychain used to resolve to the
			// literal string "2d2d2d2d2d424547494e…" and reach the build as a
			// key that is not a key.
			"a multi-line PEM comes back as 0x hex",
			"password: 0x" + pemHex + `  "…"` + "\n",
			pemFixture,
		},
		{
			// The reason -w cannot be salvaged: this value and the four bytes
			// DE AD BE EF print identically under -w.
			"a value that is literally hex digits is not decoded",
			`password: "deadbeef"` + "\n",
			"deadbeef",
		},
		{
			// security does not escape an embedded quote, so unescaping would
			// be wrong; stripping exactly one from each end is right.
			"embedded quotes survive",
			`password: "he said "hi" to me"` + "\n",
			`he said "hi" to me`,
		},
		{
			// A trailing newline is part of the value and OpenSSL cares.
			"a trailing newline is preserved",
			"password: 0x" + strings.ToUpper(hex.EncodeToString([]byte("abc\n"))) + `  "abc\012"` + "\n",
			"abc\n",
		},
		{
			"the password line need not be the only line",
			"keychain: \"login\"\n" + `password: "hunter2"` + "\n",
			"hunter2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKeychainProvider("", answeringStderr(tc.stderr))
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

// A value that cannot be read is an error, never a silently wrong secret.
func TestKeychainProvider_RefusesOutputItCannotParse(t *testing.T) {
	for _, stderr := range []string{
		"",
		"keychain: \"login\"\n",
		"password: 0xZZZZ  \"…\"\n",
		"password: unquoted\n",
	} {
		p, err := NewKeychainProvider("", answeringStderr(stderr))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := p.Resolve(t.Context(), "A"); err == nil {
			t.Errorf("stderr %q resolved to %q, want an error", stderr, got)
		}
	}
}

func TestNewKeychainProvider_RejectsAServiceThatWouldBecomeAnOption(t *testing.T) {
	for _, service := range []string{"-w", "a b", "", "with/slash", strings.Repeat("x", 100)} {
		if service == "" {
			continue // empty means "use the default"
		}
		if _, err := NewKeychainProvider(service, answeringStderr("")); err == nil {
			t.Errorf("service %q was accepted", service)
		}
	}
}

func TestKeychainProvider_RejectsABadName(t *testing.T) {
	p, err := NewKeychainProvider("", answeringStderr(`password: "x"`))
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
