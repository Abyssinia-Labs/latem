// Package appstore checks that an App Store Connect credential set agrees with
// itself (RUNNER_SPEC §19, §20, amendment A35).
//
// # Why this is not a presence check
//
// Every value in an App Store Connect credential can be individually valid
// while the set is unusable. The JWT carries a `kid` naming one key and a
// signature made by another; Apple answers 401 and names neither. Verified on
// 2026-08-11: four `AuthKey_*.p8` files on one laptop, all 257 bytes, all
// parsing, one of them the right one.
//
// `fastlane pilot upload` reports that, a wrong issuer id, and a revoked key
// identically — and only after archiving, exporting and uploading, which is a
// two-minute round trip per guess against three independent values. So the
// question this package answers is not "is each variable set" but "do these
// three describe the same key, and does Apple agree".
//
// # What it will not do
//
// Print key material, or put any of it in an error. Every diagnostic here is
// built from the key id, the issuer id, a file path, or a fingerprint of the
// *public* key — INV-4 applies to a diagnostic as much as to a log, and a
// `doctor` that leaked a signing key while checking it would be worse than no
// check at all.
package appstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SecretKeyID, SecretIssuerID and SecretP8 are the declared secret names §18.1
// uses, and the names `examples/ios-native/ci/` reads.
//
// gosec reads these as hardcoded credentials. They are the opposite: the names
// a value is resolved *by*, which is the whole of INV-2 — the agent is handed
// names and never definitions, and §15 goes to the provider chain for the
// value.
const (
	SecretKeyID    = "APP_STORE_CONNECT_KEY_ID"    //nolint:gosec // a secret's name, not a secret
	SecretIssuerID = "APP_STORE_CONNECT_ISSUER_ID" //nolint:gosec // a secret's name, not a secret
	SecretP8       = "APP_STORE_CONNECT_KEY_P8"    //nolint:gosec // a secret's name, not a secret
)

// DefaultBaseURL is App Store Connect's API root.
const DefaultBaseURL = "https://api.appstoreconnect.apple.com"

// TokenLifetime is how long a minted token claims to be valid. Apple's ceiling
// is 20 minutes; 15 leaves room for a clock a little behind Apple's without
// producing a token that is already expired when it arrives.
const TokenLifetime = 15 * time.Minute

var (
	// A key id is ten uppercase alphanumerics — it is the XXXXXXXXXX in the
	// AuthKey_XXXXXXXXXX.p8 Apple hands you.
	keyIDPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	// The issuer id is a UUID. The common mistake is pasting the key id here,
	// or a team id, or an App ID — none of which are UUIDs, which is what
	// makes this worth checking rather than assuming.
	issuerPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// Apple names every download AuthKey_<KEY_ID>.p8, so the filename is the
	// key id. That is the whole basis of [ScanKeyFiles].
	keyFilePattern = regexp.MustCompile(`^AuthKey_([A-Za-z0-9]+)\.p8$`)
)

// Credentials is the trio a TestFlight upload authenticates with.
//
// P8 is the contents of the `.p8`, not a path: §15 resolves secrets to bytes
// and nothing writes them to disk.
type Credentials struct {
	KeyID    string
	IssuerID string
	P8       []byte
}

// ValidKeyID reports whether s has the shape Apple issues.
func ValidKeyID(s string) bool { return keyIDPattern.MatchString(s) }

// ValidIssuerID reports whether s is a UUID.
func ValidIssuerID(s string) bool { return issuerPattern.MatchString(s) }

// ParseKey decodes a PKCS#8 EC P-256 private key from the contents of a `.p8`.
//
// The returned error never contains any part of the input. A parse failure here
// is reported to an operator, and "could not parse <the key material>" is a
// leak with a helpful tone of voice.
func ParseKey(p8 []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("not PEM: expected a block beginning -----BEGIN PRIVATE KEY-----")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("PEM block is %q, want PRIVATE KEY (a PKCS#8 .p8)", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// x509's message describes structure, not content.
		return nil, fmt.Errorf("not a PKCS#8 key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want an EC key", parsed)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("curve is %s, want P-256", key.Curve.Params().Name)
	}
	return key, nil
}

// Fingerprint identifies a key by its public half.
//
// SHA-256 over the uncompressed point, truncated to sixteen hex characters,
// matching `examples/ios-native/ci/ios/check-credentials.sh` so an operator
// comparing the two sees the same string. It is derived from public material
// only: printing it discloses nothing a signed token would not.
func Fingerprint(pub *ecdsa.PublicKey) string {
	point := elliptic.Marshal(pub.Curve, pub.X, pub.Y) //nolint:staticcheck // the uncompressed form is the wire format the script hashes
	sum := sha256.Sum256(point)
	return hex.EncodeToString(sum[:])[:16]
}

// KeyFile is one `AuthKey_*.p8` found on disk.
type KeyFile struct {
	// KeyID comes from the filename, which is Apple's own naming.
	KeyID string
	// Fingerprint is [Fingerprint] of the key inside the file.
	Fingerprint string
	Path        string
}

// DefaultKeyDirs are the directories an `AuthKey_*.p8` is normally left in.
//
// Downloads first because that is where it lands, and the two private_keys
// directories because that is where Apple's own tooling looks for it.
func DefaultKeyDirs(home string) []string {
	return []string{
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, ".appstoreconnect", "private_keys"),
		filepath.Join(home, ".private_keys"),
	}
}

