package main

import (
	"archive/zip"
	"context"
	"crypto/sha1" //nolint:gosec // the manifest's own checksum; see verify below
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// androidRepository is Google's SDK repository: a manifest, and what it names.
// A var so a test can serve one.
var androidRepository = "https://dl.google.com/android/repository/"

const (
	// A manifest is a few hundred kilobytes, and the tools a few hundred
	// megabytes unpacked. These only bound what a broken server can do.
	androidManifestLimit = 16 << 20
	androidArchiveLimit  = 1 << 30
)

func newSetupAndroidSDKCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "android-sdk",
		Short: "Put Google's Android command-line tools in ~/Android/Sdk, for this user",
		Long: `Put Google's Android command-line tools where a build finds them, on Linux:
$ANDROID_HOME, or else ~/Android/Sdk.

Linux has no one package for these, and the ones that exist install them
where only root can add to them, so every later sdkmanager would need sudo.
This asks for nothing: it reads Google's manifest, downloads the archive it
names over HTTPS, checks it against the manifest, and unpacks it as this user.

It accepts no licence. After it, you run sdkmanager --licenses, which ` + "`latem setup`" + `
hands you a terminal for.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runtime.GOOS != "linux" {
				return errors.New("this is for Linux; on a Mac: brew install --cask android-commandlinetools")
			}
			dest := os.Getenv("ANDROID_HOME")
			if dest == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				dest = filepath.Join(home, "Android", "Sdk")
			}
			return installCmdlineTools(cmd.Context(), cmd.OutOrStdout(), http.DefaultClient, dest)
		},
	}
}

// sdkManifest is the part of Google's repository2-3.xml this reads.
type sdkManifest struct {
	Packages []struct {
		Path     string `xml:"path,attr"`
		Archives []struct {
			HostOS   string `xml:"host-os"`
			Complete struct {
				Size     int64  `xml:"size"`
				Checksum string `xml:"checksum"`
				URL      string `xml:"url"`
			} `xml:"complete"`
		} `xml:"archives>archive"`
	} `xml:"remotePackage"`
}

// cmdlineToolsArchive is what the manifest says to download for Linux.
func cmdlineToolsArchive(ctx context.Context, client *http.Client) (name, sum string, size int64, err error) {
	body, err := androidGet(ctx, client, "repository2-3.xml")
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = body.Close() }()
	var m sdkManifest
	if err := xml.NewDecoder(io.LimitReader(body, androidManifestLimit)).Decode(&m); err != nil {
		return "", "", 0, fmt.Errorf("reading Google's SDK manifest: %w", err)
	}
	for _, p := range m.Packages {
		if p.Path != "cmdline-tools;latest" {
			continue
		}
		for _, a := range p.Archives {
			if a.HostOS == "linux" {
				return a.Complete.URL, a.Complete.Checksum, a.Complete.Size, nil
			}
		}
	}
	return "", "", 0, errors.New("the SDK manifest names no command-line tools for Linux")
}

func androidGet(ctx context.Context, client *http.Client, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, androidRepository+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	return resp.Body, nil
}

// installCmdlineTools puts the tools at dest/cmdline-tools/latest, where
// sdkmanager works out the SDK's root from where it is. Nothing lands there
// until all of it has: a build, or the next `latem setup`, must not find half.
func installCmdlineTools(ctx context.Context, w io.Writer, client *http.Client, dest string) error {
	final := filepath.Join(dest, "cmdline-tools", "latest")
	// #nosec G703 -- dest is the operator's own $ANDROID_HOME or home directory.
	if _, err := os.Stat(filepath.Join(final, "bin", "sdkmanager")); err == nil {
		printf(w, "the Android command-line tools are already at %s\n", final)
		return nil
	}
	name, sum, size, err := cmdlineToolsArchive(ctx, client)
	if err != nil {
		return err
	}
	if size <= 0 || size > androidArchiveLimit || filepath.Base(name) != name {
		return fmt.Errorf("the SDK manifest says something unexpected: %q, %d bytes", name, size)
	}
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil { //nolint:gosec // an SDK is meant to be read by whoever builds
		return err
	}
	work, err := os.MkdirTemp(parent, ".latem-")
	if err != nil {
		return err
	}
	// #nosec G703 -- made just above, inside the operator's own SDK.
	defer func() { _ = os.RemoveAll(work) }()

	printf(w, "downloading %s (%d MB) from %s\n", name, size>>20, androidRepository)
	archive := filepath.Join(work, "tools.zip")
	if err := downloadVerified(ctx, client, name, archive, size, sum); err != nil {
		return err
	}
	unpacked := filepath.Join(work, "latest")
	if err := unpackCmdlineTools(archive, unpacked); err != nil {
		return err
	}
	// #nosec G703 -- under a directory made above.
	if _, err := os.Stat(filepath.Join(unpacked, "bin", "sdkmanager")); err != nil {
		return errors.New("the archive holds no bin/sdkmanager")
	}
	// A folder left at latest by something else, with no sdkmanager in it.
	// #nosec G703 -- inside the operator's own SDK.
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	// #nosec G703 -- both are inside the operator's own SDK.
	if err := os.Rename(unpacked, final); err != nil {
		return err
	}
	printf(w, "put them in %s\n", final)
	return nil
}

// downloadVerified fetches name to path and checks its size and SHA-1 against
// the manifest. SHA-1 is what the manifest publishes, and sdkmanager itself
// checks no more: this catches a truncated or corrupted download, and HTTPS
// to Google is what says who sent it.
func downloadVerified(ctx context.Context, client *http.Client, name, path string, size int64, sum string) error {
	body, err := androidGet(ctx, client, name)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // path is under a directory made here
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	hash := sha1.New() //nolint:gosec // see above
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(body, androidArchiveLimit))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s came to %d bytes, and the manifest says %d", name, n, size)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, sum) {
		return fmt.Errorf("%s does not match the manifest: its SHA-1 is %s, not %s", name, got, sum)
	}
	return f.Close()
}

// unpackCmdlineTools unpacks the archive's cmdline-tools/ into dest. Each
// name is checked to stay inside it, and anything but a file or a folder is
// refused: this is a download being written into a home directory.
func unpackCmdlineTools(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	var total uint64
	for _, zf := range zr.File {
		rel, ok := strings.CutPrefix(filepath.ToSlash(zf.Name), "cmdline-tools/")
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(filepath.Separator)) {
			return fmt.Errorf("the archive names a path outside itself: %q", zf.Name)
		}
		switch mode := zf.Mode(); {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // as the SDK's own folders are
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("the archive holds something that is not a file: %q", zf.Name)
		}
		if total += zf.UncompressedSize64; total > 2*androidArchiveLimit {
			return errors.New("the archive unpacks to more than it should")
		}
		if err := unpackFile(zf, target); err != nil {
			return err
		}
	}
	return nil
}

func unpackFile(zf *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // as the SDK's own folders are
		return err
	}
	in, err := zf.Open()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	mode := os.FileMode(0o644)
	if zf.Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode) //nolint:gosec // checked by the caller
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, io.LimitReader(in, androidArchiveLimit)); err != nil {
		return err
	}
	return out.Close()
}
