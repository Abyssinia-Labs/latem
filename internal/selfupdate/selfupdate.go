// Package selfupdate implements RUNNER_SPEC §20's self-update: check a release
// feed daily, verify a minisign signature, download to a temp path, atomic
// rename, and let the caller re-exec when no run is active.
//
// # The trust chain, and where it is anchored
//
// One signature covers every platform, because what gets signed is the
// checksums file rather than the binaries:
//
//	trusted.pub (embedded)  signs  latem_<v>_checksums.txt
//	                               which lists sha256 of every archive
//	                               one of which is this machine's
//
// So the release feed does not have to be trusted. It supplies a version
// string and nothing else; every byte that ends up on disk is covered by a
// signature made by a key that is compiled into the running binary. A hostile
// or compromised feed can refuse to answer, or answer with a version this key
// signed — and no more than that.
//
// Two attacks the chain above does not stop on its own, and what does:
//
//   - **Downgrade.** Serving an old, genuinely signed release. Refused by
//     comparing versions: an update must be strictly newer than the running
//     binary ([Updater.Check]).
//   - **Relabelling.** Serving v0.1.0's signed checksums under a tag claiming
//     v9.9.9. Refused because the asset name is derived from the claimed
//     version and looked up *inside* the signed file — `latem_9.9.9_…tar.gz`
//     is not a line anyone but the signer can produce. The version is bound
//     by the signed content rather than by the feed's word for it.
//
// # What this package deliberately does not do
//
// It does not restart anything. The atomic rename is the last thing it
// touches, and the decision about when a machine may replace its own running
// process belongs to the thing that knows whether a build is in flight
// (internal/agent). §20's "never update mid-run" is not a property this
// package can enforce, so it does not pretend to.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultFeedURL is the release feed: GitHub's own answer to "what is the
// latest release", which excludes drafts and prereleases.
//
// Not configurable through config.toml. An operator who could repoint it would
// gain nothing — the signature is the trust root, so a different feed can only
// offer versions this key signed — while the option itself would read as a way
// to run a private build, which it is not. A test injects its own.
const DefaultFeedURL = "https://api.github.com/repos/Abyssinia-Labs/latem/releases/latest"

// DefaultDownloadBase is where release assets live.
const DefaultDownloadBase = "https://github.com/Abyssinia-Labs/latem/releases/download"

// maxAssetBytes caps a downloaded archive. The binary is ~25 MB; the cap is
// generous enough to survive a year of growth and small enough that a feed
// pointing at something enormous fills no disk.
const maxAssetBytes = 256 << 20

// DevVersion is the version a `go build` produces, and the one value that must
// never self-update: there is nothing to compare it against.
const DevVersion = "dev"

// Updater checks for and fetches releases. Zero values take the defaults
// above; everything is injectable so the tests need no network.
type Updater struct {
	// Current is the running binary's version, as `latem version --short`
	// prints it.
	Current string
	// Key is the trust root. Use [TrustedKey].
	Key PublicKey

	FeedURL      string
	DownloadBase string
	// GOOS and GOARCH name the asset to fetch. Empty takes this build's.
	GOOS, GOARCH string

	Client *http.Client
	Log    *slog.Logger
}

// Release is one entry from the feed, after the version has been parsed but
// before anything has been verified.
type Release struct {
	// Version is the tag, as the feed spelled it (`v0.1.0`).
	Version string
	// Bare is the tag without its leading `v`, which is the spelling that
	// appears in asset names.
	Bare string
}

// ErrUpToDate is returned by [Updater.Check] when the feed offers nothing
// newer. It is the expected daily outcome, so callers should not log it as a
// failure.
var ErrUpToDate = errors.New("already running the latest release")

// AssetName is the per-platform archive name. install.sh builds the same
// string in sh, and a test in test/scripts asserts the two agree — the day
// they drift, half the installs fail and the other half self-update into
// nothing.
func AssetName(bareVersion, goos, goarch string) string {
	return fmt.Sprintf("latem_%s_%s_%s.tar.gz", bareVersion, goos, goarch)
}

