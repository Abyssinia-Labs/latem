package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// mapProvider answers from a map: the values stored on "this machine".
type mapProvider map[string]string

func (m mapProvider) Name() string { return "map" }

func (m mapProvider) Resolve(_ context.Context, name string) (secrets.Value, error) {
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, secrets.ErrNotFound)
	}
	return secrets.Value(v), nil
}

type sshCall struct {
	argv  []string
	stdin string
}

// fakeCopy stands in for this machine's stores and for ssh, which answers
// with fail when it is set.
func fakeCopy(t *testing.T, stored mapProvider, fail error) *[]sshCall {
	t.Helper()
	var calls []sshCall
	prevReader, prevSSH := newSecretReader, runSSH
	newSecretReader = func(config.Paths) (secrets.Provider, error) { return stored, nil }
	runSSH = func(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
		in, _ := io.ReadAll(stdin)
		calls = append(calls, sshCall{argv, string(in)})
		if fail != nil {
			return fail
		}
		_, _ = io.WriteString(stdout, "stored it\n")
		return nil
	}
	t.Cleanup(func() { newSecretReader, runSSH = prevReader, prevSSH })
	return &calls
}

// A value goes on ssh's stdin and nowhere else, under the scope it was
// read from here, with the repository named the way the other machine knows it.
func TestSecretsCopy_SendsEachValueOnStdin(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	calls := fakeCopy(t, mapProvider{
		secrets.ScopedName("CLOUDFLARE_API_TOKEN", "app", "staging"): "cf-value-0123456789",
		secrets.ScopedName("CONVEX_DEPLOY_KEY", "app", "staging"):    "-----BEGIN-----\nline\n-----END-----\n",
	}, nil)

	out, err := latemIn(t, dir, "", "secrets", "copy", "--to", "mini", "-i", "/keys/latem",
		"--repo", "app", "--env", "staging", "CLOUDFLARE_API_TOKEN", "CONVEX_DEPLOY_KEY")
	if err != nil {
		t.Fatalf("secrets copy: %v\n%s", err, out)
	}
	if len(*calls) != 2 {
		t.Fatalf("ssh ran %d times, want 2", len(*calls))
	}
	first := (*calls)[0]
	if first.stdin != "cf-value-0123456789" || (*calls)[1].stdin != "-----BEGIN-----\nline\n-----END-----\n" {
		t.Errorf("stdin = %q, %q; want each value byte for byte", first.stdin, (*calls)[1].stdin)
	}
	argv := strings.Join(first.argv, " ")
	if strings.Contains(argv, "cf-value") || strings.Contains(out, "cf-value") {
		t.Errorf("the value left stdin:\nargv %s\nout %s", argv, out)
	}
	want := []string{"ssh", "-i", "/keys/latem", "-o", "IdentitiesOnly=yes", "--", "mini",
		`PATH="$HOME/.local/bin:$PATH" latem secrets set --repo me/app --env staging -- CLOUDFLARE_API_TOKEN`}
	if strings.Join(want, "\x00") != strings.Join(first.argv, "\x00") {
		t.Errorf("argv =\n%q\nwant\n%q", first.argv, want)
	}
	if !strings.Contains(out, "stored it") {
		t.Errorf("the other machine's answer was not shown:\n%s", out)
	}
}

// The first name that does not copy stops the rest: the next would fail the
// same way, or be sent to a machine that just refused one.
func TestSecretsCopy_StopsAtTheFirstFailure(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	calls := fakeCopy(t, mapProvider{"B": "bbbbbbbb"}, nil)
	_, err := latemIn(t, dir, "", "secrets", "copy", "--to", "mini", "A", "B")
	if err == nil || !strings.Contains(err.Error(), "no value for A") {
		t.Errorf("err = %v, want one naming A", err)
	}
	if len(*calls) != 0 {
		t.Errorf("ssh ran %d times after a missing name", len(*calls))
	}

	calls = fakeCopy(t, mapProvider{"A": "aaaaaaaa", "B": "bbbbbbbb"}, errors.New("exit status 1"))
	_, err = latemIn(t, dir, "", "secrets", "copy", "--to", "mini", "A", "B")
	if err == nil || !strings.Contains(err.Error(), "mini did not store A") {
		t.Errorf("err = %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("ssh ran %d times, want 1: B was sent after A was refused", len(*calls))
	}
}

func TestSecretsCopy_Refusals(t *testing.T) {
	dir := configDirWith(t, baseConfig)
	fakeCopy(t, mapProvider{}, nil)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"secrets", "copy", "--to", "mini"}, "at least one"},
		{[]string{"secrets", "copy", "TOKEN"}, "--to is required"},
		{[]string{"secrets", "copy", "--to", "mini", "--store", "vault", "TOKEN"}, "one of keychain, file"},
		{[]string{"secrets", "copy", "--to", "mini", "BAD-NAME"}, "must match"},
	} {
		if _, err := latemIn(t, dir, "", tc.args...); exitCode(err) != ExitUsage || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v; want a usage error mentioning %q", tc.args, err, tc.want)
		}
	}
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"tsedal":        "tsedal",
		"me/app":        "me/app",
		"my env":        "'my env'",
		"it's":          `'it'\''s'`,
		"":              "''",
		"$(rm -rf ~)":   "'$(rm -rf ~)'",
		"--repo":        "--repo",
		"production-eu": "production-eu",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %s, want %s", in, got, want)
		}
	}
}
