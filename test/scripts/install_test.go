package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/selfupdate"
)

// install.sh is the first thing anybody runs and, until this file, the only
// deliverable in the tree that nothing exercised. M7's acceptance criterion is
// that a stranger gets from `curl | sh` to a green run in five minutes, and
// there was no way to find out that the script still worked short of
// publishing a release and asking one.
//
// These tests serve a synthesised release over loopback and run the real
// script against it, which also settles a contract that would otherwise be two
// independent copies of a filename convention:
//
//	install.sh (sh)          "latem_${VERSION#v}_${OS}_${ARCH}.tar.gz"
//	internal/selfupdate (Go)  selfupdate.AssetName(bare, goos, goarch)
//
// The release below is named by the Go function and fetched by the shell one.
// The day they disagree, this test 404s — which is the same failure every
// install would produce, found here instead of there. That is amendment A37's
// rule applied to a convention rather than to a hazard: two hands writing the
// same string in two languages needs something mechanical holding them level.

// fakeRelease is a release the installer can install: one archive for this
// machine's platform, plus the checksums file install.sh insists on.
type fakeRelease struct {
	version string
	bare    string
	files   map[string][]byte
}

func newFakeRelease(t *testing.T, version, prints string) *fakeRelease {
	t.Helper()
	r := &fakeRelease{
		version: version,
		bare:    strings.TrimPrefix(version, "v"),
		files:   map[string][]byte{},
	}

	// The "binary" is a shell script, so the test needs no cross-compiler and
	// runs in a second. install.sh only requires that the archive contains a
	// file called `latem` and that it survives being executed once, by
	// `latem doctor`, at the end.
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo " + prints + " ;;\n" +
		"  doctor)  echo 'doctor ran'; exit 1 ;;\n" + // doctor's non-zero exit must not fail the install
		"  *)       echo \"unexpected: $*\" >&2; exit 64 ;;\n" +
		"esac\n"

	name := selfupdate.AssetName(r.bare, runtime.GOOS, runtime.GOARCH)
	r.files[name] = tarGzOf(t, "latem", []byte(script))

	sum := sha256.Sum256(r.files[name])
	r.files[selfupdate.ChecksumsName(r.bare)] =
		[]byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
	return r
}

func (r *fakeRelease) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// install.sh builds "$BASE_URL/$VERSION/$ASSET", so the last element
		// is the only part this needs to look at — and a request for a name
		// the release does not carry has to 404 rather than return something,
		// or the checksum step would be testing nothing.
		body, ok := r.files[filepath.Base(req.URL.Path)]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func tarGzOf(t *testing.T, name string, body []byte) []byte {
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
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runInstaller runs install.sh against a served release and returns its
// combined output.
func runInstaller(t *testing.T, r *fakeRelease, installDir string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget is installed; install.sh needs one")
		}
	}

	script := filepath.Join(moduleRoot(t), "install.sh")
	// /bin/sh on purpose. The script's own header says it is POSIX sh because
	// `curl | sh` runs under dash on Debian, and running it under bash here
	// would be testing a shell nobody installs with.
	cmd := exec.Command("/bin/sh", script) //nolint:gosec // a path inside this repository
	cmd.Env = append(os.Environ(),
		"LATEM_VERSION="+r.version,
		"LATEM_BASE_URL="+r.serve(t),
		"LATEM_INSTALL_DIR="+installDir,
		// So the transcript in a failure message is readable.
		"NO_COLOR=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScript_InstallsAVerifiedRelease(t *testing.T) {
	r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
	dir := filepath.Join(t.TempDir(), "bin")

	out, err := runInstaller(t, r, dir)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}

	binary := filepath.Join(dir, "latem")
	fi, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("nothing was installed: %v\n%s", err, out)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %v, want 0755", got)
	}
	// It ran doctor, and doctor's non-zero exit did not fail the install. That
	// is deliberate in the script and worth holding: the operator is meant to
	// read the list of things to fix, not be handed an error instead of it.
	if !strings.Contains(out, "doctor ran") {
		t.Errorf("install.sh did not run doctor:\n%s", out)
	}
	// And nothing was left behind by the rename-into-place.
	if _, err := os.Stat(filepath.Join(dir, ".latem.new")); err == nil {
		t.Error("the staging file survived the install")
	}
}