// ChecksumsName is the signed manifest's name. Its signature is
// ChecksumsName + ".minisig".
func ChecksumsName(bareVersion string) string {
	return fmt.Sprintf("latem_%s_checksums.txt", bareVersion)
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	// A timeout rather than none: this runs on a ticker, and a feed that
	// accepts a connection and never answers would otherwise leak a goroutine
	// a day. Generous enough for a 25 MB download on a slow link.
	return &http.Client{Timeout: 10 * time.Minute}
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (u *Updater) goos() string {
	if u.GOOS != "" {
		return u.GOOS
	}
	return runtime.GOOS
}

func (u *Updater) goarch() string {
	if u.GOARCH != "" {
		return u.GOARCH
	}
	return runtime.GOARCH
}

func (u *Updater) feedURL() string {
	if u.FeedURL != "" {
		return u.FeedURL
	}
	return DefaultFeedURL
}

func (u *Updater) downloadBase() string {
	if u.DownloadBase != "" {
		return u.DownloadBase
	}
	return DefaultDownloadBase
}

// Check asks the feed for the latest release and reports whether it is newer
// than the running binary.
//
// It returns [ErrUpToDate] when it is not, which is the ordinary answer.
func (u *Updater) Check(ctx context.Context) (Release, error) {
	var rel Release

	if u.Key.Zero() {
		return rel, ErrNoTrustedKey
	}
	if u.Current == "" || u.Current == DevVersion {
		return rel, fmt.Errorf("refusing to self-update a %q build: "+
			"it has no version to compare a release against", DevVersion)
	}
	if _, err := parseVersion(u.Current); err != nil {
		// A binary whose own version is unparseable cannot know whether a
		// release is newer, and guessing in either direction is worse than
		// saying so.
		return rel, fmt.Errorf("this binary's version %q is not a release version: %w", u.Current, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.feedURL(), nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "latem-agent/"+u.Current)

	resp, err := u.client().Do(req)
	if err != nil {
		return rel, fmt.Errorf("fetching the release feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("the release feed answered %s", resp.Status)
	}

	var feed struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	// 1 MB is a generous release payload; the field this reads is a tag.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&feed); err != nil {
		return rel, fmt.Errorf("decoding the release feed: %w", err)
	}
	if feed.TagName == "" {
		return rel, errors.New("the release feed carried no tag_name")
	}
	// `releases/latest` already excludes both, so this is a guard against a
	// feed that is not the one this code was written against rather than
	// against GitHub.
	if feed.Draft || feed.Prerelease {
		return rel, fmt.Errorf("the feed's latest release %s is a draft or prerelease", feed.TagName)
	}

	cmp, err := compareVersions(feed.TagName, u.Current)
	if err != nil {
		return rel, fmt.Errorf("comparing %s against this binary's %s: %w", feed.TagName, u.Current, err)
	}
	if cmp <= 0 {
		return rel, ErrUpToDate
	}

	rel.Version = feed.TagName
	rel.Bare = strings.TrimPrefix(feed.TagName, "v")
	return rel, nil
}

// Fetch downloads a release, verifies it, and leaves an executable binary in
// dir. The returned path is the new binary; nothing on the machine has been
// replaced yet.
//
// dir should be on the same filesystem as the binary it will replace, because
// [Install]'s rename cannot cross one.
func (u *Updater) Fetch(ctx context.Context, rel Release, dir string) (string, error) {
	if u.Key.Zero() {
		return "", ErrNoTrustedKey
	}

	sumsName := ChecksumsName(rel.Bare)
	sums, err := u.get(ctx, rel, sumsName, 1<<20)
	if err != nil {
		return "", err
	}
	sigRaw, err := u.get(ctx, rel, sumsName+".minisig", 1<<16)
	if err != nil {
		return "", err
	}
	sig, err := ParseSignature(string(sigRaw))
	if err != nil {
		return "", fmt.Errorf("%s.minisig: %w", sumsName, err)
	}
	if err := u.Key.Verify(sums, sig); err != nil {
		return "", fmt.Errorf("%s is not signed by the key this build trusts: %w", sumsName, err)
	}
	// Everything from here reads a verified file. The trusted comment carries
	// the version the signer meant, which is worth logging and is *not* what
	// binds this download to a version — the asset name below does that, and
	// it does it inside the signed bytes.
	u.log().Info("release signature verified",
		"release", rel.Version, "key", u.Key.ID(), "comment", sig.TrustedComment())

	asset := AssetName(rel.Bare, u.goos(), u.goarch())
	want, err := checksumFor(string(sums), asset)
	if err != nil {
		return "", err
	}

	archive, err := u.get(ctx, rel, asset, maxAssetBytes)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("%s does not match its signed checksum\n  expected %s\n  got      %s",
			asset, want, hex.EncodeToString(got[:]))
	}

	path, err := extractBinary(archive, dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", asset, err)
	}

	// Ask the new binary what it is. The signature already proves who built
	// it, so this is not a trust check — it is the check that the release was
	// assembled correctly: an archive built from the wrong tag, or published
	// under the wrong platform's name, is signed exactly as faithfully as a
	// correct one.
	if err := confirmVersion(ctx, path, rel.Version); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// get fetches one release asset.
func (u *Updater) get(ctx context.Context, rel Release, name string, limit int64) ([]byte, error) {
	url := strings.TrimSuffix(u.downloadBase(), "/") + "/" + rel.Version + "/" + name

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "latem-agent/"+u.Current)

	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", name, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return body, nil
}

// checksumFor finds one asset's line in a verified checksums file.
//
// The lookup is by exact name, and that is the version binding described in
// the package comment: the name contains the version, and the file is signed.
func checksumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// `sha256sum` writes `<hex>  <name>`; `<hex> *<name>` is the binary-mode
		// spelling. Accept both, since the file is produced by whichever tool
		// the release runner had.
		if strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != hex.EncodedLen(sha256.Size) {
			return "", fmt.Errorf("%s's checksum is %d characters, not a sha256", asset, len(sum))
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("%s's checksum is not hex: %w", asset, err)
		}
		return sum, nil
	}
	return "", fmt.Errorf("%s is not listed in the signed checksums; "+
		"this release does not contain a build for this platform, or it is not the release "+
		"the feed said it was", asset)
}

