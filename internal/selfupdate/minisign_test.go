package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures under testdata/interop* were produced by minisign 0.12 on
// macOS, and the secret key was destroyed in the same shell that made them.
// That is deliberate: they are a fixed test vector rather than a keypair, and
// nothing in this repository should be able to mint a new signature that this
// code then agrees with. If the fixture ever has to change, regenerate all
// three files together:
//
//	T=$(mktemp -d)
//	minisign -G -p "$T/pub" -s "$T/sec" -W
//	minisign -S -s "$T/sec" -m checksums.txt -x checksums.txt.minisig -t "latem v0.1.0"
//	cp "$T/pub" interop.pub && rm -rf "$T"
//
// Verifying against a signature this package's own code produced would prove
// only that it is self-consistent, which is the one property a verifier gets
// for free and the one nobody needs.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMinisign_VerifiesWhatTheRealToolProduced(t *testing.T) {
	key, err := ParsePublicKey(string(fixture(t, "interop.pub")))
	if err != nil {
		t.Fatalf("parsing a public key minisign wrote: %v", err)
	}
	// minisign printed this id in the `.pub` file's comment line. The on-disk
	// byte order is the reverse of the displayed one, so an implementation that
	// skips the reversal produces a plausible-looking id that matches nothing
	// an operator can compare it against.
	if got, want := key.ID(), "FE75DDE9410473F3"; got != want {
		t.Errorf("key id = %s, want %s (the id in the fixture's comment line)", got, want)
	}

	sig, err := ParseSignature(string(fixture(t, "checksums.txt.minisig")))
	if err != nil {
		t.Fatalf("parsing a signature minisign wrote: %v", err)
	}
	// The one fact worth pinning: the current tool prehashes. A verifier that
	// only implements PureEdDSA parses this file, computes over the wrong
	// bytes, and reports a forgery.
	if sig.alg != algPrehash {
		t.Errorf("minisign 0.12's default algorithm is %q, and this fixture says %q — "+
			"if that changed, the pure path below is now the one that ships", algPrehash, sig.alg)
	}

	content := fixture(t, "checksums.txt")
	if err := key.Verify(content, sig); err != nil {
		t.Fatalf("a signature the real tool produced did not verify: %v", err)
	}
	if got, want := sig.TrustedComment(), "latem v0.1.0"; got != want {
		t.Errorf("trusted comment = %q, want %q", got, want)
	}
}

