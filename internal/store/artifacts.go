package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// RecordArtifacts stores what a run produced.
//
// Names, sizes, and digests only — content never crosses this boundary or any
// other (§17). An .ipa stays on the machine that signed it.
//
// Upserting by (run_id, path) makes a re-collection idempotent, which matters
// because collecting runs even after a failure and may run twice if a run is
// retried against the same id.
func (d *DB) RecordArtifacts(ctx context.Context, runID string, refs []protocol.ArtifactRef) error {
	if len(refs) == 0 {
		return nil
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		var exists int
		switch err := t.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID).Scan(&exists); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: run %s", ErrNotFound, runID)
		case err != nil:
			return fmt.Errorf("store: RecordArtifacts %s: %w", runID, err)
		}
		return insertArtifacts(ctx, t, runID, refs)
	})
}

// insertArtifacts is shared with [DB.Transition], where the artifacts arrive
// attached to the terminal update and must land in the same transaction as it.
func insertArtifacts(ctx context.Context, t *sql.Tx, runID string, refs []protocol.ArtifactRef) error {
	for _, a := range refs {
		if a.Path == "" {
			return fmt.Errorf("store: run %s: artifact with an empty path", runID)
		}
		_, err := t.ExecContext(ctx, `
			INSERT INTO artifacts (run_id, path, size_bytes, sha256, expires_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(run_id, path) DO UPDATE SET
			  size_bytes = excluded.size_bytes,
			  sha256     = excluded.sha256,
			  expires_at = excluded.expires_at`,
			runID, a.Path, a.SizeBytes, a.SHA256, nullTime(a.ExpiresAt))
		if err != nil {
			return fmt.Errorf("store: recording artifact %q for run %s: %w", a.Path, runID, err)
		}
	}
	return nil
}

// Artifacts returns a run's artifacts, ordered by path so a listing is stable
// between calls. An unknown run returns nothing rather than an error: a run
// with no artifacts and a run that never existed are the same answer to this
// question, and callers that care ask [DB.Get].
func (d *DB) Artifacts(ctx context.Context, runID string) ([]protocol.ArtifactRef, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT path, size_bytes, sha256, expires_at FROM artifacts
		 WHERE run_id = ? ORDER BY path ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: Artifacts %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []protocol.ArtifactRef
	for rows.Next() {
		var (
			a       protocol.ArtifactRef
			expires sql.NullInt64
		)
		if err := rows.Scan(&a.Path, &a.SizeBytes, &a.SHA256, &expires); err != nil {
			return nil, fmt.Errorf("store: Artifacts %s: %w", runID, err)
		}
		a.ExpiresAt = scanTime(expires)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: Artifacts %s: %w", runID, err)
	}
	return out, nil
}
