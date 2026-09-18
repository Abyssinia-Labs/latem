package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// A release, assembled and signed in-process.
//
// The signature here is made by a key generated in the test rather than by
// minisign, which is only safe because minisign_test.go proves the verifier
// agrees with the real tool on a fixture the real tool produced. These tests
// are about the chain around the signature — checksums, names, versions — and
// a key they can control is what lets them break one link at a time.
type release struct {
	version string
	bare    string
	key     PublicKey
	priv    ed25519.PrivateKey
	files   map[string][]byte
}

func newRelease(t *testing.T, version string) *release {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{
		version: version,
		bare:    strings.TrimPrefix(version, "v"),
		priv:    priv,
		files:   map[string][]byte{},
	}
	r.key.key = pub
	for i := range r.key.id {
		r.key.id[i] = byte(i + 1)
	}
	return r
}

// addBinary puts a `latem` archive for one platform into the release. The
// binary is a shell script that answers `version --short`, because that is the
// only thing [Updater.Fetch] asks of it.
func (r *release) addBinary(t *testing.T, goos, goarch, reports string) {
	t.Helper()
	script := "#!/bin/sh\necho " + reports + "\n"
	r.files[AssetName(r.bare, goos, goarch)] = tarGz(t, "latem", []byte(script))
}

// addRaw puts an arbitrary asset into the release, for the cases where the
// archive itself is the thing under test.
func (r *release) addRaw(name string, body []byte) { r.files[name] = body }

// sign builds the checksums file over everything added so far and signs it,
// the way the release workflow does.
func (r *release) sign(t *testing.T, trustedComment string) {
	t.Helper()
	var b strings.Builder
	for name, body := range r.files {
		if strings.HasSuffix(name, "checksums.txt") || strings.HasSuffix(name, ".minisig") {
			continue
		}
		sum := sha256.Sum256(body)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	sums := []byte(b.String())

	name := ChecksumsName(r.bare)
	r.files[name] = sums
	r.files[name+".minisig"] = []byte(minisigFor(r.priv, r.key.id, sums, trustedComment))
}

// serve stands up the feed and the download base.
func (r *release) serve(t *testing.T) (feedURL, base string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/feed" {
			fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, r.version)
			return
		}
		name := filepath.Base(req.URL.Path)
		body, ok := r.files[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/feed", srv.URL + "/download"
}

func (r *release) updater(t *testing.T, current string) *Updater {
	t.Helper()
	feed, base := r.serve(t)
	return &Updater{
		Current:      current,
		Key:          r.key,
		FeedURL:      feed,
		DownloadBase: base,
		GOOS:         "linux",
		GOARCH:       "amd64",
	}
}

func minisigFor(priv ed25519.PrivateKey, id [keyIDLen]byte, content []byte, trusted string) string {
	sum := blake2b.Sum512(content)
	sig := ed25519.Sign(priv, sum[:])
	global := ed25519.Sign(priv, append(append([]byte{}, sig...), []byte(trusted)...))

	enc := func(parts ...[]byte) string {
		var all []byte
		for _, p := range parts {
			all = append(all, p...)
		}
		return base64.StdEncoding.EncodeToString(all)
	}
	return "untrusted comment: test\n" +
		enc([]byte(algPrehash), id[:], sig) + "\n" +
		"trusted comment: " + trusted + "\n" +
		enc(global) + "\n"
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, zw} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- Check

func TestCheck(t *testing.T) {
	r := newRelease(t, "v0.3.0")

	t.Run("a newer release", func(t *testing.T) {
		rel, err := r.updater(t, "v0.2.9").Check(context.Background())
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if rel.Version != "v0.3.0" || rel.Bare != "0.3.0" {
			t.Errorf("Check = %+v, want v0.3.0 / 0.3.0", rel)
		}
	})

	// The two cases that must not update, and the reason they are separate
	// assertions: an equal version is the ordinary daily answer, and an older
	// one is somebody serving a genuinely signed downgrade.
	for name, current := range map[string]string{
		"the same release": "v0.3.0",
		"a newer binary":   "v0.4.0",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.updater(t, current).Check(context.Background())
			if !errors.Is(err, ErrUpToDate) {
				t.Fatalf("Check on %s = %v, want ErrUpToDate", current, err)
			}
		})
	}

	t.Run("a dev build", func(t *testing.T) {
		_, err := r.updater(t, DevVersion).Check(context.Background())
		if err == nil {
			t.Fatal("a dev build was offered an update")
		}
		if errors.Is(err, ErrUpToDate) {
			t.Fatalf("a dev build was reported up to date, which it cannot know: %v", err)
		}
	})

	t.Run("no trusted key", func(t *testing.T) {
		u := r.updater(t, "v0.2.0")
		u.Key = PublicKey{}
		if _, err := u.Check(context.Background()); !errors.Is(err, ErrNoTrustedKey) {
			t.Fatalf("Check with no key = %v, want ErrNoTrustedKey", err)
		}
	})
}

