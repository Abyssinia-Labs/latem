package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sha256 of "payload", computed independently of the code under test. A digest
// the implementation computes both ways proves nothing.
const payloadSHA = "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5"

func TestCollect_CopiesPreservingRelativePaths(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{
		"build/outputs/app.ipa": "payload",
		"build/reports/log.txt": "noise",
	})

	refs, err := NewCollector(root).Collect(
		context.Background(), "run_1", ws,
		[]Declaration{{Path: "build/outputs/*.ipa"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want one", refs)
	}

	ref := refs[0]
	if ref.Path != "build/outputs/app.ipa" {
		t.Errorf("Path = %q, want the workspace-relative path", ref.Path)
	}
	if ref.SizeBytes != int64(len("payload")) {
		t.Errorf("SizeBytes = %d", ref.SizeBytes)
	}
	if ref.SHA256 != payloadSHA {
		t.Errorf("SHA256 = %q, want %q", ref.SHA256, payloadSHA)
	}
	if ref.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil for a declaration with no retain", ref.ExpiresAt)
	}

	stored := filepath.Join(root, "artifacts", "run_1", "build", "outputs", "app.ipa")
	body, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("artifact was not stored at the expected path: %v", err)
	}
	if string(body) != "payload" {
		t.Errorf("stored content = %q", body)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "run_1", "build", "reports")); !os.IsNotExist(err) {
		t.Error("an undeclared directory was collected")
	}
}

// Amendment A6. Every path the agent builds from a run id is a place a control
// plane sending {"id": "../../../../tmp/x"} can write outside ~/.latem.
func TestCollect_RefusesInvalidRunIDs(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"app.ipa": "payload"})

	ids := []string{
		"../escape",
		"../../../../tmp/x",
		"a/b",
		".",
		"..",
		"",
		"-leading-dash",
		"run id with spaces",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if _, err := NewCollector(root).Collect(
				context.Background(), id, ws, []Declaration{{Path: "*.ipa"}},
			); err == nil {
				t.Fatalf("Collect accepted run id %q", id)
			}
		})
	}

	// Nothing may have been created anywhere near the artifact root.
	entries, err := os.ReadDir(filepath.Join(root, "artifacts"))
	if err == nil && len(entries) > 0 {
		t.Errorf("an invalid run id left %d entries behind", len(entries))
	}
}

func TestCollect_SetsExpiryFromRetention(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	refs, err := NewCollector(root).
		WithClock(func() time.Time { return now }).
		Collect(context.Background(), "run_1", ws, []Declaration{
			{Path: "build/*.ipa", Retain: 7 * 24 * time.Hour},
		})
	if err != nil {
		t.Fatal(err)
	}
	if refs[0].ExpiresAt == nil || !refs[0].ExpiresAt.Equal(now.Add(7*24*time.Hour)) {
		t.Errorf("ExpiresAt = %v, want %v", refs[0].ExpiresAt, now.Add(7*24*time.Hour))
	}
}

// Two declarations matching one file is a job author asking for it to live the
// longer of the two. Taking the first answer would delete a 30-day artifact
// three weeks early.
func TestCollect_OverlappingDeclarationsKeepTheLongestRetention(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	c := NewCollector(root).WithClock(func() time.Time { return now })
	refs, err := c.Collect(context.Background(), "run_1", ws, []Declaration{
		{Path: "build/*.ipa", Retain: 7 * 24 * time.Hour},
		{Path: "build/app.ipa", Retain: 30 * 24 * time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want the file reported once", refs)
	}
	if refs[0].ExpiresAt == nil || !refs[0].ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("ExpiresAt = %v, want the longer retention", refs[0].ExpiresAt)
	}
}

func TestCollect_NoRetentionBeatsAnyRetention(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})

	refs, err := NewCollector(root).Collect(context.Background(), "run_1", ws, []Declaration{
		{Path: "build/*.ipa", Retain: time.Hour},
		{Path: "build/app.ipa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if refs[0].ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v, want nil: a declaration with no retain is the longest", refs[0].ExpiresAt)
	}
}

func TestCollect_EmptyResultLeavesNoDirectory(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})

	refs, err := NewCollector(root).Collect(
		context.Background(), "run_1", ws, []Declaration{{Path: "build/*.dSYM"}},
	)
	if err != nil {
		t.Fatalf("a declaration matching nothing must not fail the run: %v", err)
	}
	if refs != nil {
		t.Errorf("refs = %+v, want none", refs)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "run_1")); !os.IsNotExist(err) {
		t.Error("an empty run directory was left behind")
	}
}

func TestCollect_PropagatesEscapeRejection(t *testing.T) {
	root := t.TempDir()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	write(t, filepath.Join(base, "outside", "loot.txt"), "secret")
	write(t, filepath.Join(ws, "build", "app.ipa"), "payload")
	symlink(t, filepath.Join(base, "outside", "loot.txt"), filepath.Join(ws, "build", "leak.ipa"))

	_, err := NewCollector(root).Collect(
		context.Background(), "run_1", ws, []Declaration{{Path: "build/*.ipa"}},
	)
	if err == nil {
		t.Fatal("Collect copied an artifact that escapes the workspace")
	}
	if _, statErr := os.Stat(filepath.Join(root, "artifacts", "run_1", "build", "leak.ipa")); statErr == nil {
		t.Error("the escaping file was written to the artifact store")
	}
}

func TestCollect_HonoursContextCancellation(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewCollector(root).Collect(
		ctx, "run_1", ws, []Declaration{{Path: "build/*.ipa"}},
	); err == nil {
		t.Fatal("Collect ignored a cancelled context")
	}
}

func TestCollect_StoresOwnerOnly(t *testing.T) {
	root := t.TempDir()
	ws := workspace(t, map[string]string{"build/app.ipa": "payload"})

	if _, err := NewCollector(root).Collect(
		context.Background(), "run_1", ws, []Declaration{{Path: "build/*.ipa"}},
	); err != nil {
		t.Fatal(err)
	}

	// An .ipa embeds a provisioning profile and an .aab is what ships to
	// users. On a machine with more than one login, they are nobody else's.
	info, err := os.Stat(filepath.Join(root, "artifacts", "run_1", "build", "app.ipa"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("stored artifact mode = %o, want owner-only", perm)
	}
}

// Resolve establishes containment; the copy re-establishes it at the moment of
// the open. os.Root refuses to traverse a symlink out of its root at all, so a
// link substituted after resolution — the run's environment may still be alive
// and writing — cannot redirect the read.
func TestCopyFile_RefusesToReadThroughAnEscapingLink(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	write(t, filepath.Join(base, "outside", "loot.txt"), "secret")
	write(t, filepath.Join(ws, "build", "keep"), "x")
	symlink(t, filepath.Join(base, "outside", "loot.txt"), filepath.Join(ws, "build", "swapped.ipa"))

	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	dst := filepath.Join(t.TempDir(), "app.ipa")
	if _, _, err := copyFile(root, "build/swapped.ipa", dst); err == nil {
		t.Fatal("the copy read through a symlink pointing out of the workspace")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a destination file was created for a refused read")
	}
}