// extractBinary pulls `latem` out of a release archive.
func extractBinary(archive []byte, dir string) (string, error) {
	zr, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return "", fmt.Errorf("not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()

	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading the archive: %w", err)
		}
		// Only ever one name, matched exactly. A release archive is not a
		// general-purpose tarball to be unpacked: ignoring every other entry
		// is also what makes a path-traversal entry irrelevant rather than
		// something to sanitise.
		if filepath.Clean(h.Name) != "latem" || h.Typeflag != tar.TypeReg {
			continue
		}

		path := filepath.Join(dir, ".latem.new")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // an executable, by definition
		if err != nil {
			return "", err
		}
		// The limit is the same one the download honoured: a small archive
		// that expands to fill the disk is the one thing gzip makes cheap.
		if _, err := io.Copy(f, io.LimitReader(tr, maxAssetBytes)); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", errors.New("the archive contains no `latem` binary")
}

// confirmVersion runs the downloaded binary and requires it to agree about
// what it is.
func confirmVersion(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "version", "--short").Output() //nolint:gosec // a path this package just wrote
	if err != nil {
		return fmt.Errorf("the downloaded binary would not run (wrong platform?): %w", err)
	}
	got := strings.TrimSpace(string(out))
	if got != want && got != strings.TrimPrefix(want, "v") {
		return fmt.Errorf("the downloaded binary reports version %q, and the release said %q", got, want)
	}
	return nil
}

// Install replaces target with the staged binary, atomically.
//
// Rename rather than write: the target is a running executable, so opening it
// for writing fails with ETXTBSY on Linux and would leave a half-written
// binary on any platform where it did not. The running process keeps its own
// inode and is unaffected until it re-execs.
func Install(staged, target string) error {
	if filepath.Dir(staged) != filepath.Dir(target) {
		// Rename cannot cross a filesystem, and staging elsewhere would turn
		// the atomic step into a copy.
		return fmt.Errorf("selfupdate: %s and %s are in different directories; "+
			"stage the download beside the binary it replaces", staged, target)
	}
	// Carry the old binary's mode across. An operator who made it 0700 meant
	// it, and a release should not widen that.
	if fi, err := os.Stat(target); err == nil {
		if err := os.Chmod(staged, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("selfupdate: replacing %s: %w", target, err)
	}
	return nil
}

// version is a parsed release tag.
type version struct {
	major, minor, patch int
	pre                 string
}

// parseVersion accepts `v1.2.3` and `1.2.3`, with an optional `-suffix`.
//
// Deliberately strict. This is the input to a comparison that decides whether
// to replace a binary, and a lenient parser's mistakes all look like
// "installed something older on purpose".
func parseVersion(s string) (version, error) {
	var v version

	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return v, errors.New("empty")
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		v.pre, s = s[i+1:], s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("%q is not MAJOR.MINOR.PATCH", s)
	}
	dst := []*int{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("%q is not a non-negative integer", p)
		}
		*dst[i] = n
	}
	return v, nil
}

// compareVersions reports whether a is newer than (1), the same as (0), or
// older than (-1) b.
func compareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for _, p := range [][2]int{{va.major, vb.major}, {va.minor, vb.minor}, {va.patch, vb.patch}} {
		switch {
		case p[0] > p[1]:
			return 1, nil
		case p[0] < p[1]:
			return -1, nil
		}
	}
	// Semver's rule: a prerelease precedes the release it leads to. The
	// consequence that matters here is that an agent on v0.2.0-rc.1 updates to
	// v0.2.0, and one on v0.2.0 does not move to v0.2.0-rc.2.
	switch {
	case va.pre == vb.pre:
		return 0, nil
	case va.pre == "":
		return 1, nil
	case vb.pre == "":
		return -1, nil
	}
	return strings.Compare(va.pre, vb.pre), nil
}