// The checksum is the only thing standing between `curl | sh` and running
// whatever arrived, and the script says it has no flag to skip. This is the
// test that says so too.
func TestInstallScript_RefusesAnArchiveThatDoesNotMatchItsChecksum(t *testing.T) {
	r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
	name := selfupdate.AssetName(r.bare, runtime.GOOS, runtime.GOARCH)
	r.files[name] = tarGzOf(t, "latem", []byte("#!/bin/sh\necho substituted\n"))

	dir := filepath.Join(t.TempDir(), "bin")
	out, err := runInstaller(t, r, dir)
	if err == nil {
		t.Fatalf("install.sh installed an archive that did not match its checksum:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("the failure does not say what went wrong:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "latem")); err == nil {
		t.Error("a mismatched archive was installed anyway")
	}
}

func TestInstallScript_RefusesAnAssetWithNoChecksumLine(t *testing.T) {
	// A release published with a checksums file that does not list this
	// platform's archive. The archive downloads fine, and there is nothing to
	// compare it against — which must be a refusal rather than a pass.
	r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
	r.files[selfupdate.ChecksumsName(r.bare)] = []byte(
		"0000000000000000000000000000000000000000000000000000000000000000  latem_0.9.0_plan9_mips.tar.gz\n")

	dir := filepath.Join(t.TempDir(), "bin")
	out, err := runInstaller(t, r, dir)
	if err == nil {
		t.Fatalf("install.sh installed an unlisted asset:\n%s", out)
	}
	if !strings.Contains(out, "not listed in the checksum file") {
		t.Errorf("the failure does not say the asset was unlisted:\n%s", out)
	}
}

// A missing release is the ordinary failure while a tag is still building, and
// the message has to name the URL it could not fetch — the alternative is an
// operator who cannot tell a typo in LATEM_VERSION from an outage.
func TestInstallScript_SaysWhatItCouldNotDownload(t *testing.T) {
	r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
	r.files = map[string][]byte{}

	out, err := runInstaller(t, r, filepath.Join(t.TempDir(), "bin"))
	if err == nil {
		t.Fatalf("install.sh succeeded against an empty release:\n%s", out)
	}
	if !strings.Contains(out, "download failed") ||
		!strings.Contains(out, selfupdate.AssetName("0.9.0", runtime.GOOS, runtime.GOARCH)) {
		t.Errorf("the failure does not name the asset it wanted:\n%s", out)
	}
}

// ---------------------------------------------------------------- signature

// installerWithKey writes a copy of install.sh carrying a public key, so the
// signature path can be exercised with a keypair the test controls.
//
// The substitution is asserted rather than assumed. If the `PUBKEY=""` line is
// ever reshaped, a silent no-op here would leave this test running the
// no-key branch and reporting success for a path it never entered.
func installerWithKey(t *testing.T, pubkey string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const slot = "PUBKEY=\"\""
	if n := strings.Count(string(body), slot); n != 1 {
		t.Fatalf("install.sh has %d %s lines; this test patches exactly one", n, slot)
	}
	out := filepath.Join(t.TempDir(), "install.sh")
	patched := strings.Replace(string(body), slot, "PUBKEY=\""+pubkey+"\"", 1)
	if err := os.WriteFile(out, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
	return out
}

// minisignKeypair generates a throwaway password-less keypair and returns the
// public key line and the secret key's path.
func minisignKeypair(t *testing.T) (pubkey, secret string) {
	t.Helper()
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign is not installed; `brew install minisign` or `apt-get install minisign` " +
			"to exercise install.sh's signature check")
	}
	dir := t.TempDir()
	pub, secret := filepath.Join(dir, "pub"), filepath.Join(dir, "sec")
	out, err := exec.Command("minisign", "-G", "-p", pub, "-s", secret, "-W").CombinedOutput()
	if err != nil {
		t.Fatalf("minisign -G: %v\n%s", err, out)
	}
	body, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	return lines[len(lines)-1], secret
}

// signChecksums signs a release's checksums file in place, the way the release
// workflow's `sign` job does.
func signChecksums(t *testing.T, r *fakeRelease, secret, trusted string) {
	t.Helper()
	dir := t.TempDir()
	name := selfupdate.ChecksumsName(r.bare)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, r.files[name], 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("minisign", "-S", "-s", secret, "-m", path, "-t", trusted).CombinedOutput()
	if err != nil {
		t.Fatalf("minisign -S: %v\n%s", err, out)
	}
	sig, err := os.ReadFile(path + ".minisig")
	if err != nil {
		t.Fatal(err)
	}
	r.files[name+".minisig"] = sig
}

