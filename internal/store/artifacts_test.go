package store

import (
	"errors"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestRecordArtifacts_RoundTripAndOrder(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateCollecting))

	expires := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	refs := []protocol.ArtifactRef{
		{Path: "build/app.ipa", SizeBytes: 287 << 20, SHA256: "aa", ExpiresAt: &expires},
		{Path: "build/app.dSYM.zip", SizeBytes: 12 << 20, SHA256: "bb"},
	}
	if err := d.RecordArtifacts(t.Context(), "r", refs); err != nil {
		t.Fatal(err)
	}

	got, err := d.Artifacts(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artifacts, want 2", len(got))
	}
	// Ordered by path, so a listing is stable between calls.
	if got[0].Path != "build/app.dSYM.zip" || got[1].Path != "build/app.ipa" {
		t.Errorf("order = %q, %q", got[0].Path, got[1].Path)
	}
	if got[1].SizeBytes != 287<<20 || got[1].SHA256 != "aa" {
		t.Errorf("ipa = %+v", got[1])
	}
	if got[1].ExpiresAt == nil || !got[1].ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", got[1].ExpiresAt, expires)
	}
	if got[0].ExpiresAt != nil {
		t.Errorf("an artifact with no expiry came back with %v", got[0].ExpiresAt)
	}
}

// TestRecordArtifacts_Upserts: collecting runs even after a failure and may run
// again for a retried run id, so recording the same path twice must update it
// rather than fail on the primary key.
func TestRecordArtifacts_Upserts(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateCollecting))

	first := []protocol.ArtifactRef{{Path: "out/app.apk", SizeBytes: 1, SHA256: "old"}}
	second := []protocol.ArtifactRef{{Path: "out/app.apk", SizeBytes: 99, SHA256: "new"}}

	if err := d.RecordArtifacts(t.Context(), "r", first); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordArtifacts(t.Context(), "r", second); err != nil {
		t.Fatalf("re-recording the same path: %v", err)
	}

	got, err := d.Artifacts(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for one path, want 1", len(got))
	}
	if got[0].SizeBytes != 99 || got[0].SHA256 != "new" {
		t.Errorf("artifact = %+v, want the second recording", got[0])
	}
}

func TestRecordArtifacts_Rejects(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateCollecting))

	err := d.RecordArtifacts(t.Context(), "ghost", []protocol.ArtifactRef{{Path: "x", SHA256: "y"}})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("RecordArtifacts for an unknown run = %v, want ErrNotFound", err)
	}

	// An empty path would become an artifacts row nothing can fetch.
	if err := d.RecordArtifacts(t.Context(), "r", []protocol.ArtifactRef{{SizeBytes: 1}}); err == nil {
		t.Error("RecordArtifacts accepted an empty path")
	}

	// Nothing to record is not an error; a job with no artifact globs is normal.
	if err := d.RecordArtifacts(t.Context(), "r", nil); err != nil {
		t.Errorf("RecordArtifacts with no refs = %v, want nil", err)
	}
}

// TestArtifacts_UnknownRun: "no artifacts" and "no such run" are the same
// answer to this question. Callers that need the difference call Get.
func TestArtifacts_UnknownRun(t *testing.T) {
	d := newTestDB(t)

	got, err := d.Artifacts(t.Context(), "ghost")
	if err != nil {
		t.Fatalf("Artifacts of an unknown run = %v, want no error", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

// TestRecordArtifacts_PartialFailureRollsBack: the whole set lands or none of
// it does, so a listing never shows half of what a run produced.
func TestRecordArtifacts_PartialFailureRollsBack(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateCollecting))

	err := d.RecordArtifacts(t.Context(), "r", []protocol.ArtifactRef{
		{Path: "out/good.apk", SizeBytes: 1, SHA256: "a"},
		{Path: "", SizeBytes: 2, SHA256: "b"},
	})
	if err == nil {
		t.Fatal("RecordArtifacts accepted a set containing an empty path")
	}

	got, err := d.Artifacts(t.Context(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("the good half of a rejected set was persisted: %+v", got)
	}
}
