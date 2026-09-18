package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSecretsFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Explicitly, because umask makes the WriteFile mode a request rather than
	// an instruction and this test is entirely about the mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const pemFixture = "-----BEGIN PRIVATE KEY-----\n" +
	"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDb3xK1p2vQ8lQe\n" +
	"9pJ0mQ1yTn5oXk3uYb7cWq2sVdF4hGjKlMnOpQrStUvWxYzA1B2C3D4E5F6G7H8I\n" +
	"-----END PRIVATE KEY-----"

func TestFileProvider_Resolve(t *testing.T) {
	body := strings.Join([]string{
		"# a comment",
		"",
		"   ",
		"MATCH_PASSWORD=hunter2",
		"  SPACED   =   value with spaces   ",
		`QUOTED="  leading and trailing  "`,
		`LITERAL='raw $HOME \n stays'`,
		`PEM="` + strings.ReplaceAll(pemFixture, "\n", `\n`) + `"`,
		"EMPTY=",
		"TABS=\"a\\tb\"",
		"ESCAPED=\"say \\\"hi\\\"\"",
		"CR=\"a\\rb\"",
		"BACKSLASH=\"C:\\\\path\"",
		`EMPTY_QUOTED=""`,
	}, "\n")
	path := writeSecretsFile(t, body, 0o600)
	p := NewFileProvider(path)

	tests := []struct {
		name    string
		secret  string
		want    string
		wantErr error
	}{
		{"plain", "MATCH_PASSWORD", "hunter2", nil},
		{"whitespace around key and value", "SPACED", "value with spaces", nil},
		{"double quotes preserve whitespace", "QUOTED", "  leading and trailing  ", nil},
		{"single quotes are literal", "LITERAL", `raw $HOME \n stays`, nil},
		// §15's own example job declares APP_STORE_CONNECT_KEY_P8, and a PEM
		// cannot be written as one KEY=value line without this.
		{"a multi-line PEM survives", "PEM", pemFixture, nil},
		{"escaped tab", "TABS", "a\tb", nil},
		{"escaped carriage return", "CR", "a\rb", nil},
		{"escaped backslash", "BACKSLASH", `C:\path`, nil},
		{"escaped quote", "ESCAPED", `say "hi"`, nil},
		{"present but empty", "EMPTY", "", nil},
		{"quoted empty", "EMPTY_QUOTED", "", nil},
		{"absent", "ABSENT", "", ErrNotFound},
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

// RUNNER_SPEC §8 calls a world-readable config directory "the whole threat
// model failing quietly". A file provider that reads a 0644 secrets.env is
// exactly that failure.
func TestFileProvider_RefusesALooseMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666, 0o660} {
		t.Run(mode.String(), func(t *testing.T) {
			p := NewFileProvider(writeSecretsFile(t, "A=hunter2\n", mode))
			_, err := p.Resolve(t.Context(), "A")
			if err == nil {
				t.Fatalf("mode %04o was accepted", mode.Perm())
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatal("a loose mode fell through to the next provider instead of stopping the chain")
			}
			if !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("err = %q, which does not name the fix", err)
			}
		})
	}
}

func TestFileProvider_AcceptsStricterThan0600(t *testing.T) {
	p := NewFileProvider(writeSecretsFile(t, "A=hunter2\n", 0o400))
	if _, err := p.Resolve(t.Context(), "A"); err != nil {
		t.Errorf("a read-only-by-owner file was refused: %v", err)
	}
}

// Not every machine keeps a secrets file. An absent one is a provider with
// nothing to say, not a misconfiguration.
func TestFileProvider_AMissingFileIsAMiss(t *testing.T) {
	p := NewFileProvider(filepath.Join(t.TempDir(), "nope.env"))
	if _, err := p.Resolve(t.Context(), "A"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The mode of a symlink is always 0777, so honouring one means the mode
// guarantee belongs to a file this code never inspected.
func TestFileProvider_RefusesASymlink(t *testing.T) {
	real := writeSecretsFile(t, "A=hunter2\n", 0o600)
	link := filepath.Join(t.TempDir(), FileName)
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := NewFileProvider(link).Resolve(t.Context(), "A")
	if err == nil {
		t.Fatal("a symlinked secrets.env was read")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a symlinked secrets.env fell through instead of stopping the chain")
	}
}

func TestFileProvider_RefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileProvider(dir).Resolve(t.Context(), "A"); err == nil {
		t.Error("a directory was read as a secrets file")
	}
}

func TestFileProvider_RefusesAnOversizeFile(t *testing.T) {
	path := writeSecretsFile(t, "A="+strings.Repeat("x", MaxFileSize+1)+"\n", 0o600)
	_, err := NewFileProvider(path).Resolve(t.Context(), "A")
	if err == nil {
		t.Fatal("a file past the size bound was read")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %q", err)
	}
}

// A malformed line in this file is, by construction, a line someone was trying
// to put a credential on. Every message names a line NUMBER and never content.
func TestFileProvider_MalformedLines(t *testing.T) {
	tests := []struct {
		name string
		body string
		// leaked is a substring that must not appear in the error, because it
		// is the part of the line that might be the secret.
		leaked string
	}{
		{"no equals sign", "MATCH_PASSWORD hunter2\n", "hunter2"},
		{"key is not an identifier", "not-an-identifier=hunter2\n", "hunter2"},
		{"key with a leading dash", "-w=hunter2\n", "hunter2"},
		{"exported form", "export A=hunter2\n", "hunter2"},
		{"duplicate key", "A=hunter2\nA=hunter3\n", "hunter3"},
		{"unterminated double quote", "A=\"hunter2\n", "hunter2"},
		{"unterminated single quote", "A='hunter2\n", "hunter2"},
		{"unknown escape", `A="hunter\q2"` + "\n", "hunter"},
		{"junk after a closing double quote", `A="hunter2" hunter3` + "\n", "hunter3"},
		{"junk after a closing single quote", `A='hunter2' hunter3` + "\n", "hunter3"},
		{"empty key", "=hunter2\n", "hunter2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewFileProvider(writeSecretsFile(t, tc.body, 0o600))
			_, err := p.Resolve(t.Context(), "A")
			if err == nil {
				t.Fatal("a malformed file parsed cleanly")
			}
			if errors.Is(err, ErrNotFound) {
				t.Error("a malformed file fell through instead of stopping the chain")
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("err = %q, which does not name a line number", err)
			}
			if strings.Contains(err.Error(), tc.leaked) {
				t.Errorf("err = %q quotes line content, which may be the secret", err)
			}
		})
	}
}

