package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/appstore"
	"github.com/Abyssinia-Labs/latem/internal/secrets"
)

// testKey mints a key of the kind Apple issues, and its PEM.
func testKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

const testIssuer = "69a6de70-1234-47e3-e053-5b8c7c11a4d1"

// appStoreDoctor wires a doctor holding one credential set, with Apple stood in
// for by a local server.
func appStoreDoctor(t *testing.T, values map[string]string, handler http.HandlerFunc) *doctor {
	t.Helper()
	dir := privateDir(t)
	d := newTestDoctor("darwin", dir, healthyRunner("darwin"), testConfig(dir), nil)
	d.includeSecrets = true
	d.newSecrets = (&fakeSecrets{values: values}).factory()
	d.clock = func() time.Time { return time.Unix(1_800_000_000, 0) }

	if handler != nil {
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		d.newAppStore = func() *appstore.Client {
			return &appstore.Client{BaseURL: srv.URL, HTTP: srv.Client()}
		}
	} else {
		// No handler means no test should be reaching the network at all.
		d.newAppStore = func() *appstore.Client {
			return &appstore.Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}}
		}
	}
	return d
}

func fullSet(p8 string) map[string]string {
	return map[string]string{
		appstore.SecretKeyID:    "ABC1234567",
		appstore.SecretIssuerID: testIssuer,
		appstore.SecretP8:       p8,
	}
}

// A machine with no iOS credentials is a supported machine — a Linux runner, or
// a Mac that only builds simulator smoke jobs. It must produce no checks at
// all rather than a row of skips nobody can act on.
func TestDoctorCredentials_SilentWithoutAny(t *testing.T) {
	d := appStoreDoctor(t, nil, nil)
	if got := d.checkCredentials(t.Context()); len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}

// A partial set is worse than none: it fails at upload with the same generic
// authentication error as a wrong one, having first spent the build.
func TestDoctorCredentials_PartialSetIsAFailure(t *testing.T) {
	_, p8 := testKey(t)
	values := fullSet(p8)
	delete(values, appstore.SecretIssuerID)

	got := appStoreDoctor(t, values, nil).checkCredentials(t.Context())
	if len(got) != 1 || got[0].Level != levelFail {
		t.Fatalf("got %v, want one failure", got)
	}
	if !strings.Contains(got[0].Detail, appstore.SecretIssuerID) {
		t.Fatalf("the failure does not name the missing secret: %q", got[0].Detail)
	}
}

// A backend that is broken rather than empty — a locked keychain after a reboot
// — must not read as "this machine has no iOS credentials".
func TestDoctorCredentials_AnUnresolvableSecretIsNotAnAbsentOne(t *testing.T) {
	_, p8 := testKey(t)
	values := fullSet(p8)
	delete(values, appstore.SecretP8)
	fake := &fakeSecrets{
		values: values,
		errs:   map[string]error{appstore.SecretP8: errors.New("keychain is locked")},
	}
	d := appStoreDoctor(t, nil, nil)
	d.newSecrets = fake.factory()

	got := d.checkCredentials(t.Context())
	if len(got) != 1 || got[0].Level != levelFail {
		t.Fatalf("got %v, want one failure", got)
	}
}

