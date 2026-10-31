package playstore

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAccount(t *testing.T, extra map[string]string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	file := map[string]string{
		"type": "service_account", "client_email": "ci@example.iam.gserviceaccount.com",
		"private_key_id": "kid123",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}
	for k, v := range extra {
		file[k] = v
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	return data, key
}

func TestParseServiceAccount_ReadsTheFileGoogleHandsOut(t *testing.T) {
	data, _ := testAccount(t, nil)
	s, err := ParseServiceAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "ci@example.iam.gserviceaccount.com" || s.KeyID != "kid123" {
		t.Errorf("%+v", s)
	}
}

// The decoder quotes what it trips on, and what it would be quoting here is a
// private key. Each of these is a real way to damage this secret.
func TestParseServiceAccount_NoErrorQuotesItsInput(t *testing.T) {
	const sentinel = "SENTINEL-KEY-MATERIAL"
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	for name, data := range map[string]string{
		"not json":   sentinel,
		"truncated":  `{"type":"service_account","private_key":"` + sentinel,
		"wrong type": `{"type":"authorized_user","client_secret":"` + sentinel + `"}`,
		"no address": `{"type":"service_account","private_key":"` + sentinel + `"}`,
		"not pem":    `{"type":"service_account","client_email":"a@b","private_key":"` + sentinel + `"}`,
		"not a key": `{"type":"service_account","client_email":"a@b","private_key":` +
			mustJSON(t, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte(sentinel)}))) + `}`,
		"not rsa": `{"type":"service_account","client_email":"a@b","private_key":` + mustJSON(t, ecPEM) + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServiceAccount([]byte(data))
			if err == nil {
				t.Fatal("should not parse")
			}
			if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "SENTINEL") {
				t.Fatalf("the error quoted the key: %v", err)
			}
		})
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestToken_SignsWhatGoogleAsksFor(t *testing.T) {
	data, key := testAccount(t, nil)
	s, err := ParseServiceAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)

	var form map[string][]string
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		contentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		form = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":600}`))
	}))
	defer srv.Close()

	c := &Client{TokenURL: srv.URL, HTTP: srv.Client()}
	token, err := c.Token(t.Context(), s, now)
	if err != nil {
		t.Fatal(err)
	}
	if token != "tok" {
		t.Errorf("token = %q", token)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if got := form["grant_type"]; len(got) != 1 || got[0] != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("grant_type = %v", got)
	}

	parts := strings.Split(form["assertion"][0], ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the signature does not verify: %v", err)
	}

	var header map[string]string
	decodePart(t, parts[0], &header)
	if header["alg"] != "RS256" || header["kid"] != "kid123" {
		t.Errorf("header: %v", header)
	}
	var claims map[string]any
	decodePart(t, parts[1], &claims)
	if claims["iss"] != "ci@example.iam.gserviceaccount.com" ||
		claims["aud"] != "https://oauth2.googleapis.com/token" ||
		claims["scope"] != "https://www.googleapis.com/auth/androidpublisher" {
		t.Errorf("claims: %v", claims)
	}
	if iat, exp := claims["iat"].(float64), claims["exp"].(float64); iat != float64(now.Unix()) || exp-iat > 3600 || exp <= iat {
		t.Errorf("iat=%v exp=%v", iat, exp)
	}
}

func decodePart(t *testing.T, part string, out any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

// A key file is a file. The signed assertion goes to Google, whatever it says.
func TestToken_AKeyFilesTokenURIIsNotFollowed(t *testing.T) {
	var called bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer elsewhere.Close()
	data, _ := testAccount(t, map[string]string{"token_uri": elsewhere.URL})
	s, err := ParseServiceAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok"}`))
	}))
	defer srv.Close()
	c := &Client{TokenURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Token(t.Context(), s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the assertion went where the file said")
	}
}

func TestToken_ARefusalKeepsGooglesWordsAndNoToken(t *testing.T) {
	data, _ := testAccount(t, nil)
	s, err := ParseServiceAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`))
	}))
	defer srv.Close()
	c := &Client{TokenURL: srv.URL, HTTP: srv.Client()}
	_, err = c.Token(t.Context(), s, time.Now())
	var e *Error
	if !errors.As(err, &e) || e.Code != "invalid_grant" || !strings.Contains(e.Error(), "Invalid JWT Signature") {
		t.Fatalf("got %v", err)
	}
}

func TestToken_AnAnswerWithNoTokenIsAnError(t *testing.T) {
	data, _ := testAccount(t, nil)
	s, err := ParseServiceAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{TokenURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Token(t.Context(), s, time.Now()); err == nil {
		t.Fatal("no token should be an error")
	}
}

func TestReleases_ReadsATrackWithNoEdit(t *testing.T) {
	var method, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"releases":[{"track":"internal","releaseName":"401 (0.1.0)",
			"releaseLifecycleState":"RELEASE_LIFECYCLE_STATE_PUBLISHED",
			"activeArtifacts":[{"versionCode":401},{"versionCode":402}]}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.Releases(t.Context(), "tok", "com.example.app", "internal")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || auth != "Bearer tok" {
		t.Errorf("%s, %q", method, auth)
	}
	if path != "/androidpublisher/v3/applications/com.example.app/tracks/internal/releases" {
		t.Errorf("path = %q", path)
	}
	if strings.Contains(path, "edits") {
		t.Error("this call exists to stay out of an edit")
	}
	if len(got) != 1 || !got[0].Published() || len(got[0].VersionCodes) != 2 || got[0].VersionCodes[1] != 402 {
		t.Errorf("%+v", got)
	}
}

func TestReleases_AnEmptyTrackIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.Releases(t.Context(), "tok", "com.example.app", "internal")
	if err != nil || len(got) != 0 {
		t.Errorf("%+v, %v", got, err)
	}
}

func TestReleases_ARefusalKeepsGooglesWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.Releases(t.Context(), "tok", "com.example.app", "internal")
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || e.Code != "PERMISSION_DENIED" ||
		!strings.Contains(e.Error(), "does not have permission") {
		t.Fatalf("got %v", err)
	}
}

func TestError_SaysWhatItHas(t *testing.T) {
	for _, tc := range []struct {
		e    Error
		want string
	}{
		{Error{Status: 400, Code: "invalid_grant", Message: "no"}, "HTTP 400: no"},
		{Error{Status: 400, Code: "invalid_grant"}, "HTTP 400: invalid_grant"},
		{Error{Status: 502}, "HTTP 502"},
	} {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