// Last-wins is the dotenv convention and it is the wrong one here: a shadowed
// credential is a build that signs with the key nobody meant.
func TestFileProvider_DuplicateKeyNamesBothLines(t *testing.T) {
	p := NewFileProvider(writeSecretsFile(t, "A=one\nB=two\nA=three\n", 0o600))
	_, err := p.Resolve(t.Context(), "A")
	if err == nil {
		t.Fatal("a duplicate key was accepted")
	}
	for _, want := range []string{"line 3", "line 1", "A"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

func TestFileProvider_RejectsABadName(t *testing.T) {
	p := NewFileProvider(writeSecretsFile(t, "A=x\n", 0o600))
	if _, err := p.Resolve(t.Context(), "-w"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want a validation failure", err)
	}
	if p.Name() != "file" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.Path() == "" {
		t.Error("Path() is empty")
	}
}

func TestFileProvider_HandlesCRLF(t *testing.T) {
	p := NewFileProvider(writeSecretsFile(t, "A=hunter2\r\nB=hunter3\r\n", 0o600))
	got, err := p.Resolve(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hunter2" {
		t.Errorf("value = %q; a CRLF file left the carriage return in the value", got)
	}
}

// Values other than the one asked for must not outlive the call.
func TestParseSecretsFile_ProducesIndependentBuffers(t *testing.T) {
	entries, err := parseSecretsFile([]byte("A=hunter2\nB=hunter3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	zero(entries["A"].value)
	if string(entries["B"].value) != "hunter3" {
		t.Error("zeroing one value damaged another")
	}
}

// TestFileProvider_Names is the F15 regression at its source.
//
// `doctor --secrets` enumerated names by splitting every line on the first "=",
// which promotes the interior lines of a quoted multi-line value to names — a
// PEM's own base64, printed under a flag whose documented contract is "names
// only, never values".
func TestFileProvider_Names(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "secrets.env")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("names only, sorted", func(t *testing.T) {
		p := NewFileProvider(write(t, "# c\n\nZED=1\nALPHA=2\n"))
		got, err := p.Names()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"ALPHA", "ZED"}) {
			t.Errorf("Names() = %v", got)
		}
	})

	// A value may contain "=" — base64 padding above all — and a name is what
	// is left of the FIRST one. The escaped form is how this file holds a PEM
	// at all; the literal multi-line form does not parse, which the case below
	// covers.
	t.Run("a value containing = contributes one name", func(t *testing.T) {
		escaped := strings.ReplaceAll(pemFixture, "\n", `\n`)
		p := NewFileProvider(write(t, `PEM="`+escaped+`"`+"\nTOKEN=abc=def=ghi\n"))
		got, err := p.Names()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"PEM", "TOKEN"}) {
			t.Fatalf("Names() = %v, want [PEM TOKEN]", got)
		}
	})

	t.Run("a file that does not parse yields no names", func(t *testing.T) {
		// An unterminated quote: every line below the opening one is value.
		p := NewFileProvider(write(t, "A=1\nB=\"unterminated\nQQQQINTERIOR=still-value\n"))
		got, err := p.Names()
		if err == nil {
			t.Fatal("want an error")
		}
		if len(got) != 0 {
			t.Errorf("Names() = %v, want none", got)
		}
		if strings.Contains(err.Error(), "QQQQINTERIOR") {
			t.Errorf("the error carries line content: %q", err)
		}
	})

	t.Run("an absent file is ErrNotFound", func(t *testing.T) {
		p := NewFileProvider(filepath.Join(t.TempDir(), "absent"))
		if _, err := p.Names(); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}
