package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// The minisign format, as implemented rather than as remembered.
//
// Every field below was read off files that minisign 0.12 produced, because
// the one detail that matters most here is also the one most often stated
// wrongly in summaries of the format: **minisign prehashes by default.** A
// verifier written from memory handles `Ed` (PureEdDSA over the file) and
// rejects every signature the current tool emits.
//
//	public key   42 bytes:  "Ed" | key id (8) | Ed25519 public key (32)
//	signature    74 bytes:  alg  | key id (8) | Ed25519 signature (64)
//	global sig   64 bytes:  Ed25519 over signature-bytes || trusted comment
//
// `alg` is `Ed` for a signature over the file's bytes and `ED` for one over
// BLAKE2b-512 of them. Both are accepted: which one appears depends on the
// version of minisign that signed, and an operator re-signing a release by
// hand should not produce something this code calls forged.
const (
	algPure     = "Ed"
	algPrehash  = "ED"
	keyIDLen    = 8
	publicKeyOK = 2 + keyIDLen + ed25519.PublicKeySize
	signatureOK = 2 + keyIDLen + ed25519.SignatureSize
)

// PublicKey is a minisign public key: the trust root of §20's self-update.
type PublicKey struct {
	id  [keyIDLen]byte
	key ed25519.PublicKey
}

// ParsePublicKey reads a minisign public key.
//
// Both spellings an operator has to hand are accepted: the two-line `.pub`
// file minisign writes, and the bare base64 line it prints after `-G` (which
// is what people paste). Refusing one of them would make the documented
// keygen output the wrong thing to copy.
func ParsePublicKey(s string) (PublicKey, error) {
	var out PublicKey

	line := ""
	for _, l := range strings.Split(s, "\n") {
		if isKeyNoise(l) {
			continue
		}
		line = strings.TrimSpace(l)
		break
	}
	if line == "" {
		return out, errors.New("no key line: a minisign public key is a base64 line, " +
			"optionally preceded by an `untrusted comment:` line")
	}

	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return out, fmt.Errorf("the key line is not base64: %w", err)
	}
	if len(raw) != publicKeyOK {
		return out, fmt.Errorf("a minisign public key is %d bytes; this is %d "+
			"(a secret key, or a signature, pasted into the wrong place?)", publicKeyOK, len(raw))
	}
	// A public key always carries `Ed`. The algorithm that varies is the
	// signature's, not the key's.
	if alg := string(raw[:2]); alg != algPure {
		return out, fmt.Errorf("unknown public key algorithm %q, want %q", alg, algPure)
	}
	copy(out.id[:], raw[2:2+keyIDLen])
	out.key = ed25519.PublicKey(raw[2+keyIDLen:])
	return out, nil
}

// ID is the key id in the form minisign prints it, so a mismatch can be
// compared against the comment line in a `.pub` file by eye.
func (k PublicKey) ID() string {
	if k.key == nil {
		return ""
	}
	// minisign displays the id byte-reversed from its on-disk order.
	var b [keyIDLen]byte
	for i := range b {
		b[i] = k.id[keyIDLen-1-i]
	}
	return fmt.Sprintf("%X", b)
}

// Zero reports whether this is the empty key, which no build should be
// verifying against.
func (k PublicKey) Zero() bool { return k.key == nil }

// Signature is a parsed `.minisig`.
type Signature struct {
	alg      string
	id       [keyIDLen]byte
	sig      []byte
	global   []byte
	trusted  string
	hadFirst bool
}

// ParseSignature reads a `.minisig` file.
func ParseSignature(s string) (Signature, error) {
	var out Signature

	const trustedPrefix = "trusted comment:"
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "untrusted comment:"):
		case strings.HasPrefix(l, trustedPrefix):
			// Kept byte-exact after the prefix, including leading spaces: it
			// is signed input, and trimming it is a verification failure with
			// a misleading cause.
			out.trusted = strings.TrimPrefix(l, trustedPrefix)
			out.trusted = strings.TrimPrefix(out.trusted, " ")
		case !out.hadFirst:
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
			if err != nil {
				return out, fmt.Errorf("the signature line is not base64: %w", err)
			}
			if len(raw) != signatureOK {
				return out, fmt.Errorf("a minisign signature is %d bytes; this is %d", signatureOK, len(raw))
			}
			out.alg = string(raw[:2])
			copy(out.id[:], raw[2:2+keyIDLen])
			out.sig = raw[2+keyIDLen:]
			out.hadFirst = true
		default:
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(l))
			if err != nil {
				return out, fmt.Errorf("the global signature line is not base64: %w", err)
			}
			if len(raw) != ed25519.SignatureSize {
				return out, fmt.Errorf("the global signature is %d bytes, want %d",
					len(raw), ed25519.SignatureSize)
			}
			out.global = raw
		}
	}

	if !out.hadFirst {
		return out, errors.New("no signature line")
	}
	if out.global == nil {
		return out, errors.New("no global signature line: a `.minisig` has four lines, " +
			"and the fourth is what makes the trusted comment trusted")
	}
	return out, nil
}

// TrustedComment is the comment the global signature covers. It is only
// meaningful after [PublicKey.Verify] has returned nil.
func (s Signature) TrustedComment() string { return s.trusted }

// Verify checks a signature over content.
//
// Both halves are checked. Skipping the global signature is the tempting
// shortcut — the file is already authenticated by the first one — and it is
// wrong the moment anything reads the trusted comment, because that comment
// then comes from an unauthenticated line in a downloaded file. This code does
// read it (it carries the version), so both are checked here rather than
// remembered as a caveat somewhere else.
func (k PublicKey) Verify(content []byte, sig Signature) error {
	if k.Zero() {
		return errors.New("no public key: this build trusts no release signing key")
	}
	if k.id != sig.id {
		// The likeliest real failure, and the one where "invalid signature"
		// sends somebody hunting for corruption that is not there.
		var got PublicKey
		got.id = sig.id
		got.key = k.key // only so ID() renders; the bytes are never used
		return fmt.Errorf("this signature is from key %s, and this build trusts key %s",
			got.ID(), k.ID())
	}

	var signed []byte
	switch sig.alg {
	case algPure:
		signed = content
	case algPrehash:
		sum := blake2b.Sum512(content)
		signed = sum[:]
	default:
		return fmt.Errorf("unknown signature algorithm %q, want %q or %q",
			sig.alg, algPure, algPrehash)
	}

	if !ed25519.Verify(k.key, signed, sig.sig) {
		return errors.New("signature does not verify")
	}
	// The global signature covers the file signature and the trusted comment,
	// concatenated with no separator.
	if !ed25519.Verify(k.key, append(append([]byte{}, sig.sig...), []byte(sig.trusted)...), sig.global) {
		return errors.New("the trusted comment's signature does not verify")
	}
	return nil
}
