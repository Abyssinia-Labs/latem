package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func TestRunNumber_IssuedInOrderFromOne(t *testing.T) {
	d := newTestDB(t)

	for i, id := range []string{"run-a", "run-b", "run-c"} {
		r := testRun(id, protocol.StateQueued)
		mustCreate(t, d, r)

		got, err := d.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(i + 1); got.Number != want {
			t.Fatalf("%s number = %d, want %d", id, got.Number, want)
		}
	}
}

// The defect this design exists for. §14.4's retention deletes old runs, so a
// number derived from the surviving rows — MAX(number)+1, or sqlite's rowid
// without AUTOINCREMENT — reissues the highest one after a prune. That value is
// the build number App Store Connect has already accepted, and the rejection
// arrives months later in an upload rather than here.
func TestRunNumber_SurvivesDeletingTheHighest(t *testing.T) {
	d := newTestDB(t)

	mustCreate(t, d, testRun("run-a", protocol.StateQueued))
	mustCreate(t, d, testRun("run-b", protocol.StateQueued))

	if _, err := d.db.ExecContext(t.Context(), `DELETE FROM runs WHERE id = 'run-b'`); err != nil {
		t.Fatal(err)
	}

	mustCreate(t, d, testRun("run-c", protocol.StateQueued))
	got, err := d.Get(t.Context(), "run-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 3 {
		t.Fatalf("number after pruning the highest = %d, want 3 — a number was reissued", got.Number)
	}
}

// Two runs must never be issued the same number, whatever the interleaving.
// The guarantee comes from BEGIN IMMEDIATE rather than from the SQL, so it is
// worth an actual concurrent test rather than a reading of the statement.
func TestRunNumber_ConcurrentCreatesAreUnique(t *testing.T) {
	d := newTestDB(t)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.Create(t.Context(), testRun("run-"+string(rune('a'+i)), protocol.StateQueued))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	runs, err := d.List(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	for _, r := range runs {
		if other, dup := seen[r.Number]; dup {
			t.Fatalf("runs %s and %s were both issued number %d", other, r.ID, r.Number)
		}
		seen[r.Number] = r.ID
	}
	if len(seen) != n {
		t.Fatalf("issued %d distinct numbers for %d runs", len(seen), n)
	}
}

// A caller that sets Number has decided a build number for itself, and the
// whole point is that exactly one thing decides it.
func TestRunNumber_CreateRefusesASuppliedNumber(t *testing.T) {
	d := newTestDB(t)

	r := testRun("run-a", protocol.StateQueued)
	r.Number = 99
	err := d.Create(t.Context(), r)
	if err == nil {
		t.Fatal("Create accepted a caller-supplied number")
	}
	if !strings.Contains(err.Error(), "assigned by the store") {
		t.Fatalf("error %q does not say why", err)
	}
}

// A machine with history gets numbered in creation order rather than left at
// zero, and the sequence continues past the backfill instead of colliding with
// it.
func TestRunNumber_BackfillsExistingHistory(t *testing.T) {
	path := t.TempDir() + "/state.db"

	// A database at 0003: open it, run every migration, then undo 0004 so the
	// rows look the way an upgrading machine's do.
	d := newTestDBAt(t, path)
	base := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	for i, id := range []string{"run-old-1", "run-old-2", "run-old-3"} {
		r := testRun(id, protocol.StateQueued)
		r.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		r.State = protocol.StateSucceeded
		mustCreate(t, d, r)
	}
	if _, err := d.db.ExecContext(t.Context(), `
		ALTER TABLE runs DROP COLUMN number;
		DROP TABLE sequences;
		DELETE FROM schema_migrations WHERE version = 4;`); err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("re-running 0004 over history: %v", err)
	}

	for i, id := range []string{"run-old-1", "run-old-2", "run-old-3"} {
		got, err := d.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(i + 1); got.Number != want {
			t.Errorf("%s backfilled to %d, want %d", id, got.Number, want)
		}
	}

	// And the first run after the upgrade continues the series rather than
	// reissuing a number an upload may already carry.
	mustCreate(t, d, testRun("run-new", protocol.StateQueued))
	got, err := d.Get(t.Context(), "run-new")
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 4 {
		t.Fatalf("the first run after the upgrade got %d, want 4", got.Number)
	}
}
