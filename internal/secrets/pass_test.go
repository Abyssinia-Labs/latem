package secrets

import (
	"errors"
	"slices"
	"testing"
)

func TestPassProvider_Resolve(t *testing.T) {
	tests := []struct {
		name     string
		opts     PassOptions
		wantArgv []string
	}{
		{"default prefix", PassOptions{Binary: "pass"},
			[]string{"pass", "show", "latem/MATCH_PASSWORD"}},
		{"gopass", PassOptions{Binary: "gopass"},
			[]string{"gopass", "show", "latem/MATCH_PASSWORD"}},
		{"custom prefix", PassOptions{Binary: "pass", Prefix: "ci/mac"},
			[]string{"pass", "show", "ci/mac/MATCH_PASSWORD"}},
		{"custom prefix already ending in a slash", PassOptions{Binary: "pass", Prefix: "ci/"},
			[]string{"pass", "show", "ci/MATCH_PASSWORD"}},
		{"store root", PassOptions{Binary: "pass", Prefix: "/"},
			[]string{"pass", "show", "MATCH_PASSWORD"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := answering("hunter2\n")
			p, err := NewPassProvider(tc.opts, runner)
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
			if call := runner.lastCall(); !slices.Equal(call, tc.wantArgv) {
				t.Errorf("ran %q, want %q", call, tc.wantArgv)
			}
		})
	}
}

// A PEM block is nine lines and all nine are the secret; `pass`'s
// first-line-is-the-password convention cannot represent that.
func TestPassProvider_ReadsTheWholeEntry(t *testing.T) {
	p, err := NewPassProvider(PassOptions{Binary: "pass"}, answering(pemFixture+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve(t.Context(), "APP_STORE_CONNECT_KEY_P8")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pemFixture {
		t.Errorf("value = %q; only the first line survived", got)
	}
}

// `pass` exits 1 for a missing entry, a locked GPG agent, and a corrupt store
// alike, so every failure falls through — but the diagnosis is kept.
func TestPassProvider_FailuresFallThroughButKeepTheDiagnosis(t *testing.T) {
	for _, cause := range []error{exited("pass", 1), missingBinary("pass"), timedOut("pass")} {
		p, err := NewPassProvider(PassOptions{Binary: "pass"}, failing(cause))
		if err != nil {
			t.Fatal(err)
		}
		_, resolveErr := p.Resolve(t.Context(), "A")
		if !errors.Is(resolveErr, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound so the chain moves on", resolveErr)
		}
		var ce *CommandError
		if !errors.As(resolveErr, &ce) {
			t.Error("the CommandError was dropped, so doctor has nothing to print")
		}
	}
}

func TestNewPassProvider_Rejects(t *testing.T) {
	tests := []struct {
		name string
		opts PassOptions
	}{
		{"no binary", PassOptions{}},
		{"an arbitrary binary", PassOptions{Binary: "curl"}},
		{"a prefix that is an option", PassOptions{Binary: "pass", Prefix: "-x"}},
		{"a prefix escaping the store", PassOptions{Binary: "pass", Prefix: "../../etc"}},
		{"a prefix with a space", PassOptions{Binary: "pass", Prefix: "my store"}},
		{"a prefix with a dollar", PassOptions{Binary: "pass", Prefix: "$HOME"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPassProvider(tc.opts, answering("")); err == nil {
				t.Errorf("%+v was accepted", tc.opts)
			}
		})
	}
}

func TestPassProvider_RejectsABadName(t *testing.T) {
	p, err := NewPassProvider(PassOptions{Binary: "pass"}, answering("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(t.Context(), "-w"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
	if p.Name() != "pass" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestNewPassProvider_DefaultsToTheRealRunner(t *testing.T) {
	p, err := NewPassProvider(PassOptions{Binary: "pass"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.runner.(ExecRunner); !ok {
		t.Errorf("runner = %T, want ExecRunner", p.runner)
	}
}
