package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/config"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// memoryKeychain stands in for the macOS Keychain. No test in this package
// may touch the real one: it holds the credentials of whoever runs the tests.
type memoryKeychain struct{ items map[string]string }

func (m *memoryKeychain) Name() string { return "keychain" }

func (m *memoryKeychain) Set(_ context.Context, name string, v secrets.Value) error {
	m.items[name] = string(v)
	return nil
}

func (m *memoryKeychain) Remove(_ context.Context, name string) (bool, error) {
	_, ok := m.items[name]
	delete(m.items, name)
	return ok, nil
}

func (m *memoryKeychain) Names(context.Context) ([]string, error) {
	return slices.Sorted(maps.Keys(m.items)), nil
}

// fakeStores routes the secrets commands to a memory keychain (nil for a
// machine without one) and a real secrets.env in the test's config directory.
func fakeStores(t *testing.T, withKeychain bool) *memoryKeychain {
	t.Helper()
	var kc *memoryKeychain
	if withKeychain {
		kc = &memoryKeychain{items: map[string]string{}}
	}
	prev := newSecretStores
	newSecretStores = func(p config.Paths) (secretStores, error) {
		st := secretStores{file: secrets.NewFileStore(p.SecretsEnv)}
		if kc != nil {
			st.keychain = kc
		}
		return st, nil
	}
	t.Cleanup(func() { newSecretStores = prev })
	return kc
}

const secretsConfig = baseConfig + `
[repos.latem-cloud]
url          = "https://github.com/me/latem-cloud.git"
allowed_refs = ["main"]
`

// The name a value is filed under is the name the run looks it up by — the
// whole point of --repo and --env.
func TestSecretsSet_FilesTheValueUnderTheRunsLookupName(t *testing.T) {
	dir := configDirWith(t, secretsConfig)
	kc := fakeStores(t, true)

	out, err := latemIn(t, dir, "dk_prod_0123456789\n", "secrets", "set",
		"--repo", "me/latem-cloud", "--env", "production", "CONVEX_DEPLOY_KEY")
	if err != nil {
		t.Fatalf("secrets set: %v\n%s", err, out)
	}
	want := secrets.ScopedName("CONVEX_DEPLOY_KEY", "latem-cloud", "production")
	if got, ok := kc.items[want]; !ok || got != "dk_prod_0123456789" {
		t.Errorf("keychain = %v, want %s = the value without echo's newline", kc.items, want)
	}
	if strings.Contains(out, "dk_prod") {
		t.Errorf("the value was printed:\n%s", out)
	}
	if !strings.Contains(out, want) || !strings.Contains(out, "Keychain") {
		t.Errorf("output does not say where it went:\n%s", out)
	}
}

func TestSecretsSet_FileStoreAndAMultiLineValue(t *testing.T) {
	dir := configDirWith(t, secretsConfig)
	fakeStores(t, false)
	pem := "-----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGByqGSM49\n-----END PRIVATE KEY-----\n"

	if out, err := latemIn(t, dir, pem, "secrets", "set", "--repo", "app", "APP_STORE_CONNECT_KEY_P8"); err != nil {
		t.Fatalf("secrets set: %v\n%s", err, out)
	}
	got, err := secrets.NewFileProvider(filepath.Join(dir, "secrets.env")).Resolve(context.Background(), "app__APP_STORE_CONNECT_KEY_P8")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pem {
		t.Errorf("a PEM lost bytes on the way in: %q", got)
	}
}

func TestSecretsSet_WarnsWhenSecretsEnvShadowsTheKeychain(t *testing.T) {
	dir := configDirWith(t, secretsConfig)
	fakeStores(t, true)
	if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("TOKEN=old-value-from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := latemIn(t, dir, "new-value-for-the-keychain", "secrets", "set", "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is read first") || !strings.Contains(out, "latem secrets rm --store file TOKEN") {
		t.Errorf("no warning that secrets.env answers first:\n%s", out)
	}
}

