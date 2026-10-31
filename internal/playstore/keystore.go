package playstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Why an upload key cannot be read.
var (
	// ErrNoKeytool means this machine has no JDK to read a keystore with. That
	// is not a fault in the keystore, and a caller reports it as a skip.
	ErrNoKeytool = errors.New("no keytool on this machine")
	// ErrKeystorePassword is the password not opening the keystore.
	ErrKeystorePassword = errors.New("the password does not open the keystore")
	// ErrNoAlias is the alias not being in the keystore.
	ErrNoAlias = errors.New("the keystore has no key under that alias")
)

// UploadKey is what a keystore says of its upload key.
type UploadKey struct {
	Expires time.Time
}

const (
	// keytoolPasswordEnv carries the keystore's password to keytool. Not as
	// -storepass, which is an argument in every user's process listing.
	keytoolPasswordEnv = "LATEM_KEYSTORE_PASSWORD" //nolint:gosec // a variable's name, not a secret
	// keytoolTimeout is generous: a JVM starting on a loaded build machine.
	keytoolTimeout = 30 * time.Second
)

// ReadUploadKey opens a base64 keystore with keytool and reads its upload
// key's expiry.
//
// keytool reads a keystore from a file and nothing else, so the keystore is on
// disk for as long as keytool runs: in a directory only this user can open, and
// gone on every path out, as the build scripts do it. The alias is matched here
// and not passed to keytool, and no line of keytool's output reaches an error,
// because it holds the alias.
func ReadUploadKey(ctx context.Context, keytool string, keystoreB64, password []byte, alias string) (UploadKey, error) {
	if keytool == "" {
		return UploadKey{}, ErrNoKeytool
	}
	keystore := make([]byte, base64.StdEncoding.DecodedLen(len(keystoreB64)))
	n, err := base64.StdEncoding.Decode(keystore, bytes.TrimSpace(keystoreB64))
	if err != nil {
		return UploadKey{}, errors.New("it is not base64")
	}
	defer clear(keystore)

	dir, err := os.MkdirTemp("", "latem-store-")
	if err != nil {
		return UploadKey{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "upload.keystore")
	if err := os.WriteFile(path, keystore[:n], 0o600); err != nil {
		return UploadKey{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, keytoolTimeout)
	defer cancel()
	// #nosec G204 -- argv, no shell: keytool is a path this binary found, and
	// nothing here came from a job or a broker.
	cmd := exec.CommandContext(ctx, keytool,
		"-J-Duser.language=en", "-J-Duser.country=US", "-J-Duser.timezone=UTC",
		"-list", "-v", "-keystore", path, "-storepass:env", keytoolPasswordEnv)
	cmd.Env = append(os.Environ(), keytoolPasswordEnv+"="+string(password))
	output, err := cmd.CombinedOutput()
	defer clear(output)
	if err != nil {
		if strings.Contains(string(output), "password was incorrect") {
			return UploadKey{}, ErrKeystorePassword
		}
		if strings.Contains(string(output), "Unable to locate a Java Runtime") {
			return UploadKey{}, ErrNoKeytool // macOS's /usr/bin/keytool, with no JDK behind it
		}
		return UploadKey{}, errors.New("keytool could not read the keystore")
	}
	expires, found := expiryOfAlias(output, alias)
	if !found {
		return UploadKey{}, ErrNoAlias
	}
	return UploadKey{Expires: expires}, nil
}

// expiryOfAlias reads the first certificate of an alias out of `keytool -list
// -v`: the entry's own, and the ones after it are its chain. A date it cannot
// read comes back zero.
func expiryOfAlias(output []byte, alias string) (time.Time, bool) {
	current, found := false, false
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if name, ok := strings.CutPrefix(line, "Alias name: "); ok {
			if current {
				break
			}
			// A JKS keystore lowercases aliases, and a key alias is matched
			// the same way when Gradle signs.
			current = strings.EqualFold(strings.TrimSpace(name), alias)
			found = found || current
			continue
		}
		if !current {
			continue
		}
		if _, until, ok := strings.Cut(line, " until: "); ok {
			t, err := time.Parse("Mon Jan 2 15:04:05 MST 2006", strings.TrimSpace(until))
			if err != nil {
				return time.Time{}, true
			}
			return t, true
		}
	}
	return time.Time{}, found
}

// FindKeytool finds a keytool for a machine, in the order a build finds a JDK:
// JAVA_HOME, Homebrew's keg-only JDKs, then the PATH. A Mac's /usr/bin/keytool
// is last for a reason: with no JDK installed it is a stub.
//
// kegRoot is a path list of where Homebrew's kegs live, [DefaultKegRoots] on a
// real machine; a test passes "" to find none of the host's.
func FindKeytool(javaHome, kegRoot string, lookPath func(string) (string, error)) string {
	if javaHome != "" {
		if p := filepath.Join(javaHome, "bin", "keytool"); isExecutable(p) {
			return p
		}
	}
	for _, root := range strings.Split(kegRoot, string(os.PathListSeparator)) {
		if root == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, "openjdk*", "bin", "keytool"))
		for i := len(matches) - 1; i >= 0; i-- { // the newest sorts last
			if isExecutable(matches[i]) {
				return matches[i]
			}
		}
	}
	if p, err := lookPath("keytool"); err == nil {
		return p
	}
	return ""
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// DefaultKegRoots is where Homebrew keeps keg-only JDKs, as a path list.
func DefaultKegRoots() string {
	return fmt.Sprintf("/opt/homebrew/opt%c/usr/local/opt", os.PathListSeparator)
}
