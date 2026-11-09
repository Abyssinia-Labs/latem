package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/internal/executor"
)

type tarEntry struct {
	name, body, link string
	dir              bool
}

func archive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		switch {
		case e.dir:
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		case e.link != "":
			hdr.Typeflag, hdr.Linkname, hdr.Size = tar.TypeSymlink, e.link, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func copier(t *testing.T, f *fakeAPI) executor.TempCopier {
	t.Helper()
	env := prepared(t, f, executor.Spec{RunID: "run-1"})
	tc, ok := env.(executor.TempCopier)
	if !ok {
		t.Fatal("docker's environment is no TempCopier")
	}
	return tc
}

// sokie's pack uploads the tarball npm wrote to $RUNNER_TEMP/pack: one file,
// or the directory it is in, copied out to the same relative path on the host
// (A84). A path that is not there copies nothing, and a pattern's directory
// that is all of runner.temp copies all of it.
func TestCopyFromTemp(t *testing.T) {
	f := &fakeAPI{archives: map[string][]byte{
		TempPath + "/pack/sokie-1.0.0.tgz": archive(t, tarEntry{name: "sokie-1.0.0.tgz", body: "tgz"}),
		TempPath + "/pack": archive(t,
			tarEntry{name: "pack", dir: true},
			tarEntry{name: "pack/sokie-1.0.0.tgz", body: "tgz"},
			tarEntry{name: "pack/sub", dir: true},
			tarEntry{name: "pack/sub/notes.txt", body: "notes"}),
		TempPath: archive(t,
			tarEntry{name: "temp", dir: true},
			tarEntry{name: "temp/a.txt", body: "a"}),
	}}
	tc := copier(t, f)
	ctx := context.Background()

	dst := t.TempDir()
	n, err := tc.CopyFromTemp(ctx, "pack/sokie-1.0.0.tgz", dst, 1<<20)
	if err != nil || n != 3 {
		t.Fatalf("one file: %d, %v", n, err)
	}
	assertFile(t, filepath.Join(dst, "pack", "sokie-1.0.0.tgz"), "tgz")

	dst = t.TempDir()
	if _, err := tc.CopyFromTemp(ctx, "pack", dst, 1<<20); err != nil {
		t.Fatalf("a directory: %v", err)
	}
	assertFile(t, filepath.Join(dst, "pack", "sokie-1.0.0.tgz"), "tgz")
	assertFile(t, filepath.Join(dst, "pack", "sub", "notes.txt"), "notes")

	dst = t.TempDir()
	if _, err := tc.CopyFromTemp(ctx, "", dst, 1<<20); err != nil {
		t.Fatalf("all of runner.temp: %v", err)
	}
	assertFile(t, filepath.Join(dst, "a.txt"), "a")

	if n, err := tc.CopyFromTemp(ctx, "missing", t.TempDir(), 1<<20); err != nil || n != 0 {
		t.Errorf("a path not there: %d, %v", n, err)
	}
	if _, err := tc.CopyFromTemp(ctx, "../etc", t.TempDir(), 1<<20); err == nil {
		t.Error("a path outside runner.temp was copied")
	}
	if _, err := tc.CopyFromTemp(ctx, "pack", t.TempDir(), 4); err == nil || !strings.Contains(err.Error(), "more than 4 bytes") {
		t.Errorf("past the limit: %v", err)
	}
}

// What the daemon sends was built from files a step wrote, so it is read as
// hostile: an entry that climbs out, or a symlink, writes nothing outside
// where the copy lands.
func TestCopyFromTempRefusesWhatEscapes(t *testing.T) {
	outside := t.TempDir()
	f := &fakeAPI{archives: map[string][]byte{
		TempPath + "/climb": archive(t, tarEntry{name: "climb/../../../escaped.txt", body: "x"}),
		TempPath + "/link": archive(t,
			tarEntry{name: "link", dir: true},
			tarEntry{name: "link/out", link: outside},
			tarEntry{name: "link/kept.txt", body: "kept"}),
	}}
	tc := copier(t, f)
	ctx := context.Background()

	dst := t.TempDir()
	if _, err := tc.CopyFromTemp(ctx, "climb", dst, 1<<20); err == nil {
		t.Error("an entry outside the copy was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escaped.txt")); err == nil {
		t.Error("an entry was written outside the copy")
	}

	dst = t.TempDir()
	if _, err := tc.CopyFromTemp(ctx, "link", dst, 1<<20); err != nil {
		t.Fatalf("a symlink should be left out, not fail the copy: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link", "out")); err == nil {
		t.Error("a symlink was written")
	}
	assertFile(t, filepath.Join(dst, "link", "kept.txt"), "kept")
}

// A download into runner.temp is staged on the host and copied in, at the
// same relative paths, for the step's user (A84): the Mac releases' publish
// reads its DMG from $RUNNER_TEMP/dmg.
func TestCopyToTemp(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dmg"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dmg", "TakeOne-1.dmg"), []byte("dmg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "dmg", "link")); err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{envFileMissing: true}
	tc := copier(t, f)
	before := len(f.copiedTo)
	if err := tc.CopyToTemp(context.Background(), src); err != nil {
		t.Fatalf("CopyToTemp: %v", err)
	}
	if len(f.copiedTo) != before+1 {
		t.Fatalf("copies = %d, want one more than %d", len(f.copiedTo), before)
	}
	got := f.copiedTo[len(f.copiedTo)-1]
	if got.dst != TempPath || got.entries["dmg/TakeOne-1.dmg"] != "dmg" {
		t.Fatalf("copied %+v, want dmg/TakeOne-1.dmg into %s", got.entries, TempPath)
	}
	if _, ok := got.entries["dmg/link"]; ok {
		t.Error("a symlink was copied in")
	}
	if h := got.headers["dmg/"]; h.Typeflag != tar.TypeDir || h.Mode != 0o777 {
		t.Errorf("dmg/ is %c %o, want a directory anyone can write, the step user being no uid", h.Typeflag, h.Mode)
	}
	if h := got.headers["dmg/TakeOne-1.dmg"]; h.Mode != 0o666 {
		t.Errorf("the DMG is %o", h.Mode)
	}
}

func assertFile(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil || string(got) != want {
		t.Errorf("%s = %q, %v; want %q", p, got, err, want)
	}
}
