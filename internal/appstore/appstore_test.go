package appstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newKey mints a key of the kind Apple issues and returns it with its PEM, the
// way a resolved secret would arrive.
func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key, pkcs8PEM(t, key)
}

func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestParseKey_AcceptsWhatAppleIssues(t *testing.T) {
	key, p8 := newKey(t)

	got, err := ParseKey(p8)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if got.D.Cmp(key.D) != 0 {
		t.Fatal("ParseKey returned a different key")
	}
}

func TestParseKey_RefusesEveryOtherShape(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	_, p256 := newKey(t)

	cases := map[string]struct {
		in   []byte
		want string
	}{
		"empty":            {nil, "not PEM"},
		"not pem at all":   {[]byte("AUTHKEY_ABCDEFGHIJ"), "not PEM"},
		"wrong block type": {pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")}), "want PRIVATE KEY"},
		"not a key":        {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not der")}), "not a PKCS#8 key"},
		"wrong curve":      {pkcs8PEM(t, p384), "want P-256"},
		"rsa":              {pkcs8PEM(t, rsaKey), "want an EC key"},
		// The one that is not a shape problem: a valid key, truncated by a
		// copy-paste. It must fail rather than parse to something usable.
		"truncated": {p256[:len(p256)/2], "not PEM"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseKey(tc.in)
			if err == nil {
				t.Fatal("ParseKey accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// INV-4 applies to a diagnostic as much as to a log. A parse error is printed
// by `doctor`, so it must not carry the material it failed to parse.
func TestParseKey_TheErrorNeverCarriesTheKey(t *testing.T) {
	_, p8 := newKey(t)
	// Corrupt the DER while leaving the PEM envelope intact, which is what a
	// mangled paste produces.
	block, _ := pem.Decode(p8)
	block.Bytes[20] ^= 0xff
	broken := pem.EncodeToMemory(block)

	_, err := ParseKey(broken)
	if err == nil {
		t.Skip("corruption did not break the parse")
	}
	msg := err.Error()
	for _, fragment := range []string{
		string(broken),
		strings.TrimSpace(strings.Split(string(broken), "\n")[1]),
		base64.StdEncoding.EncodeToString(block.Bytes),
	} {
		if fragment != "" && strings.Contains(msg, fragment) {
			t.Fatalf("error message contains key material: %q", msg)
		}
	}
}

func TestFingerprint_StableAndKeySpecific(t *testing.T) {
	a, aPEM := newKey(t)
	b, _ := newKey(t)

	// Stability across a round trip through PEM, which is how the same key
	// arrives twice in practice: once as a resolved secret and once as a file
	// in ~/Downloads. A fingerprint that depended on anything but the public
	// point would compare the two and find them different.
	reparsed, err := ParseKey(aPEM)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(&a.PublicKey) != Fingerprint(&reparsed.PublicKey) {
		t.Fatal("the same key fingerprinted differently after a PEM round trip")
	}
	if Fingerprint(&a.PublicKey) == Fingerprint(&b.PublicKey) {
		t.Fatal("two keys fingerprint the same")
	}
	if got := len(Fingerprint(&a.PublicKey)); got != 16 {
		t.Fatalf("fingerprint is %d characters, want 16 to match the shell implementation", got)
	}
}

func TestShapes(t *testing.T) {
	for _, ok := range []string{"ABC1234567", "1234567890", "ZZZZZZZZZZ"} {
		if !ValidKeyID(ok) {
			t.Errorf("ValidKeyID(%q) = false", ok)
		}
	}
	// The lowercase case matters: Apple's are uppercase, and a lowercased
	// paste is a value that looks right and is not.
	for _, bad := range []string{"", "abc1234567", "ABC123456", "ABC12345678", "ABC-123456"} {
		if ValidKeyID(bad) {
			t.Errorf("ValidKeyID(%q) = true", bad)
		}
	}
	if !ValidIssuerID("69a6de70-1234-47e3-e053-5b8c7c11a4d1") {
		t.Error("a UUID was rejected")
	}
	// The single most common mistake: the key id pasted into the issuer field.
	if ValidIssuerID("ABC1234567") {
		t.Error("a key id was accepted as an issuer id")
	}
}

func TestScanKeyFiles(t *testing.T) {
	dir := t.TempDir()
	_, wanted := newKey(t)
	_, other := newKey(t)

	write := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("AuthKey_ABC1234567.p8", wanted)
	write("nested/one/AuthKey_ZZZ9999999.p8", other)
	// Skipped, each for its own reason: not Apple's naming, not parseable, and
	// too deep to be worth the IO.
	write("key.p8", wanted)
	write("AuthKey_BROKEN0001.p8", []byte("-----BEGIN PRIVATE KEY-----\nnope\n-----END PRIVATE KEY-----\n"))
	write("a/b/c/AuthKey_TOODEEP001.p8", wanted)

	files := ScanKeyFiles([]string{dir, filepath.Join(dir, "does-not-exist")})

	var ids []string
	for _, f := range files {
		ids = append(ids, f.KeyID)
	}
	if got := strings.Join(ids, ","); got != "ABC1234567,ZZZ9999999" {
		t.Fatalf("found %q, want ABC1234567,ZZZ9999999", got)
	}
}

// The defect A35 exists for, in one test: the key material on disk belongs to a
// different key than the one the environment names.
func TestMatchOnDisk_ReportsAKeyIDMismatch(t *testing.T) {
	key, p8 := newKey(t)
	files := []KeyFile{
		{KeyID: "OLDKEY0001", Fingerprint: Fingerprint(&key.PublicKey), Path: "/x/AuthKey_OLDKEY0001.p8"},
		{KeyID: "ABC1234567", Fingerprint: "0000000000000000", Path: "/x/AuthKey_ABC1234567.p8"},
	}

	file, found, agrees := MatchOnDisk(Credentials{KeyID: "ABC1234567", P8: p8}, files)
	if !found {
		t.Fatal("the key material is on disk and was not found")
	}
	if agrees {
		t.Fatal("a kid that does not match its own signature was reported as agreeing")
	}
	if file.KeyID != "OLDKEY0001" {
		t.Fatalf("matched %q, want the file the material actually came from", file.KeyID)
	}

	// And the agreeing case, so the test does not pass by always saying no.
	_, found, agrees = MatchOnDisk(Credentials{KeyID: "OLDKEY0001", P8: p8}, files)
	if !found || !agrees {
		t.Fatalf("a correct set was reported as found=%v agrees=%v", found, agrees)
	}
}

func TestMatchOnDisk_NothingOnDiskIsNotAMismatch(t *testing.T) {
	_, p8 := newKey(t)
	if _, found, _ := MatchOnDisk(Credentials{KeyID: "ABC1234567", P8: p8}, nil); found {
		t.Fatal("found a match among no files")
	}
}

// A token Apple accepts has three properties this test asserts and the 401 it
// returns otherwise does not distinguish: the claims for the key's own kind,
// the kid in the header, and a 64-byte raw signature rather than DER.
func TestToken(t *testing.T) {
	key, p8 := newKey(t)
	creds := Credentials{KeyID: "ABC1234567", IssuerID: "69a6de70-1234-47e3-e053-5b8c7c11a4d1", P8: p8}
	now := time.Unix(1_800_000_000, 0)

	for _, form := range []TokenForm{TeamKey, IndividualKey} {
		t.Run(string(form), func(t *testing.T) {
			tok, err := Token(creds, form, now)
			if err != nil {
				t.Fatalf("Token: %v", err)
			}
			parts := strings.Split(tok, ".")
			if len(parts) != 3 {
				t.Fatalf("token has %d segments, want 3", len(parts))
			}

			var header map[string]string
			decodeSegment(t, parts[0], &header)
			if header["alg"] != "ES256" || header["typ"] != "JWT" {
				t.Fatalf("header = %v", header)
			}
			if header["kid"] != creds.KeyID {
				t.Fatalf("kid = %q, want %q", header["kid"], creds.KeyID)
			}

			var payload map[string]any
			decodeSegment(t, parts[1], &payload)
			if payload["aud"] != "appstoreconnect-v1" {
				t.Fatalf("aud = %v", payload["aud"])
			}
			switch form {
			case TeamKey:
				if payload["iss"] != creds.IssuerID {
					t.Fatalf("a team token must carry iss: %v", payload)
				}
				if _, ok := payload["sub"]; ok {
					t.Fatalf("a team token must not carry sub: %v", payload)
				}
			case IndividualKey:
				if payload["sub"] != "user" {
					t.Fatalf("an individual token must carry sub=user: %v", payload)
				}
				if _, ok := payload["iss"]; ok {
					t.Fatalf("an individual token must not carry iss: %v", payload)
				}
			}
			if payload["iat"] != float64(now.Unix()) {
				t.Fatalf("iat = %v, want %d", payload["iat"], now.Unix())
			}
			if payload["exp"] != float64(now.Add(TokenLifetime).Unix()) {
				t.Fatalf("exp = %v", payload["exp"])
			}

			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatalf("signature is not base64url: %v", err)
			}
			if len(sig) != 64 {
				t.Fatalf("signature is %d bytes; JWS wants raw r||s, not DER", len(sig))
			}
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			r := new(big.Int).SetBytes(sig[:32])
			s := new(big.Int).SetBytes(sig[32:])
			if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
				t.Fatal("the signature does not verify against the key that made it")
			}
		})
	}
}

func TestToken_UnknownFormIsAnError(t *testing.T) {
	_, p8 := newKey(t)
	if _, err := Token(Credentials{KeyID: "ABC1234567", P8: p8}, TokenForm("guess"), time.Now()); err == nil {
		t.Fatal("an unknown form was minted anyway")
	}
}

func decodeSegment(t *testing.T, seg string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
}

func TestClientApps(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; this probe must be read-only", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"attributes":{"bundleId":"com.example.zeta","name":"Zeta"}},
			{"attributes":{"bundleId":"com.example.alpha","name":"Alpha"}},
			{"attributes":{"name":"no bundle id at all"}}
		]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := c.Apps(t.Context(), "a.b.c")
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	if got := strings.Join(res.BundleIDs, ","); got != "com.example.alpha,com.example.zeta" {
		t.Fatalf("bundle ids = %q, want them sorted and the blank one dropped", got)
	}
	if gotAuth != "Bearer a.b.c" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotPath, "/v1/apps") {
		t.Fatalf("path = %q", gotPath)
	}
}

// 401 and 403 mean different things — Apple rejected the token, versus accepted
// it and refused the role — and a caller that collapsed them would send an
// operator to regenerate a credential that works.
func TestClientApps_PassesStatusThroughWithoutABody(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`not json`))
		}))
		c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
		res, err := c.Apps(t.Context(), "t")
		srv.Close()
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if res.Status != status {
			t.Fatalf("status = %d, want %d", res.Status, status)
		}
		if len(res.BundleIDs) != 0 {
			t.Fatalf("status %d returned bundle ids", status)
		}
	}
}
