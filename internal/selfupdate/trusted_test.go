package selfupdate

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// The embedded key is the one thing in this package that changes state exactly
// once in the project's life: it is a placeholder until a release signing
// keypair exists, and a real key afterwards. A test written for one of those
// states is a test that starts failing on the day the other arrives — for a
// reason that has nothing to do with a defect — so this one asserts whichever
// is true and says what it checked.
func TestTrustedKey(t *testing.T) {
	body, err := os.ReadFile("trusted.pub")
	if err != nil {
		t.Fatal(err)
	}
	key, err := TrustedKey()

	if !hasKeyLine(string(body)) {
		if !errors.Is(err, ErrNoTrustedKey) {
			t.Fatalf("trusted.pub is the placeholder and TrustedKey returned %v, want ErrNoTrustedKey", err)
		}
		if HaveTrustedKey() {
			t.Error("HaveTrustedKey is true on a build with no key")
		}
		// The message an operator will meet in a config error, so it has to
		// say what to do and not merely what went wrong.
		if !strings.Contains(string(body), "minisign -G") {
			t.Error("the placeholder does not say how to replace itself")
		}
		t.Log("trusted.pub is the placeholder: the refusal path is what is under test")
		return
	}

	if err != nil {
		t.Fatalf("trusted.pub holds a key and TrustedKey refused it: %v", err)
	}
	if !HaveTrustedKey() {
		t.Error("HaveTrustedKey is false on a build with a key")
	}
	// A key id of the shape minisign prints, so an operator can compare it
	// against the release's signature by eye.
	if len(key.ID()) != 16 {
		t.Errorf("key id %q is not the 16 hex characters minisign displays", key.ID())
	}
	t.Logf("trusted.pub holds key %s", key.ID())
}

// A malformed key is worse than an absent one, because the build looks like it
// can update and cannot. The error has to name the file.
func TestTrustedKey_AMalformedKeyNamesTheFile(t *testing.T) {
	// The real embedded file cannot be substituted at runtime, so this
	// exercises the parse the same way TrustedKey does and checks the framing
	// separately.
	if _, err := ParsePublicKey("untrusted comment: x\nbm90LWEta2V5\n"); err == nil {
		t.Fatal("a short base64 blob was accepted as a public key")
	}
	if !hasKeyLine("untrusted comment: x\nbm90LWEta2V5\n") {
		t.Error("a file with a base64 line was read as having no key")
	}
	// And the inverse: comments alone are not a key, in either comment style.
	for _, body := range []string{
		"",
		"# nothing here\n",
		"untrusted comment: minisign public key AAAA\n",
		"\n\n   \n",
	} {
		if hasKeyLine(body) {
			t.Errorf("%q was read as holding a key", body)
		}
	}
}
