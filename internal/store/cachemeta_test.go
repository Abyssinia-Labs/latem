package store

import (
	"testing"
	"time"
)

func TestUpsertCache_RoundTripAndColdestFirst(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	seed := []CacheMeta{
		{Name: "latem-cache-docker-gradle", Runner: "docker", SizeBytes: 22 << 30, LastUsedAt: base},
		{Name: "latem-cache-mac-tart-spm", Runner: "mac-tart", SizeBytes: 3 << 30, LastUsedAt: base.Add(-48 * time.Hour)},
		{Name: "latem-cache-docker-npm", Runner: "docker", SizeBytes: 1 << 30, LastUsedAt: base.Add(-time.Hour)},
	}
	for _, m := range seed {
		if err := d.UpsertCache(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}

	got, err := d.Caches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d caches, want 3", len(got))
	}
	// §14.4 evicts by access time, oldest first, so that is the listing order.
	want := []string{"latem-cache-mac-tart-spm", "latem-cache-docker-npm", "latem-cache-docker-gradle"}
	for i, m := range got {
		if m.Name != want[i] {
			t.Fatalf("Caches = %v, want coldest first %v", got, want)
		}
	}
	if got[0].Runner != "mac-tart" || got[0].SizeBytes != 3<<30 {
		t.Errorf("cache = %+v", got[0])
	}
	if !got[0].LastUsedAt.Equal(base.Add(-48 * time.Hour)) {
		t.Errorf("last_used_at = %v", got[0].LastUsedAt)
	}

	// A re-measurement updates in place rather than adding a row.
	updated := seed[0]
	updated.SizeBytes = 30 << 30
	updated.LastUsedAt = base.Add(time.Hour)
	if err := d.UpsertCache(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if got, err = d.Caches(t.Context()); err != nil || len(got) != 3 {
		t.Fatalf("Caches = %v, %v; want the same three rows", got, err)
	}
}

func TestUpsertCache_Rejects(t *testing.T) {
	d := newTestDB(t)

	if err := d.UpsertCache(t.Context(), CacheMeta{Runner: "docker"}); err == nil {
		t.Error("UpsertCache accepted an empty name")
	}
	// The runner is what scopes a cache (§14.1): two runners sharing one
	// ~/.gradle corrupt each other, so an unscoped row is not a valid record.
	if err := d.UpsertCache(t.Context(), CacheMeta{Name: "c"}); err == nil {
		t.Error("UpsertCache accepted an empty runner")
	}
}

func TestUpsertCache_NeverUsed(t *testing.T) {
	d := newTestDB(t)

	if err := d.UpsertCache(t.Context(), CacheMeta{Name: "fresh", Runner: "docker"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Caches(t.Context())
	if err != nil || len(got) != 1 {
		t.Fatalf("Caches = %v, %v", got, err)
	}
	if !got[0].LastUsedAt.IsZero() {
		t.Errorf("last_used_at = %v for a cache nothing has used, want the zero time", got[0].LastUsedAt)
	}
}

func TestDeleteCache(t *testing.T) {
	d := newTestDB(t)

	if err := d.UpsertCache(t.Context(), CacheMeta{Name: "doomed", Runner: "docker"}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteCache(t.Context(), "doomed"); err != nil {
		t.Fatal(err)
	}
	got, err := d.Caches(t.Context())
	if err != nil || len(got) != 0 {
		t.Fatalf("Caches = %v, %v; want empty", got, err)
	}

	// Deleting metadata for a cache that is already gone is the outcome asked
	// for, not a failure — the prune path calls this after evicting a volume.
	if err := d.DeleteCache(t.Context(), "doomed"); err != nil {
		t.Errorf("second DeleteCache = %v, want nil", err)
	}
}
