package selfupdate

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// trustedFS carries the release signing key into the binary.
//
// Embedded rather than read from disk at runtime, and that is the whole point:
// the key is what decides whether a downloaded binary may replace this one, so
// it has to live somewhere an attacker who can write to `~/.latem` cannot
// reach. A `trusted_key` config option would move the trust root into the
// directory the agent already treats as mutable state.
//
//go:embed trusted.pub
var trustedFS embed.FS

// ErrNoTrustedKey is returned by [TrustedKey] on a build whose trusted.pub is
// still the placeholder. It is not a failure to handle quietly: §20's
// self-update has no meaning without it, so the config loader refuses
// `auto_update = true` rather than letting the agent discover this daily.
var ErrNoTrustedKey = errors.New("this build trusts no release signing key")

var (
	trustedOnce sync.Once
	trustedKey  PublicKey
	trustedErr  error
)

// TrustedKey is the key releases must be signed with.
func TrustedKey() (PublicKey, error) {
	trustedOnce.Do(func() {
		b, err := trustedFS.ReadFile("trusted.pub")
		if err != nil {
			trustedErr = fmt.Errorf("reading the embedded trusted key: %w", err)
			return
		}
		body := string(b)
		if !hasKeyLine(body) {
			trustedErr = ErrNoTrustedKey
			return
		}
		trustedKey, trustedErr = ParsePublicKey(body)
		if trustedErr != nil {
			// A build with a malformed key is worse than one with none: it
			// looks like it can update and cannot. Say which file.
			trustedErr = fmt.Errorf("internal/selfupdate/trusted.pub is not a minisign public key: %w", trustedErr)
		}
	})
	return trustedKey, trustedErr
}

// HaveTrustedKey reports whether this build can verify a release. It is what
// the config loader consults, so the answer to "why was auto_update refused"
// is a property of the binary rather than of the machine.
func HaveTrustedKey() bool {
	_, err := TrustedKey()
	return err == nil
}

// hasKeyLine reports whether anything in the file could be a key, ignoring
// blank lines, `#` comments, and minisign's own comment line.
func hasKeyLine(body string) bool {
	for _, l := range strings.Split(body, "\n") {
		if isKeyNoise(l) {
			continue
		}
		return true
	}
	return false
}

func isKeyNoise(l string) bool {
	l = strings.TrimSpace(l)
	return l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "untrusted comment:")
}
