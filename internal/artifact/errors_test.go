package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Every failure here is E_ARTIFACT, and the code is what a control plane maps
// to copy and to a retry button (§21). An error that loses it becomes "the run
// failed" with no further detail.
func TestError_CarriesTheCodeAndTheCause(t *testing.T) {
	cause := errors.New("permission denied")

	wrapped := artifactErr(cause, "opening %s", "app.ipa")
	if !strings.HasPrefix(wrapped.Error(), string(protocol.ErrArtifact)) {
		t.Errorf("Error() = %q, want it to lead with the code", wrapped.Error())
	}
	if !strings.Contains(wrapped.Error(), "app.ipa") || !strings.Contains(wrapped.Error(), "permission denied") {
		t.Errorf("Error() = %q, want both the context and the cause", wrapped.Error())
	}
	if !errors.Is(wrapped, cause) {
		t.Error("the cause is not reachable with errors.Is")
	}

	bare := artifactErr(nil, "glob %q escapes", "../x")
	if strings.Contains(bare.Error(), "%!") {
		t.Errorf("Error() = %q", bare.Error())
	}
}

func TestCode(t *testing.T) {
	if got := Code(nil); got != "" {
		t.Errorf("Code(nil) = %q, want empty", got)
	}
	if got := Code(errors.New("something else")); got != "" {
		t.Errorf("Code(other) = %q, want empty: only this package's errors are E_ARTIFACT", got)
	}
	if got := Code(artifactErr(nil, "x")); got != protocol.ErrArtifact {
		t.Errorf("Code = %q", got)
	}
}

// An unreadable directory the glob does reach is a real failure, and the
// message names the path relative to the workspace rather than leaking the
// host's layout.
func TestResolve_ReportsUnreadableDirectoriesItMustEnter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0 does not stop a read")
	}
	root := filepath.Join(t.TempDir(), "ws")
	build := filepath.Join(root, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(build, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(build, 0o755) })

	_, err := Resolve(root, "build/*.ipa")
	if err == nil {
		t.Fatal("an unreadable output directory was reported as no artifacts")
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("the message leaks the host path: %v", err)
	}
	if !strings.Contains(err.Error(), "build") {
		t.Errorf("the message does not name the directory: %v", err)
	}
}

func TestCollect_ReportsAnUnreadableArtifact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0 does not stop a read")
	}
	store := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})
	locked := filepath.Join(ws, "build", "app.ipa")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	if _, err := NewCollector(store).Collect(
		context.Background(), "run_1", ws, []Declaration{{Path: "build/*.ipa"}},
	); err == nil {
		t.Fatal("an unreadable artifact was reported as collected")
	}
}

func TestWithClock_NilKeepsTheExistingClock(t *testing.T) {
	c := NewCollector(t.TempDir())
	if c.WithClock(nil) != c {
		t.Error("WithClock(nil) replaced the collector")
	}
}