func TestDoctorCredentials_Shapes(t *testing.T) {
	_, p8 := testKey(t)

	cases := map[string]struct {
		mutate  func(map[string]string)
		wantID  string
		wantSay string
	}{
		"a lowercased key id": {
			mutate:  func(m map[string]string) { m[appstore.SecretKeyID] = "abc1234567" },
			wantID:  "secret.asc.keyid",
			wantSay: "ten uppercase alphanumerics",
		},
		// The single most common mistake, and the reason the issuer id is
		// checkable at all: it is a UUID and a key id is not.
		"the key id pasted into the issuer id": {
			mutate:  func(m map[string]string) { m[appstore.SecretIssuerID] = "ABC1234567" },
			wantID:  "secret.asc.issuer",
			wantSay: "not a UUID",
		},
		"a path instead of the key's contents": {
			mutate:  func(m map[string]string) { m[appstore.SecretP8] = "~/Downloads/AuthKey_ABC1234567.p8" },
			wantID:  "secret.asc.key",
			wantSay: "not PEM",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			values := fullSet(p8)
			tc.mutate(values)

			// No handler: a set this wrong must not reach Apple.
			got := appStoreDoctor(t, values, nil).checkCredentials(t.Context())

			rep := report{Sections: []section{{Name: "Secrets", Checks: got}}}
			c, ok := findCheck(rep, tc.wantID)
			if !ok {
				t.Fatalf("check %s did not run: %v", tc.wantID, got)
			}
			if c.Level != levelFail {
				t.Fatalf("%s = %v, want fail", tc.wantID, c.Level)
			}
			if !strings.Contains(c.Detail, tc.wantSay) {
				t.Fatalf("detail %q does not mention %q", c.Detail, tc.wantSay)
			}
			auth, ok := findCheck(rep, "secret.asc.auth")
			if !ok || auth.Level != levelSkip {
				t.Fatalf("Apple was contacted with a broken set: %v", auth)
			}
		})
	}
}

// The A35 finding itself: four AuthKey_*.p8 on one laptop, all valid, one of
// them the right one. The token would carry a kid the signature disagrees
// with, and Apple's 401 names neither half.
func TestDoctorCredentials_KeyIDMismatchAgainstDisk(t *testing.T) {
	key, p8 := testKey(t)

	d := appStoreDoctor(t, fullSet(p8), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	// The same key material, on disk, under the name of a different key.
	downloads := filepath.Join(d.sys.Home(), "Downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(filepath.Join(downloads, "AuthKey_OLDKEY0001.p8"), onDisk, 0o600); err != nil {
		t.Fatal(err)
	}

	rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
	c, ok := findCheck(rep, "secret.asc.material")
	if !ok {
		t.Fatal("the key id agreement check did not run")
	}
	if c.Level != levelFail {
		t.Fatalf("level = %v, want fail (detail %q)", c.Level, c.Detail)
	}
	for _, want := range []string{"ABC1234567", "OLDKEY0001"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q does not name %q", c.Detail, want)
		}
	}
	if len(c.Fix) == 0 {
		t.Error("the mismatch carries no fix")
	}
	// Nothing anywhere in the output may carry the key material itself.
	assertNoKeyMaterial(t, rep, string(onDisk))
}