func TestSecretsSet_Refusals(t *testing.T) {
	dir := configDirWith(t, secretsConfig)
	kc := fakeStores(t, true)
	cases := []struct {
		args  []string
		stdin string
		code  int
		want  string
	}{
		{[]string{"secrets", "set", "TOKEN", "the-value"}, "", ExitUsage, "never from the command line"},
		{[]string{"secrets", "set", "--env", "production", "TOKEN"}, "v", ExitUsage, "--env needs --repo"},
		{[]string{"secrets", "set", "--repo", "nope", "TOKEN"}, "v", ExitFailure, "no repo"},
		{[]string{"secrets", "set", "BAD-NAME"}, "v", ExitUsage, "must match"},
		{[]string{"secrets", "set", "TOKEN"}, "", ExitFailure, "no value arrived"},
		{[]string{"secrets", "set", "--store", "vault", "TOKEN"}, "v", ExitUsage, "one of keychain, file"},
		{[]string{"secrets", "set", "TOKEN"}, "cf-token\x1b[Dvalue\n", ExitFailure, "escape character at byte 9 of 16 (an arrow key"},
		{[]string{"secrets", "set", "TOKEN"}, "binary\x00file", ExitFailure, "NUL byte"},
	}
	for _, tc := range cases {
		_, err := latemIn(t, dir, tc.stdin, tc.args...)
		if exitCode(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: exit %d, %v; want exit %d mentioning %q", tc.args, exitCode(err), err, tc.code, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "cf-token") {
			t.Errorf("%v: the refusal quotes the value: %v", tc.args, err)
		}
	}
	if len(kc.items) != 0 {
		t.Errorf("a refused value was stored: %v", slices.Collect(maps.Keys(kc.items)))
	}
}

func TestSecretsListAndRm(t *testing.T) {
	dir := configDirWith(t, secretsConfig+"\n[secrets.commands]\nMATCH_PASSWORD = \"op read op://ci/match\"\n")
	kc := fakeStores(t, true)
	kc.items["CLOUDFLARE_API_TOKEN"] = "x"
	kc.items[secrets.ScopedName("CONVEX_DEPLOY_KEY", "latem-cloud", "production")] = "x"
	kc.items[secrets.ScopedName("SENTRY", "app")] = "x"
	if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("CLOUDFLARE_API_TOKEN=y\nODD__NAME=z\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := latemIn(t, dir, "", "secrets", "list", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	want := "SCOPE\tNAME\tSTORE\n" +
		"*\tCLOUDFLARE_API_TOKEN\tfile, keychain (file answers)\n" +
		"*\tMATCH_PASSWORD\tcommand\n" +
		"*\tODD__NAME\tfile\n" +
		"app\tSENTRY\tkeychain\n" +
		"latem-cloud / production\tCONVEX_DEPLOY_KEY\tkeychain\n"
	if out != want {
		t.Errorf("secrets list =\n%s\nwant\n%s", out, want)
	}

	out, err = latemIn(t, dir, "", "secrets", "list", "--no-color", "--repo", "latem-cloud")
	if err != nil || strings.Contains(out, "CLOUDFLARE") || !strings.Contains(out, "CONVEX_DEPLOY_KEY") {
		t.Errorf("--repo filter:\n%s (%v)", out, err)
	}

	out, err = latemIn(t, dir, "", "secrets", "rm", "CLOUDFLARE_API_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "from the secrets.env") || !strings.Contains(out, "from the Keychain") {
		t.Errorf("rm did not remove from both stores:\n%s", out)
	}
	if _, err := latemIn(t, dir, "", "secrets", "rm", "CLOUDFLARE_API_TOKEN"); err == nil {
		t.Error("removing an absent secret succeeded")
	}
}

func TestTrimPipedValue(t *testing.T) {
	for in, want := range map[string]string{
		"token\n":     "token",
		"token\r\n":   "token",
		"token":       "token",
		"a\nb\n":      "a\nb\n",
		"token\n\n":   "token\n\n",
		"\n":          "",
		"two words\n": "two words",
	} {
		if got := string(trimPipedValue([]byte(in))); got != want {
			t.Errorf("trimPipedValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// The prompt reads past MAX_CANON — 1024 bytes on macOS, where a longer
// paste used to ring the bell and never end — and does canonical mode's
// editing itself.
func TestReadTypedLine(t *testing.T) {
	long := strings.Repeat("t", 1500)
	for name, c := range map[string]struct{ in, want string }{
		"a long paste":             {long + "\n", long},
		"a carriage return":        {"abc\r", "abc"},
		"erase":                    {"abx\x7fc\n", "abc"},
		"erase a multibyte letter": {"café\x7fe\n", "cafe"},
		"backspace":                {"ab\x08c\n", "ac"},
		"kill the line":            {"wrong\x15right\n", "right"},
		"end of input mid-line":    {"abc\x04", "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readTypedLine(bufio.NewReader(strings.NewReader(c.in)))
			if err != nil || string(got) != c.want {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
	if _, err := readTypedLine(bufio.NewReader(strings.NewReader("\x04"))); !errors.Is(err, io.EOF) {
		t.Errorf("^D on an empty line = %v, want EOF", err)
	}
}
