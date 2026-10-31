// Package playstore reads what Google Play says about an app (A65).
//
// # What it will not do
//
// Open an edit. Almost everything in the Play Developer API happens inside an
// edit, and a new edit invalidates the same user's open one. A check that opened
// one could kill a running `fastlane supply` upload, so this package reads
// through applications.tracks.releases.list, which has no edit in its path, and
// has no other call. It lists a track and changes nothing.
//
// Put key material in an error. As in internal/appstore, every diagnostic is
// built from the service account's address, the key's id, or Google's own
// words, and a service account key that does not parse says only that it does
// not parse.
package playstore

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SecretServiceAccount is the name jobs read the service account key by, as in
// docs/android.md.
const SecretServiceAccount = "PLAY_SERVICE_ACCOUNT_JSON" //nolint:gosec // a secret's name, not a secret

const (
	// DefaultBaseURL is the Play Developer API's root.
	DefaultBaseURL = "https://androidpublisher.googleapis.com"
	// DefaultTokenURL is where a service account trades a signed assertion for
	// an access token. Google requires this exact value as the assertion's aud.
	// A key file's token_uri is not read: the assertion goes here and nowhere
	// a file names.
	DefaultTokenURL = "https://oauth2.googleapis.com/token" //nolint:gosec // an address, not a credential

	scope = "https://www.googleapis.com/auth/androidpublisher"
	// tokenLifetime covers one check. Google's ceiling is an hour.
	tokenLifetime = 10 * time.Minute
)

// ServiceAccount is a parsed key.
type ServiceAccount struct {
	// Email is the address invited in Play Console.
	Email string
	// KeyID names the key in Google Cloud's list of them.
	KeyID string

	key *rsa.PrivateKey
}

// ParseServiceAccount reads the JSON Google Cloud hands out.
//
// No error here wraps the JSON decoder's, which quotes the input.
func ParseServiceAccount(data []byte) (*ServiceAccount, error) {
	var file struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, errors.New("not JSON: it should be the key file Google Cloud downloaded, whole")
	}
	if file.Type != "service_account" {
		return nil, fmt.Errorf("its type is %q, want service_account", file.Type)
	}
	if file.ClientEmail == "" {
		return nil, errors.New("it has no client_email")
	}
	block, _ := pem.Decode([]byte(file.PrivateKey))
	if block == nil {
		return nil, errors.New("its private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("its private_key is not a PKCS#8 key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("its private_key is %T, want an RSA key", parsed)
	}
	return &ServiceAccount{Email: file.ClientEmail, KeyID: file.PrivateKeyID, key: key}, nil
}

// assertion signs the JWT Google trades for an access token.
func (s *ServiceAccount) assertion(now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.KeyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   s.Email,
		"scope": scope,
		"aud":   DefaultTokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(tokenLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	input := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Error is Google's refusal, in Google's words.
type Error struct {
	Status int
	// Code is invalid_grant from the token endpoint, or a status such as
	// PERMISSION_DENIED from the API.
	Code    string
	Message string
}

func (e *Error) Error() string {
	switch {
	case e.Message != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	case e.Code != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Code)
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// Release is a release on a track. Play gives it no timestamp.
type Release struct {
	Name         string
	State        string
	VersionCodes []int
}

// Published reports a release testers can have.
func (r Release) Published() bool { return r.State == "RELEASE_LIFECYCLE_STATE_PUBLISHED" }

// Client talks to the Play Developer API. Both URLs are injectable because
// every test here runs against a local server.
type Client struct {
	BaseURL  string
	TokenURL string
	HTTP     *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Token trades a service account's signed assertion for an access token.
//
// This POST is the one request here that is not a GET. It goes to Google's
// token endpoint and changes nothing in Play.
func (c *Client) Token(ctx context.Context, s *ServiceAccount, now time.Time) (string, error) {
	assertion, err := s.assertion(now)
	if err != nil {
		return "", err
	}
	endpoint := c.TokenURL
	if endpoint == "" {
		endpoint = DefaultTokenURL
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Status: resp.StatusCode, Code: body.Error, Message: body.Description}
	}
	if decodeErr != nil || body.AccessToken == "" {
		return "", &Error{Status: resp.StatusCode, Message: "the token endpoint answered with no token"}
	}
	return body.AccessToken, nil
}

// Releases lists a track's releases, newest state first as Google orders them.
//
// applications.tracks.releases.list: no edit in its path, so it cannot disturb
// an upload running under the same service account.
func (c *Client) Releases(ctx context.Context, token, packageName, track string) ([]Release, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/tracks/%s/releases",
		base, url.PathEscape(packageName), url.PathEscape(track))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		var doc struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(body).Decode(&doc)
		return nil, &Error{Status: resp.StatusCode, Code: doc.Error.Status, Message: doc.Error.Message}
	}

	var doc struct {
		Releases []struct {
			ReleaseName     string `json:"releaseName"`
			LifecycleState  string `json:"releaseLifecycleState"`
			ActiveArtifacts []struct {
				VersionCode int `json:"versionCode"`
			} `json:"activeArtifacts"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode the releases of track %s: %w", track, err)
	}
	out := make([]Release, 0, len(doc.Releases))
	for _, r := range doc.Releases {
		rel := Release{Name: r.ReleaseName, State: r.LifecycleState}
		for _, a := range r.ActiveArtifacts {
			rel.VersionCodes = append(rel.VersionCodes, a.VersionCode)
		}
		out = append(out, rel)
	}
	return out, nil
}
