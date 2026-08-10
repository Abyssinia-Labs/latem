package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReadToken_RefusesEveryShapeThatIsNotAToken.
//
// Each of these is a way the machine token can be wrong on a real machine, and
// each has to fail with a message naming the file and never the contents. The
// mode check in particular is deliberately duplicated from internal/config: the
// config is read once at startup and a token is rotated whenever somebody runs
// `latem register`, so the load-time check cannot see the file the dial will
// actually use.
func TestReadToken_RefusesEveryShapeThatIsNotAToken(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, err := ReadToken(filepath.Join(t.TempDir(), "nope.key"))
		errorContains(t, err, "latem register")
	})

	t.Run("no path configured", func(t *testing.T) {
		_, err := ReadToken("")
		errorContains(t, err, "token_file")
	})

	t.Run("empty", func(t *testing.T) {
		_, err := ReadToken(tokenFileWith(t, "   \n", 0o600))
		errorContains(t, err, "empty")
	})

	t.Run("multi-line", func(t *testing.T) {
		// A pasted PEM. It cannot be a header value, and net/http would report
		// it much later as an opaque write failure.
		_, err := ReadToken(tokenFileWith(t, "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n", 0o600))
		errorContains(t, err, "several lines")
	})

	t.Run("a directory", func(t *testing.T) {
		_, err := ReadToken(t.TempDir())
		errorContains(t, err, "directory")
	})

	t.Run("far too large", func(t *testing.T) {
		_, err := ReadToken(tokenFileWith(t, strings.Repeat("x", MaxTokenBytes+1), 0o600))
		errorContains(t, err, "not a file this size")
	})

	t.Run("readable by other", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file modes carry no such meaning on Windows")
		}
		_, err := ReadToken(tokenFileWith(t, "tok", 0o644))
		errorContains(t, err, "chmod 0600")
	})
}

// TestReadToken_NeverPutsTheTokenInAnError. A token in an error message is a
// token in a log, an issue, and a screenshot. The whole reason §8 puts it in its
// own file is that config.toml is the file people paste into bug reports.
func TestReadToken_NeverPutsTheTokenInAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes carry no such meaning on Windows")
	}
	const secret = "sk-live-do-not-log-me"
	_, err := ReadToken(tokenFileWith(t, secret, 0o644))
	if err == nil {
		t.Fatal("expected the mode check to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the token appears in the error message: %v", err)
	}
}

// TestWriteToken_LandsAt0600AndAtomically.
//
// The mode has to be right from the moment the file exists, not set afterwards:
// a token briefly readable by everyone on a shared build machine is a token
// that leaked. And the rename means a machine that loses power mid-registration
// has either the old token or the new one, never half of either.
func TestWriteToken_LandsAt0600AndAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")

	if err := WriteToken(path, "  tok-one  "); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	got, err := ReadToken(path)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if got != "tok-one" {
		t.Errorf("token round-tripped as %q; surrounding whitespace should be trimmed", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != TokenFileMode {
			t.Errorf("token file is mode %04o, want %04o", perm, TokenFileMode)
		}
	}

	// Rotation replaces it, and leaves no temporary file behind.
	if err := WriteToken(path, "tok-two"); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	if got, _ := ReadToken(path); got != "tok-two" {
		t.Errorf("after rotation the token is %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the token directory holds %v; a failed or partial write left something behind", names)
	}
}

// TestWriteToken_RefusesWhatItCouldNotReadBack. Writing a token that ReadToken
// will reject turns a registration failure into a mysterious startup failure
// hours later.
func TestWriteToken_RefusesWhatItCouldNotReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	if err := WriteToken(path, "   "); err == nil {
		t.Error("WriteToken accepted an empty token")
	}
	if err := WriteToken(path, "line one\nline two"); err == nil {
		t.Error("WriteToken accepted a multi-line token")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused write still created the token file")
	}
}