func TestCheck_RefusesAFeedItCannotBelieve(t *testing.T) {
	for name, body := range map[string]string{
		"a draft":                `{"tag_name":"v9.0.0","draft":true,"prerelease":false}`,
		"a prerelease":           `{"tag_name":"v9.0.0","draft":false,"prerelease":true}`,
		"no tag":                 `{"draft":false,"prerelease":false}`,
		"a branch name as a tag": `{"tag_name":"main","draft":false,"prerelease":false}`,
		"not json":               `<html>rate limited</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			u := &Updater{Current: "v0.1.0", Key: newRelease(t, "v0.1.0").key, FeedURL: srv.URL}
			if _, err := u.Check(context.Background()); err == nil {
				t.Fatalf("%s was accepted as a release", name)
			}
		})
	}

	t.Run("an http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limited", http.StatusForbidden)
		}))
		defer srv.Close()

		u := &Updater{Current: "v0.1.0", Key: newRelease(t, "v0.1.0").key, FeedURL: srv.URL}
		_, err := u.Check(context.Background())
		if err == nil {
			t.Fatal("a 403 was accepted")
		}
		// api.github.com rate-limits by IP, and a machine behind a shared NAT
		// will meet this. The status has to be in the message or the daily log
		// line says nothing an operator can act on.
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("the error does not carry the status: %v", err)
		}
	})
}

// ---------------------------------------------------------------- Fetch

func TestFetch_VerifiesAndStages(t *testing.T) {
	r := newRelease(t, "v0.3.0")
	r.addBinary(t, "linux", "amd64", "v0.3.0")
	r.addBinary(t, "darwin", "arm64", "v0.3.0")
	r.sign(t, "latem v0.3.0")

	u := r.updater(t, "v0.2.0")
	rel, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path, err := u.Fetch(context.Background(), rel, dir)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := filepath.Dir(path); got != dir {
		t.Errorf("staged in %s, want %s", got, dir)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the staged binary is not executable: %v", fi.Mode())
	}
	// It staged, and it replaced nothing: Install is a separate step precisely
	// so a failed verification cannot leave a machine half-updated.
	if _, err := os.Stat(filepath.Join(dir, "latem")); err == nil {
		t.Error("Fetch installed the binary rather than staging it")
	}
}

func TestFetch_Refusals(t *testing.T) {
	// Each case breaks exactly one link in the chain the package comment
	// describes, and every one of them must stop before anything is staged.
	cases := map[string]struct {
		build func(t *testing.T, r *release)
		says  string
	}{
		"an archive that does not match its signed checksum": {
			build: func(t *testing.T, r *release) {
				r.addBinary(t, "linux", "amd64", "v0.3.0")
				r.sign(t, "latem v0.3.0")
				// Substituted after signing: the checksums file is authentic
				// and the archive is not.
				r.addRaw(AssetName(r.bare, "linux", "amd64"), tarGz(t, "latem", []byte("#!/bin/sh\necho v0.3.0\n# extra")))
			},
			says: "signed checksum",
		},
		"a checksums file signed by another key": {
			build: func(t *testing.T, r *release) {
				r.addBinary(t, "linux", "amd64", "v0.3.0")
				r.sign(t, "latem v0.3.0")
				other := newRelease(t, r.version)
				name := ChecksumsName(r.bare)
				r.addRaw(name+".minisig", []byte(minisigFor(other.priv, other.key.id, r.files[name], "latem v0.3.0")))
			},
			says: "trusts",
		},
		"a checksums file with a rewritten line": {
			build: func(t *testing.T, r *release) {
				r.addBinary(t, "linux", "amd64", "v0.3.0")
				r.sign(t, "latem v0.3.0")
				name := ChecksumsName(r.bare)
				r.addRaw(name, append(r.files[name], []byte("0000  extra\n")...))
			},
			says: "not signed by the key",
		},
		// The relabelling defence from the package comment: v0.1.0's real,
		// signed checksums served under a tag claiming v0.3.0. Nothing is
		// forged; the asset this platform needs simply is not in the file.
		"a genuine release relabelled as a newer one": {
			build: func(t *testing.T, r *release) {
				old := newRelease(t, "v0.1.0")
				old.key, old.priv = r.key, r.priv
				old.addBinary(t, "linux", "amd64", "v0.1.0")
				old.sign(t, "latem v0.1.0")
				// Published under v0.3.0's names, contents untouched.
				r.addRaw(ChecksumsName(r.bare), old.files[ChecksumsName("0.1.0")])
				r.addRaw(ChecksumsName(r.bare)+".minisig", old.files[ChecksumsName("0.1.0")+".minisig"])
				r.addRaw(AssetName(r.bare, "linux", "amd64"), old.files[AssetName("0.1.0", "linux", "amd64")])
			},
			says: "not listed in the signed checksums",
		},
		"a release with no build for this platform": {
			build: func(t *testing.T, r *release) {
				r.addBinary(t, "darwin", "arm64", "v0.3.0")
				r.sign(t, "latem v0.3.0")
			},
			says: "not listed in the signed checksums",
		},
		"an archive with no latem in it": {
			build: func(t *testing.T, r *release) {
				r.addRaw(AssetName(r.bare, "linux", "amd64"), tarGz(t, "README", []byte("nothing here")))
				r.sign(t, "latem v0.3.0")
			},
			says: "no `latem` binary",
		},
		"an archive that is not an archive": {
			build: func(t *testing.T, r *release) {
				r.addRaw(AssetName(r.bare, "linux", "amd64"), []byte("404: Not Found"))
				r.sign(t, "latem v0.3.0")
			},
			says: "not gzip",
		},
		// A correctly signed release whose binary is from another tag. The
		// signature is honest and the build is wrong, which no amount of
		// cryptography detects — only asking the binary does.
		"a binary that reports another version": {
			build: func(t *testing.T, r *release) {
				r.addBinary(t, "linux", "amd64", "v0.2.0")
				r.sign(t, "latem v0.3.0")
			},
			says: "reports version",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, "v0.3.0")
			tc.build(t, r)

			u := r.updater(t, "v0.2.0")
			dir := t.TempDir()
			_, err := u.Fetch(context.Background(), Release{Version: r.version, Bare: r.bare}, dir)
			if err == nil {
				t.Fatal("this release was accepted")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the error does not say %q: %v", tc.says, err)
			}
			// Nothing left behind for a later run to trip over.
			entries, rerr := os.ReadDir(dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 0 {
				t.Errorf("a refused release left %d file(s) staged: %v", len(entries), entries)
			}
		})
	}
}

func TestFetch_MissingAssetsAreNamed(t *testing.T) {
	r := newRelease(t, "v0.3.0")
	// Nothing published at all: the 404 has to name the file, because the
	// operator's next step is to look at the release page for it.
	u := r.updater(t, "v0.2.0")
	_, err := u.Fetch(context.Background(), Release{Version: r.version, Bare: r.bare}, t.TempDir())
	if err == nil {
		t.Fatal("an empty release was accepted")
	}
	if !strings.Contains(err.Error(), ChecksumsName("0.3.0")) {
		t.Errorf("the error does not name the missing asset: %v", err)
	}
}

// ---------------------------------------------------------------- Install

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "latem")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".latem.new")
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Install(staged, target); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "new" {
		t.Errorf("target = %q, want the staged binary", body)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged file survived the rename")
	}
	// An operator who narrowed the mode meant it, and a release should not
	// widen it back.
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %v, want the 0700 the old binary had", got)
	}
}

func TestInstall_RefusesToCrossDirectories(t *testing.T) {
	// The rename is the atomic step, and a rename cannot cross a filesystem.
	// Accepting this call would silently turn the guarantee into a copy.
	a, b := t.TempDir(), t.TempDir()
	staged := filepath.Join(a, ".latem.new")
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(staged, filepath.Join(b, "latem")); err == nil {
		t.Fatal("Install accepted a staged file in another directory")
	}
}

// ---------------------------------------------------------------- versions

func TestCompareVersions(t *testing.T) {
	newer := [][2]string{
		{"v0.2.0", "v0.1.9"},
		{"v1.0.0", "v0.99.99"},
		{"0.1.1", "v0.1.0"},
		{"v0.1.10", "v0.1.9"},
		// Semver: a prerelease precedes its release, so an agent on an rc
		// updates to the real thing.
		{"v0.2.0", "v0.2.0-rc.1"},
		{"v0.2.0-rc.2", "v0.2.0-rc.1"},
	}
	for _, p := range newer {
		got, err := compareVersions(p[0], p[1])
		if err != nil {
			t.Fatalf("compareVersions(%s, %s): %v", p[0], p[1], err)
		}
		if got != 1 {
			t.Errorf("compareVersions(%s, %s) = %d, want 1", p[0], p[1], got)
		}
		if back, _ := compareVersions(p[1], p[0]); back != -1 {
			t.Errorf("compareVersions(%s, %s) = %d, want -1", p[1], p[0], back)
		}
	}

	if got, err := compareVersions("v0.1.0", "0.1.0"); err != nil || got != 0 {
		t.Errorf("compareVersions ignoring the v prefix = %d, %v", got, err)
	}

	// Refused rather than guessed: every one of these would otherwise compare
	// as something, and the consequence is a binary replaced on a machine
	// nobody is watching.
	for _, s := range []string{"", "dev", "main", "v1", "v1.2", "v1.2.3.4", "v1.x.0", "latest", "v-1.0.0"} {
		if _, err := parseVersion(s); err == nil {
			t.Errorf("parseVersion(%q) was accepted", s)
		}
	}
}