func TestDoctorCredentials_AppleAccepts(t *testing.T) {
	_, p8 := testKey(t)
	d := appStoreDoctor(t, fullSet(p8), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"attributes":{"bundleId":"com.abyssinialabs.latemexample"}}]}`))
	})

	rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
	c, ok := findCheck(rep, "secret.asc.auth")
	if !ok {
		t.Fatal("the auth check did not run")
	}
	if c.Level != levelPass {
		t.Fatalf("level = %v, want pass (detail %q)", c.Level, c.Detail)
	}
	if !strings.Contains(strings.Join(c.Notes, " "), "com.abyssinialabs.latemexample") {
		t.Fatalf("the visible apps are not reported: %v", c.Notes)
	}
	assertNoKeyMaterial(t, rep, p8)
}

// Authenticating and seeing nothing is not success: the upload fails for an app
// this key cannot see, after the build has been paid for.
func TestDoctorCredentials_AcceptedButSeesNoApps(t *testing.T) {
	_, p8 := testKey(t)
	d := appStoreDoctor(t, fullSet(p8), func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})

	rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
	c, _ := findCheck(rep, "secret.asc.auth")
	if c.Level != levelWarn {
		t.Fatalf("level = %v, want warn (detail %q)", c.Level, c.Detail)
	}
}

// An individual key authenticates here and can never upload through fastlane,
// which is a whole afternoon if nothing says so. The team form is tried first
// and answered 401; the individual form is what this server accepts.
func TestDoctorCredentials_IndividualKeyIsNamed(t *testing.T) {
	_, p8 := testKey(t)
	d := appStoreDoctor(t, fullSet(p8), func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(tokenPayload(r), `"sub":"user"`) {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})

	rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
	c, _ := findCheck(rep, "secret.asc.auth")
	if c.Level != levelFail {
		t.Fatalf("level = %v, want fail (detail %q)", c.Level, c.Detail)
	}
	if !strings.Contains(c.Detail, "INDIVIDUAL") {
		t.Fatalf("detail %q does not name the key kind", c.Detail)
	}
}

func TestDoctorCredentials_AppleRejects(t *testing.T) {
	_, p8 := testKey(t)
	for status, want := range map[int]level{
		http.StatusUnauthorized:        levelFail,
		http.StatusForbidden:           levelFail,
		http.StatusTooManyRequests:     levelWarn,
		http.StatusInternalServerError: levelWarn,
	} {
		d := appStoreDoctor(t, fullSet(p8), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
		c, ok := findCheck(rep, "secret.asc.auth")
		if !ok {
			t.Fatalf("status %d: the auth check did not run", status)
		}
		if c.Level != want {
			t.Errorf("status %d = %v, want %v (detail %q)", status, c.Level, want, c.Detail)
		}
	}
}

// doctor runs on machines with no egress, and a credential nobody can verify is
// not a credential anybody has shown to be wrong.
func TestDoctorCredentials_NoNetworkIsASkip(t *testing.T) {
	_, p8 := testKey(t)
	d := appStoreDoctor(t, fullSet(p8), nil) // points at a closed port

	rep := report{Sections: []section{{Name: "Secrets", Checks: d.checkCredentials(t.Context())}}}
	c, ok := findCheck(rep, "secret.asc.auth")
	if !ok {
		t.Fatal("the auth check did not run")
	}
	if c.Level != levelSkip {
		t.Fatalf("level = %v, want skip (detail %q)", c.Level, c.Detail)
	}
}

// Every resolved value is zeroed, including the key. §15 keeps secrets as
// []byte for exactly this reason (F10), and a check that resolved three of
// them and left them in the heap would be spending the invariant for a
// diagnostic.
func TestDoctorCredentials_ValuesAreZeroed(t *testing.T) {
	_, p8 := testKey(t)
	held := map[string]secrets.Value{}
	fake := &fakeSecrets{values: fullSet(p8)}

	d := appStoreDoctor(t, nil, nil)
	d.newSecrets = func(secrets.Options) (secretResolver, error) {
		return &recordingSecrets{inner: fake, held: held}, nil
	}
	_ = d.checkCredentials(t.Context())

	if len(held) != 3 {
		t.Fatalf("resolved %d values, want 3", len(held))
	}
	for name, v := range held {
		for i, b := range v {
			if b != 0 {
				t.Fatalf("%s was not zeroed (byte %d = %q)", name, i, b)
			}
		}
	}
}

// recordingSecrets keeps the slices it handed out, so a test can look at them
// after the check has finished with them.
type recordingSecrets struct {
	inner *fakeSecrets
	held  map[string]secrets.Value
}

func (r *recordingSecrets) Resolve(ctx context.Context, name string) (secrets.Value, error) {
	v, err := r.inner.Resolve(ctx, name)
	if err == nil && len(v) > 0 {
		r.held[name] = v
	}
	return v, err
}

func (r *recordingSecrets) Providers() []string { return r.inner.Providers() }

// tokenPayload decodes the claims out of the bearer token on a request.
func tokenPayload(r *http.Request) string {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return string(raw)
}

// assertNoKeyMaterial is INV-4 applied to a diagnostic. `doctor` prints these
// checks to a terminal and --fix collects them into a list an operator pastes
// into a ticket.
func assertNoKeyMaterial(t *testing.T, rep report, p8 string) {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(p8, "\n") {
		if line = strings.TrimSpace(line); len(line) > 12 && !strings.HasPrefix(line, "-----") {
			lines = append(lines, line)
		}
	}
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			blob := strings.Join(append(append([]string{c.Detail}, c.Fix...), c.Notes...), "\n")
			for _, line := range lines {
				if strings.Contains(blob, line) {
					t.Errorf("check %s carries key material", c.ID)
				}
			}
		}
	}
}