// ScanKeyFiles fingerprints every readable AuthKey_*.p8 under dirs.
//
// Unreadable and unparseable files are skipped rather than reported: this
// exists to explain a mismatch, and a stranger's key in ~/Downloads that does
// not parse is not a fault of the machine. Results are sorted by key id so a
// diagnostic is stable between runs.
//
// Depth is bounded at two levels below each directory. A full walk of
// ~/Documents on a working machine is minutes of IO for a check that has to
// answer in a second, and Apple's downloads do not arrive nested deeply.
func ScanKeyFiles(dirs []string) []KeyFile {
	var out []KeyFile
	seen := map[string]bool{}
	for _, dir := range dirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return nil // a directory that is simply not there
				}
				return fs.SkipDir
			}
			if d.IsDir() {
				if depthBelow(root, path) > 2 {
					return fs.SkipDir
				}
				return nil
			}
			m := keyFilePattern.FindStringSubmatch(d.Name())
			if m == nil || seen[path] {
				return nil
			}
			seen[path] = true
			data, err := readCapped(path, 8<<10)
			if err != nil {
				return nil
			}
			key, err := ParseKey(data)
			if err != nil {
				return nil
			}
			out = append(out, KeyFile{KeyID: m[1], Fingerprint: Fingerprint(&key.PublicKey), Path: path})
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].KeyID != out[j].KeyID {
			return out[i].KeyID < out[j].KeyID
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func depthBelow(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

// readCapped reads at most limit bytes. A `.p8` is 257 bytes; anything claiming
// to be one and arriving in megabytes is not one, and reading it would be the
// check doing damage rather than finding it.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the path came from a directory walk of the operator's own home
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// MatchOnDisk finds the file the given key material came from, if it is here.
//
// Returns the matching file and whether its key id agrees with creds.KeyID. A
// disagreement is the A35 defect: the token would claim `kid` of one key while
// being signed by another.
func MatchOnDisk(creds Credentials, files []KeyFile) (KeyFile, bool, bool) {
	key, err := ParseKey(creds.P8)
	if err != nil {
		return KeyFile{}, false, false
	}
	want := Fingerprint(&key.PublicKey)
	for _, f := range files {
		if f.Fingerprint == want {
			return f, true, f.KeyID == creds.KeyID
		}
	}
	return KeyFile{}, false, false
}

// TokenForm is which of Apple's two key kinds a token is minted for.
//
// They are created on the same page, both hand you an AuthKey_XXXXXXXXXX.p8,
// both display an issuer id — and the payload differs. A team key carries
// `iss`; an individual key omits it and carries `sub: "user"`. Using the wrong
// shape returns the same 401 a wrong key does, which is why both are tried and
// Apple is allowed to say which this is.
type TokenForm string

const (
	// TeamKey is the form fastlane's spaceship builds, and the only one it can
	// use. An individual key therefore fails there however correct it is.
	TeamKey TokenForm = "team key"
	// IndividualKey authenticates, and then cannot upload through fastlane.
	// Worth distinguishing precisely because it half works.
	IndividualKey TokenForm = "individual key"
)

// Token mints an ES256 JWT for one form.
//
// now is a parameter so a test is not at the mercy of the clock, and because a
// caller that has already measured skew against Apple can compensate.
func Token(creds Credentials, form TokenForm, now time.Time) (string, error) {
	key, err := ParseKey(creds.P8)
	if err != nil {
		return "", err
	}

	header := map[string]string{"alg": "ES256", "kid": creds.KeyID, "typ": "JWT"}
	payload := map[string]any{
		"iat": now.Unix(),
		"exp": now.Add(TokenLifetime).Unix(),
		"aud": "appstoreconnect-v1",
	}
	switch form {
	case TeamKey:
		payload["iss"] = creds.IssuerID
	case IndividualKey:
		payload["sub"] = "user"
	default:
		return "", fmt.Errorf("unknown token form %q", form)
	}

	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64(h) + "." + b64(p)

	// JWS wants the raw r||s form, 32 bytes each, left-padded. ecdsa.SignASN1
	// returns DER, which Apple rejects with the same 401 as a wrong key — a
	// failure that looks like a credential problem and is a serialisation one.
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := append(pad32(r), pad32(s)...)
	return signingInput + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(v *big.Int) []byte {
	out := make([]byte, 32)
	v.FillBytes(out)
	return out
}

// Client talks to the App Store Connect API.
//
// BaseURL and HTTP are both injectable because every test here runs against a
// local server: a package whose only coverage came from a real Apple credential
// would be a package with no coverage.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// AppsResult is what a read-only probe learned.
type AppsResult struct {
	// Status is the HTTP status. 200 authenticated, 401 Apple rejected the
	// token, 403 authenticated without authorisation.
	Status int
	// BundleIDs are the apps this key can see, which is what decides whether
	// an upload will work after authentication succeeds.
	BundleIDs []string
}

// Apps lists the apps a token can see.
//
// GET only, and the read-only endpoint on purpose: this runs from `doctor`,
// against the operator's own credential, and must not be able to change
// anything in App Store Connect however wrong the credential turns out to be.
func (c *Client) Apps(ctx context.Context, token string) (AppsResult, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/apps?limit=200", nil)
	if err != nil {
		return AppsResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return AppsResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	out := AppsResult{Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		return out, nil
	}

	var body struct {
		Data []struct {
			Attributes struct {
				BundleID string `json:"bundleId"`
				Name     string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return out, fmt.Errorf("decode /v1/apps: %w", err)
	}
	for _, app := range body.Data {
		if app.Attributes.BundleID != "" {
			out.BundleIDs = append(out.BundleIDs, app.Attributes.BundleID)
		}
	}
	sort.Strings(out.BundleIDs)
	return out, nil
}
