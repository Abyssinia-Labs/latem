package secrets

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseCommand_Accepts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"the spec's own example", `op read op://Private/match/password`,
			[]string{"op", "read", "op://Private/match/password"}},
		{"absolute path", `/usr/local/bin/op read op://x/y`,
			[]string{"/usr/local/bin/op", "read", "op://x/y"}},
		{"repeated whitespace", "op   read\top://x/y",
			[]string{"op", "read", "op://x/y"}},
		{"double quotes group", `op read "op://My Vault/item/field"`,
			[]string{"op", "read", "op://My Vault/item/field"}},
		{"single quotes are literal", `op read 'op://a b/c'`,
			[]string{"op", "read", "op://a b/c"}},
		// A shell would expand $HOME inside single quotes into nothing at all;
		// passing it literally is the same thing the shell would do.
		{"single quotes protect a dollar", `op read '$HOME/x'`,
			[]string{"op", "read", "$HOME/x"}},
		{"backslash escapes one character", `op read a\ b`,
			[]string{"op", "read", "a b"}},
		{"escaped dollar is literal", `op read \$HOME`,
			[]string{"op", "read", "$HOME"}},
		{"escaped quote inside double quotes", `op read "a\"b"`,
			[]string{"op", "read", `a"b`}},
		{"an empty argument is a real argument", `op read ""`,
			[]string{"op", "read", ""}},
		{"question marks survive, URLs have them", `op read op://x?y=1`,
			[]string{"op", "read", "op://x?y=1"}},
		{"leading and trailing space", `  op read  `,
			[]string{"op", "read"}},
		{"adjacent quoted and bare", `op read pre"fix"post`,
			[]string{"op", "read", "prefixpost"}},
		{"tilde inside a word is not expanded by a shell either", `op read a~b`,
			[]string{"op", "read", "a~b"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCommand(tc.in)
			if err != nil {
				t.Fatalf("ParseCommand(%q) = %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Every case here is something a shell would interpret. Passing it through as
// literal text would make the command quietly do something other than what the
// operator wrote, and the discovery point is a signing step at the end of a
// forty-minute build.
func TestParseCommand_RejectsAnythingNeedingAShell(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"pipe", `op read x | head -1`},
		{"semicolon", `op read x; rm -rf /`},
		{"background", `op read x & sleep 1`},
		{"output redirect", `op read x > /tmp/leak`},
		{"input redirect", `op read < /tmp/x`},
		{"command substitution", `op read $(cat /tmp/x)`},
		{"backtick substitution", "op read `cat /tmp/x`"},
		{"variable reference", `op read $VAULT/item`},
		{"variable inside double quotes", `op read "$VAULT/item"`},
		{"backtick inside double quotes", "op read \"`id`\""},
		{"subshell parens", `op read (x)`},
		{"newline is a command separator", "op read x\nrm -rf /"},
		{"carriage return", "op read x\rrm -rf /"},
		{"leading tilde", `~/bin/op read x`},
		{"tilde starting an argument", `op read ~/x`},
		{"unterminated single quote", `op read 'x`},
		{"unterminated double quote", `op read "x`},
		{"unterminated escape inside quotes", `op read "x\`},
		{"trailing backslash", `op read x\`},
		{"empty", ``},
		{"only whitespace", "  \t "},
		{"program is an option", `--version`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCommand(tc.in)
			if err == nil {
				t.Fatalf("ParseCommand(%q) = %q, want an error", tc.in, got)
			}
		})
	}
}

// The error has to tell an operator what to do instead, not just that they are
// wrong.
func TestParseCommand_ErrorNamesTheFix(t *testing.T) {
	_, err := ParseCommand(`op read $VAULT/item`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("err = %q, which does not explain that no shell is involved", err)
	}
}

func TestNewCommandProvider_ResolvesAConfiguredCommand(t *testing.T) {
	runner := answering("hunter2\n")
	p, err := NewCommandProvider(map[string]string{
		"MATCH_PASSWORD": "op read op://Private/match/password",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.Resolve(t.Context(), "MATCH_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hunter2" {
		t.Errorf("value = %q, want the tool's output minus its trailing newline", got)
	}

	// INV-3: an argv slice, never a shell.
	want := []string{"op", "read", "op://Private/match/password"}
	if !slices.Equal(p.commands["MATCH_PASSWORD"], want) {
		t.Errorf("argv = %q, want %q", p.commands["MATCH_PASSWORD"], want)
	}
	if got := runner.lastCall(); !slices.Equal(got, want) {
		t.Errorf("ran %q, want %q", got, want)
	}
}

func TestNewCommandProvider_RejectsAnUnparseableCommandAtLoad(t *testing.T) {
	_, err := NewCommandProvider(map[string]string{
		"MATCH_PASSWORD": "op read $VAULT/item",
	}, answering(""))
	if err == nil {
		t.Fatal("a command needing a shell was accepted at config load")
	}
	if !strings.Contains(err.Error(), "MATCH_PASSWORD") {
		t.Errorf("err = %q, which does not say which entry is wrong", err)
	}
}

func TestNewCommandProvider_RejectsABadSecretName(t *testing.T) {
	if _, err := NewCommandProvider(map[string]string{"-w": "op read x"}, answering("")); err == nil {
		t.Error("a secret name that is an option to `security` was accepted")
	}
}

func TestCommandProvider_UnconfiguredNameIsNotFound(t *testing.T) {
	p, err := NewCommandProvider(map[string]string{"A": "op read a"}, answering("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(t.Context(), "B"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound so the chain moves on", err)
	}
}

// The operator said explicitly where this secret comes from. Falling through
// to "no provider held it" would throw away the only diagnosis anyone has.
func TestCommandProvider_AFailingCommandIsNotANearMiss(t *testing.T) {
	p, err := NewCommandProvider(map[string]string{"A": "op read a"}, failing(exited("op", 1)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Resolve(t.Context(), "A")
	if err == nil {
		t.Fatal("a failing command was reported as success")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a failing command was reported as ErrNotFound, hiding it from the operator")
	}
}

func TestCommandProvider_AMissingBinaryIsAConfigurationError(t *testing.T) {
	p, err := NewCommandProvider(map[string]string{"A": "op read a"}, failing(missingBinary("op")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Resolve(t.Context(), "A")
	if errors.Is(err, ErrNotFound) {
		t.Error("an uninstalled program named by the operator was skipped silently")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("err = %q, which does not name the fix", err)
	}
}

func TestCommandProvider_RejectsABadNameAtResolve(t *testing.T) {
	p, err := NewCommandProvider(nil, answering(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(t.Context(), "not-an-identifier"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
}

func TestCommandProvider_Configured(t *testing.T) {
	p, err := NewCommandProvider(map[string]string{"B": "op read b", "A": "op read a"}, answering(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Configured(); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("Configured() = %q", got)
	}
	if p.Name() != "command" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestCommandProvider_DefaultsToTheRealRunner(t *testing.T) {
	p, err := NewCommandProvider(map[string]string{"A": "op read a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.runner.(ExecRunner); !ok {
		t.Errorf("runner = %T, want ExecRunner", p.runner)
	}
}