func runInstallerScript(t *testing.T, script string, r *fakeRelease, installDir string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", script) //nolint:gosec // a copy of this repository's own script
	cmd.Env = append(os.Environ(),
		"LATEM_VERSION="+r.version,
		"LATEM_BASE_URL="+r.serve(t),
		"LATEM_INSTALL_DIR="+installDir,
		"NO_COLOR=1",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScript_VerifiesTheReleaseSignature(t *testing.T) {
	pubkey, secret := minisignKeypair(t)
	script := installerWithKey(t, pubkey)

	t.Run("a signed release installs", func(t *testing.T) {
		r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
		signChecksums(t, r, secret, "latem v0.9.0")

		dir := filepath.Join(t.TempDir(), "bin")
		out, err := runInstallerScript(t, script, r, dir)
		if err != nil {
			t.Fatalf("install.sh: %v\n%s", err, out)
		}
		if !strings.Contains(out, "signature ok") {
			t.Errorf("the signature was not checked:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "latem")); err != nil {
			t.Errorf("nothing was installed: %v\n%s", err, out)
		}
	})

	// A checksums file signed by a key this installer does not know. Every
	// byte of it is internally consistent — the archive matches its own
	// checksum — which is exactly the case the checksum cannot catch and the
	// signature exists for.
	t.Run("another key's signature is refused", func(t *testing.T) {
		_, otherSecret := minisignKeypair(t)
		r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
		signChecksums(t, r, otherSecret, "latem v0.9.0")

		dir := filepath.Join(t.TempDir(), "bin")
		out, err := runInstallerScript(t, script, r, dir)
		if err == nil {
			t.Fatalf("a release signed by an unknown key was installed:\n%s", out)
		}
		if !strings.Contains(out, "not signed by Latem's release key") {
			t.Errorf("the failure does not say the signature was wrong:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(dir, "latem")); err == nil {
			t.Error("it installed anyway")
		}
	})

	// A release published without a signature, to an installer that expects
	// one. Falling back to the checksum here would make the signature
	// optional in practice, which is the same as not having it.
	t.Run("a release with no signature is refused", func(t *testing.T) {
		r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")

		out, err := runInstallerScript(t, script, r, filepath.Join(t.TempDir(), "bin"))
		if err == nil {
			t.Fatalf("an unsigned release was installed:\n%s", out)
		}
		if !strings.Contains(out, "could not download the signature") {
			t.Errorf("the failure does not name the missing signature:\n%s", out)
		}
	})

	// And tampering after signing: the signature is over the checksums file,
	// so an edited manifest must fail there rather than at the sha256 compare.
	t.Run("an edited checksums file is refused", func(t *testing.T) {
		r := newFakeRelease(t, "v0.9.0", "latem v0.9.0")
		signChecksums(t, r, secret, "latem v0.9.0")
		name := selfupdate.ChecksumsName(r.bare)
		r.files[name] = append(r.files[name], []byte("0000  extra.tar.gz\n")...)

		out, err := runInstallerScript(t, script, r, filepath.Join(t.TempDir(), "bin"))
		if err == nil {
			t.Fatalf("an edited checksums file was accepted:\n%s", out)
		}
		if !strings.Contains(out, "not signed by Latem's release key") {
			t.Errorf("the failure does not point at the signature:\n%s", out)
		}
	})
}

// The installer's PUBKEY and the agent's internal/selfupdate/trusted.pub are
// two copies of one key, written by hand into two languages. This is the
// mechanical check A37 asks for in place of a comment hoping they match.
//
// Either both are empty — a repository with no signing key yet, where
// install.sh warns and the config loader refuses auto_update — or both hold
// the same key. Nothing else is a coherent state: a populated trusted.pub with
// an empty PUBKEY ships an installer that cannot check what the agent it
// installs will insist on.
func TestInstallScript_CarriesTheSameKeyTheAgentTrusts(t *testing.T) {
	root := moduleRoot(t)

	script, err := os.ReadFile(filepath.Join(root, "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^PUBKEY="([^"]*)"$`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh has no PUBKEY= line; the agent's trusted key has no counterpart there")
	}
	inScript := string(m[1])

	embedded, err := os.ReadFile(filepath.Join(root, "internal/selfupdate/trusted.pub"))
	if err != nil {
		t.Fatal(err)
	}
	inAgent := ""
	for _, line := range strings.Split(string(embedded), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		inAgent = line
		break
	}

	switch {
	case inScript == "" && inAgent == "":
		t.Log("neither carries a key yet: install.sh warns, and the config loader " +
			"refuses auto_update (docs/release.md §2 is how that ends)")
	case inScript != inAgent:
		t.Errorf("install.sh and the agent trust different keys.\n  install.sh:  %q\n  trusted.pub: %q\n"+
			"An installer that cannot verify what it downloads, or an agent that refuses "+
			"what the installer accepted — whichever way round, one of them is wrong.",
			inScript, inAgent)
	}
}
