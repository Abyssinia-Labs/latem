package remote

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TokenFileMode is the mode the machine token must be stored at. It is the
// same 0600 internal/config enforces at load; it is re-checked here because a
// token can be rotated long after the config was read, and because a single
// validation point that everything else trusts is the shape of the next
// incident (INV-3's own words, applied to a credential).
const TokenFileMode os.FileMode = 0o600

// MaxTokenBytes bounds the token file. A machine token is an opaque string of
// some tens of bytes; anything approaching this is a wrong path, a log file, or
// somebody's private key, and reading it into an Authorization header would put
// it on a network.
const MaxTokenBytes = 8 << 10

// ReadToken reads the machine token.
//
// It is called on every dial rather than once at startup, which is what makes
// `latem register` able to rotate a token without a restart — internal/config's
// reloader documents exactly that expectation.
//
// The returned value is never logged, never included in an error message, and
// never put anywhere but the Authorization header. Every error here names the
// path and says nothing about the contents.
func ReadToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("remote: no token_file configured")
	}

	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("remote: the machine token file %s does not exist; run `latem register` to create it", path)
		}
		return "", fmt.Errorf("remote: reading the machine token: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("remote: the machine token file %s is a directory", path)
	}
	if fi.Size() > MaxTokenBytes {
		return "", fmt.Errorf("remote: the machine token file %s is %d bytes; a machine token is not a file this size", path, fi.Size())
	}
	// Modes are a Unix concept. RUNNER_SPEC targets Linux and macOS (§13) and
	// Windows carries none of this meaning, so the check is skipped there rather
	// than made to mean something it does not.
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf(
				"remote: the machine token file %s is mode %04o and is readable by group or other; a leaked machine token is a machine somebody else can build on — `chmod 0600 %s`",
				path, perm, path)
		}
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is this agent's own configured token_file, checked above
	if err != nil {
		return "", fmt.Errorf("remote: reading the machine token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("remote: the machine token file %s is empty; run `latem register` to write one", path)
	}
	// A header value cannot carry a newline, and a token that contains one is
	// either a PEM somebody pasted by mistake or an attempt at header
	// injection. Both are refused here rather than by net/http, which would
	// report it as an opaque write failure much later.
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("remote: the machine token in %s spans several lines; it should be the single opaque string `latem register` was given", path)
	}
	return token, nil
}

// WriteToken stores the machine token at mode 0600, replacing whatever was
// there.
//
// The write is to a temporary file in the same directory followed by a rename,
// so a machine that loses power mid-registration has either the old token or
// the new one and never half of either. The temporary file is created at 0600
// before anything is written to it, so the secret is never briefly readable.
func WriteToken(path, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("remote: refusing to write an empty machine token to %s", path)
	}
	if strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("remote: refusing to write a multi-line machine token to %s", path)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remote: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".agent.key-*")
	if err != nil {
		return fmt.Errorf("remote: creating a temporary token file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(TokenFileMode); err != nil {
		return fmt.Errorf("remote: setting the mode on the machine token file: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("remote: writing the machine token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("remote: flushing the machine token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remote: closing the machine token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("remote: installing the machine token at %s: %w", path, err)
	}
	return nil
}
