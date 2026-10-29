package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The values a store has to carry exactly, PEM first: §15's worked example
// declares one, and the file format only grew quoting because of it.
var awkwardValues = []string{
	"plain-token-value",
	"-----BEGIN PRIVATE KEY-----\nMIGHAgEAMBMGByqGSM49AgEGCCqGSM49\n-----END PRIVATE KEY-----\n",
	`with "quotes" and \backslash\`,
	"tab\tand trailing spaces   ",
	"  leading spaces and a # that is not a comment",
	"carriage\r\nreturn",
}

func newFileStore(t *testing.T, initial string) *FileStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewFileStore(path)
}

func TestFileStore_EveryValueReadsBackExactly(t *testing.T) {
	s := newFileStore(t, "")
	for i, v := range awkwardValues {
		name := "S" + string(rune('A'+i))
		if err := s.Set(context.Background(), name, Value(v)); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	p := NewFileProvider(s.Path())
	for i, v := range awkwardValues {
		name := "S" + string(rune('A'+i))
		got, err := p.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if string(got) != v {
			t.Errorf("%s: got %q, want %q", name, got, v)
		}
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secrets.env is %04o, want 0600", perm)
	}
}

func TestFileStore_ReplacesAndRemovesOnlyItsLine(t *testing.T) {
	initial := "# deploy credentials\nFIRST=one\n\nSECOND='two'\n"
	s := newFileStore(t, initial)
	ctx := context.Background()

	if err := s.Set(ctx, "FIRST", Value("uno")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "THIRD", Value("three")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(s.Path())
	if want := "# deploy credentials\nFIRST=\"uno\"\n\nSECOND='two'\nTHIRD=\"three\"\n"; string(got) != want {
		t.Errorf("after set:\n%q\nwant\n%q", got, want)
	}

	removed, err := s.Remove(ctx, "SECOND")
	if err != nil || !removed {
		t.Fatalf("remove: %v, %v", removed, err)
	}
	if removed, err := s.Remove(ctx, "SECOND"); err != nil || removed {
		t.Errorf("removing it twice: %v, %v", removed, err)
	}
	names, err := s.Names(ctx)
	if err != nil || !slices.Equal(names, []string{"FIRST", "THIRD"}) {
		t.Errorf("names = %v, %v", names, err)
	}
}

// A file this code cannot read is not one it may rewrite: the malformed line
// is, by construction, a line somebody was putting a credential on.
func TestFileStore_RefusesAFileItCannotRead(t *testing.T) {
	s := newFileStore(t, "GOOD=1\nthis line is not KEY=value-shaped\x00 ok\nnot a key\n")
	err := s.Set(context.Background(), "NEW", Value("x"))
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("want a refusal, got %v", err)
	}
	got, _ := os.ReadFile(s.Path())
	if !bytes.Contains(got, []byte("not a key")) || bytes.Contains(got, []byte("NEW")) {
		t.Errorf("the file was rewritten anyway:\n%s", got)
	}

	loose := newFileStore(t, "A=1\n")
	if err := os.Chmod(loose.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loose.Set(context.Background(), "B", Value("2")); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a world-readable secrets.env was written to: %v", err)
	}
}

func TestFileStore_NamesOfAMissingFileAreNone(t *testing.T) {
	names, err := newFileStore(t, "").Names(context.Background())
	if err != nil || len(names) != 0 {
		t.Errorf("names = %v, %v", names, err)
	}
}

// inputRunner records what a store hands security(1), stdin included.
type inputRunner struct {
	fakeRunner
	stdin [][]byte
}

func (r *inputRunner) RunInput(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
	r.stdin = append(r.stdin, bytes.Clone(stdin))
	return r.Run(ctx, argv)
}

func TestKeychainStore_SetKeepsTheValueOutOfArgv(t *testing.T) {
	r := &inputRunner{fakeRunner: fakeRunner{fn: func([]string) ([]byte, []byte, error) { return nil, nil, nil }}}
	s, err := NewKeychainStore("", r)
	if err != nil {
		t.Fatal(err)
	}
	value := awkwardValues[1]
	if err := s.Set(context.Background(), "app__production__KEY", Value(value)); err != nil {
		t.Fatal(err)
	}
	argv := r.lastCall()
	if !slices.Equal(argv, []string{"security", "-i"}) {
		t.Errorf("argv = %q, want security -i and nothing else", argv)
	}
	want := "add-generic-password -U -s latem -a app__production__KEY -X " + hex.EncodeToString([]byte(value)) + "\n"
	if len(r.stdin) != 1 || string(r.stdin[0]) != want {
		t.Errorf("stdin = %q\nwant  %q", r.stdin, want)
	}
	for _, a := range argv {
		if strings.Contains(a, "PRIVATE") {
			t.Error("the value reached argv")
		}
	}
}

func TestKeychainStore_SetReportsWhatSecuritySaid(t *testing.T) {
	r := &inputRunner{fakeRunner: fakeRunner{fn: func([]string) ([]byte, []byte, error) {
		return nil, []byte("add-generic-password: returned -25308\n"), nil
	}}}
	s, _ := NewKeychainStore("", r)
	if err := s.Set(context.Background(), "X", Value("v")); err == nil || !strings.Contains(err.Error(), "-25308") {
		t.Errorf("a refused add passed as success: %v", err)
	}
}

func TestKeychainStore_Remove(t *testing.T) {
	code := 0
	r := &inputRunner{fakeRunner: fakeRunner{fn: func([]string) ([]byte, []byte, error) {
		if code != 0 {
			return nil, nil, &CommandError{Program: "security", ExitCode: code, Err: errors.New("exit")}
		}
		return nil, nil, nil
	}}}
	s, _ := NewKeychainStore("", r)
	if removed, err := s.Remove(context.Background(), "X"); err != nil || !removed {
		t.Errorf("present: %v, %v", removed, err)
	}
	if want := []string{"security", "delete-generic-password", "-s", "latem", "-a", "X"}; !slices.Equal(r.lastCall(), want) {
		t.Errorf("argv = %q", r.lastCall())
	}
	code = keychainNotFound
	if removed, err := s.Remove(context.Background(), "X"); err != nil || removed {
		t.Errorf("absent: %v, %v", removed, err)
	}
	code = 51
	if _, err := s.Remove(context.Background(), "X"); err == nil {
		t.Error("a failing security passed as success")
	}
}

// The shape `security dump-keychain` prints, as captured on macOS 26.
const keychainDump = `keychain: "/Users/me/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    0x00000007 <blob>="latem"
    "acct"<blob>="app__production__CONVEX_DEPLOY_KEY"
    "cdat"<timedate>=0x32303236313032393132303030305A00  "20261029120000Z\000"
    "svce"<blob>="latem"
keychain: "/Users/me/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="someone@example.com"
    "svce"<blob>="Slack Safe Storage"
keychain: "/Users/me/Library/Keychains/login.keychain-db"
version: 512
class: "inet"
attributes:
    "acct"<blob>="NOT_A_GENP"
    "svce"<blob>="latem"
keychain: "/Users/me/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>="CLOUDFLARE_API_TOKEN"
    "svce"<blob>="latem"
keychain: "/Users/me/Library/Keychains/login.keychain-db"
version: 512
class: "genp"
attributes:
    "acct"<blob>=0x6E6F6E2D6173636969C3A9  "non-ascii\303\251"
    "svce"<blob>="latem"
`

func TestKeychainStore_NamesAreOnlyLatemsGenericPasswords(t *testing.T) {
	s, _ := NewKeychainStore("", &inputRunner{fakeRunner: fakeRunner{fn: func([]string) ([]byte, []byte, error) {
		return []byte(keychainDump), nil, nil
	}}})
	names, err := s.Names(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CLOUDFLARE_API_TOKEN", "app__production__CONVEX_DEPLOY_KEY"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}
