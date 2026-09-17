package store

import (
	"context"
	"database/sql"
	"fmt"
)

// seqRunNumber is the sequence issuing [Run.Number] (amendment A36).
//
// Named rather than implicit because a second sequence is foreseeable and a
// table with one hardcoded row would have to be migrated to grow a second.
const seqRunNumber = "run_number"

// nextSequence issues the next value of a named sequence.
//
// # Why not MAX(number) + 1, and why not a rowid
//
// Both reuse a value after a delete, and §14.4's retention deletes old runs.
// Reissuing a build number App Store Connect has already accepted is the exact
// failure A36 exists to prevent, and it would appear only on a machine old
// enough to have pruned — months after the code was written, in an upload
// rather than in a test. The sequence row is never pruned, so the value it
// holds is the highest ever issued rather than the highest currently stored.
//
// The caller must already be inside a transaction. [DB.tx] opens BEGIN
// IMMEDIATE, which is what makes this read-modify-write safe against a
// concurrent Create: two writers cannot both hold the reservation, so two runs
// cannot be issued the same number.
func nextSequence(ctx context.Context, t *sql.Tx, name string) (int64, error) {
	var value int64
	err := t.QueryRowContext(ctx, `
		INSERT INTO sequences (name, value) VALUES (?, 1)
		  ON CONFLICT(name) DO UPDATE SET value = value + 1
		  RETURNING value`, name).Scan(&value)
	if err != nil {
		return 0, fmt.Errorf("store: issuing %s: %w", name, err)
	}
	return value, nil
}
