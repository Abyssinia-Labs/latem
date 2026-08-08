package source

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestNew_CreatesDirectoriesPrivately(t *testing.T) {
	root := t.TempDir()
	mgr := newManager(t, root)

	for _, dir := range []string{mgr.MirrorDir(), mgr.WorkDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		// ~/.latem holds a machine token and secrets.env beside these; a
		// world-readable config tree is the threat model failing quietly.
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s is mode %o, want 700", dir, perm)
		}
	}
}

func TestNew_RequiresAbsolutePaths(t *testing.T) {
	root := t.TempDir()

	if _, err := New(Config{MirrorDir: "mirrors", WorkDir: filepath.Join(root, "work")}); err == nil {
		t.Error("accepted a relative mirror dir")
	}
	if _, err := New(Config{MirrorDir: filepath.Join(root, "mirrors"), WorkDir: "work"}); err == nil {
		t.Error("accepted a relative work dir")
	}
}

func TestNew_RequiresGit(t *testing.T) {
	root := t.TempDir()
	_, err := New(Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
		GitBinary: filepath.Join(root, "no-such-git"),
	})
	if err == nil {
		t.Fatal("accepted a git binary that does not exist")
	}
	if got := CodeOf(err); got != protocol.ErrSource {
		t.Errorf("code = %s, want %s", got, protocol.ErrSource)
	}
}

func TestNew_ReportsUncreatableDirectories(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "a-file-not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := New(Config{
		MirrorDir: filepath.Join(blocker, "mirrors"),
		WorkDir:   filepath.Join(root, "work"),
	}); err == nil {
		t.Error("New succeeded with an uncreatable mirror dir")
	}
	if _, err := New(Config{
		MirrorDir: filepath.Join(root, "mirrors"),
		WorkDir:   filepath.Join(blocker, "work"),
	}); err == nil {
		t.Error("New succeeded with an uncreatable work dir")
	}
}

// TestCheckSameFilesystem_RejectsWhatWouldSilentlyCopy is the check that earns
// its place. `git clone --local` hardlinks the object store; across
// filesystems git falls back to copying it with no error and no warning, and
// every run on the machine gets slower for reasons nobody attributes to the
// right cause.
//
// The negative case needs two real filesystems, which a unit test cannot
// conjure, so it is exercised through a directory that cannot be linked into
// at all — the same failure the check reports, arrived at by a different
// route. A genuine cross-device case is left to `latem doctor` on a machine
// with a second volume.
func TestCheckSameFilesystem_RejectsWhatWouldSilentlyCopy(t *testing.T) {
	root := t.TempDir()
	mirrors := filepath.Join(root, "mirrors")
	work := filepath.Join(root, "work")
	for _, d := range []string{mirrors, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := CheckSameFilesystem(mirrors, work); err != nil {
		t.Fatalf("two directories in one temp dir were reported as different filesystems: %v", err)
	}

	if err := CheckSameFilesystem(mirrors, filepath.Join(root, "does-not-exist")); err == nil {
		t.Error("reported hardlink support into a directory that is not there")
	}
	if err := CheckSameFilesystem(filepath.Join(root, "does-not-exist"), work); err == nil {
		t.Error("probed a mirror directory that is not there")
	}
}

// TestCheckSameFilesystem_LeavesNothingBehind. The probe runs at every agent
// start; a leaked file per start accumulates in a directory the operator
// believes contains only mirrors.
func TestCheckSameFilesystem_LeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	mirrors := filepath.Join(root, "mirrors")
	work := filepath.Join(root, "work")
	for _, d := range []string{mirrors, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	for range 3 {
		if err := CheckSameFilesystem(mirrors, work); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{mirrors, work} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("%s holds %d entries after three probes", d, len(entries))
		}
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{
		"git@github.com:me/myapp.git",
		"ssh://git@forgejo.internal/me/api.git",
		"https://example.com/me/app.git",
		"/srv/git/app.git",
		"file:///srv/git/app.git",
	}
	for _, u := range ok {
		if err := validateURL(u); err != nil {
			t.Errorf("rejected a legitimate url %q: %v", u, err)
		}
	}
}

func TestValidateRepoKey_AcceptsRealKeys(t *testing.T) {
	for _, key := range []string{"myapp", "api", "my-app", "my_app", "example.com", "a", "A1"} {
		if err := (Repo{Key: key, URL: "https://example.com/x.git"}).validate(); err != nil {
			t.Errorf("rejected a legitimate repo key %q: %v", key, err)
		}
	}
}

func TestErrorFormatting(t *testing.T) {
	base := codeErr(protocol.ErrSource, "something broke")
	if got := base.Error(); got != "E_SOURCE: something broke" {
		t.Errorf("Error() = %q", got)
	}
	wrapped := wrapErr(protocol.ErrSource, base, "while fetching")
	if got := wrapped.Error(); got != "E_SOURCE: while fetching: E_SOURCE: something broke" {
		t.Errorf("wrapped Error() = %q", got)
	}
	if !errors.Is(wrapped, error(base)) {
		t.Error("Unwrap did not return the cause")
	}
	if CodeOf(wrapped) != protocol.ErrSource {
		t.Errorf("CodeOf = %s", CodeOf(wrapped))
	}
	if CodeOf(os.ErrNotExist) != "" {
		t.Errorf("CodeOf on a foreign error = %s, want empty", CodeOf(os.ErrNotExist))
	}
	bare := &Error{Code: protocol.ErrSource}
	if got := bare.Error(); got != "E_SOURCE" {
		t.Errorf("bare Error() = %q", got)
	}
	causeOnly := &Error{Code: protocol.ErrSource, Err: os.ErrNotExist}
	if got := causeOnly.Error(); got != "E_SOURCE: file does not exist" {
		t.Errorf("cause-only Error() = %q", got)
	}
}

func TestGitError_ReportsArgvAndStderr(t *testing.T) {
	f := newFixture(t)
	_, err := f.mgr.run(t.Context(), f.mgr.MirrorDir(), "rev-parse", "--verify", "nope")
	if err == nil {
		t.Fatal("rev-parse in a non-repository succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"rev-parse", "--verify", "nope"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}

	// The exit status has to survive the wrapping, because a caller
	// distinguishing "git said no" from "git could not run" needs it.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("could not recover the exit status from %v", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Error("exit code is 0 on a failed command")
	}

	// A gitError with no stderr still names the command.
	bare := &gitError{args: []string{"status"}, err: os.ErrNotExist}
	if got := bare.Error(); !strings.Contains(got, "status") {
		t.Errorf("gitError without stderr = %q", got)
	}
}