func TestMinisign_RejectsTamperedInput(t *testing.T) {
	key, err := ParsePublicKey(string(fixture(t, "interop.pub")))
	if err != nil {
		t.Fatal(err)
	}
	content := fixture(t, "checksums.txt")
	raw := string(fixture(t, "checksums.txt.minisig"))

	t.Run("content", func(t *testing.T) {
		sig, err := ParseSignature(raw)
		if err != nil {
			t.Fatal(err)
		}
		// One hex digit of one checksum: the smallest edit that would let a
		// substituted archive through.
		bad := append([]byte{}, content...)
		bad[0] = 'b'
		if err := key.Verify(bad, sig); err == nil {
			t.Fatal("a checksums file with one character changed verified")
		}
	})

	t.Run("trusted comment", func(t *testing.T) {
		// This is the assertion the global signature exists for. The version
		// this package reads comes out of that comment, so a comment anyone can
		// rewrite is a version anyone can rewrite.
		sig, err := ParseSignature(strings.Replace(raw,
			"trusted comment: latem v0.1.0", "trusted comment: latem v9.9.9", 1))
		if err != nil {
			t.Fatal(err)
		}
		if err := key.Verify(content, sig); err == nil {
			t.Fatal("a rewritten trusted comment verified: the global signature is not being checked")
		}
	})

	t.Run("signature bytes", func(t *testing.T) {
		sig, err := ParseSignature(raw)
		if err != nil {
			t.Fatal(err)
		}
		sig.sig[0] ^= 0x01
		if err := key.Verify(content, sig); err == nil {
			t.Fatal("a flipped signature bit verified")
		}
	})

	t.Run("another key", func(t *testing.T) {
		other, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		mine := key
		mine.key = other
		sig, err := ParseSignature(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := key.Verify(content, sig); err != nil {
			t.Fatalf("control: the real key should verify: %v", err)
		}
		if err := mine.Verify(content, sig); err == nil {
			t.Fatal("a different key verified the same signature")
		}
	})
}

// A signature from a key this build does not trust is the failure an operator
// meets after a key rotation, and it is not corruption. The message has to say
// which two keys, or the next step is an hour spent re-downloading a file that
// was never damaged.
func TestMinisign_AWrongKeyIDNamesBothKeys(t *testing.T) {
	key, err := ParsePublicKey(string(fixture(t, "interop.pub")))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := ParseSignature(string(fixture(t, "checksums.txt.minisig")))
	if err != nil {
		t.Fatal(err)
	}
	sig.id[0] ^= 0xFF

	err = key.Verify(fixture(t, "checksums.txt"), sig)
	if err == nil {
		t.Fatal("a signature from an untrusted key id verified")
	}
	if !strings.Contains(err.Error(), key.ID()) {
		t.Errorf("the error does not name the trusted key %s: %v", key.ID(), err)
	}
	if strings.Contains(err.Error(), "does not verify") {
		t.Errorf("a key mismatch is reported as a bad signature: %v", err)
	}
}

// The legacy PureEdDSA form. minisign 0.12 cannot produce one — its `-H` flag
// is the default and there is no way to turn prehashing off — so this fixture
// is assembled here from the format rather than from the tool, and is labelled
// as such. It exists because signature files outlive the versions that wrote
// them, and a release signed years ago by an older minisign must still verify.
func TestMinisign_AcceptsThePureEdDSAForm(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("deadbeef  latem_0.1.0_linux_amd64.tar.gz\n")
	const trusted = "latem v0.1.0"

	id := [keyIDLen]byte{1, 2, 3, 4, 5, 6, 7, 8}
	fileSig := ed25519.Sign(priv, content)
	global := ed25519.Sign(priv, append(append([]byte{}, fileSig...), []byte(trusted)...))

	b64 := func(parts ...[]byte) string {
		var all []byte
		for _, p := range parts {
			all = append(all, p...)
		}
		return base64.StdEncoding.EncodeToString(all)
	}
	sigFile := "untrusted comment: synthesised\n" +
		b64([]byte(algPure), id[:], fileSig) + "\n" +
		"trusted comment: " + trusted + "\n" +
		b64(global) + "\n"
	pubFile := "untrusted comment: synthesised\n" + b64([]byte(algPure), id[:], pub) + "\n"

	key, err := ParsePublicKey(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := ParseSignature(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	if sig.alg != algPure {
		t.Fatalf("this fixture is meant to exercise %q, and is %q", algPure, sig.alg)
	}
	if err := key.Verify(content, sig); err != nil {
		t.Fatalf("a PureEdDSA signature did not verify: %v", err)
	}
	// And the prehashed branch must not accept it: if the two were
	// interchangeable, the algorithm byte would be decoration.
	sig.alg = algPrehash
	if err := key.Verify(content, sig); err == nil {
		t.Fatal("a PureEdDSA signature verified as prehashed")
	}
}

func TestParsePublicKey_Refusals(t *testing.T) {
	// A bare base64 line is what `minisign -G` prints and therefore what gets
	// pasted. It has to work, or the documented keygen output is the wrong
	// thing to copy.
	bare := strings.Split(strings.TrimSpace(string(fixture(t, "interop.pub"))), "\n")[1]
	if _, err := ParsePublicKey(bare); err != nil {
		t.Errorf("the bare base64 line minisign prints was refused: %v", err)
	}

	for name, in := range map[string]string{
		"empty":       "",
		"comment":     "untrusted comment: minisign public key FE75DDE9410473F3\n",
		"not base64":  "this is not a key\n",
		"a signature": strings.Split(string(fixture(t, "checksums.txt.minisig")), "\n")[1],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublicKey(in); err == nil {
				t.Fatalf("%s was accepted as a public key", name)
			}
		})
	}
}

// A `.minisig` missing its fourth line is the shape a hand-edited or
// truncated file takes, and the missing line is the trusted comment's
// signature. Treating its absence as "nothing to check" is how a verifier
// ends up trusting an unsigned comment.
func TestParseSignature_RequiresTheGlobalSignature(t *testing.T) {
	raw := string(fixture(t, "checksums.txt.minisig"))
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	truncated := strings.Join(lines[:3], "\n") + "\n"

	if _, err := ParseSignature(truncated); err == nil {
		t.Fatal("a signature file with no global signature line was accepted")
	} else if !strings.Contains(err.Error(), "global") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}
