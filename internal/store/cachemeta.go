package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CacheMeta is one row of the cache_meta table (§22).
//
// It is deliberately not cache.Info: that type carries Source, which is a
// filesystem path or a docker volume name that the cache manager derives from
// (runner, name) and §22 does not store. Mirroring a neighbour's struct into a
// schema is how a column nobody meant to persist ends up persisted.
type CacheMeta struct {
	// Name is the volume or directory name, unique across runners because it
	// embeds the runner (§14.1).
	Name   string
	Runner string
	// SizeBytes and LastUsedAt drive §14.4's eviction by size and access time.
	SizeBytes  int64
	LastUsedAt time.Time
}

// UpsertCache records a cache's size and last use.
//
// Not part of [Store] — §22 defines the table but the frozen interface has no
// methods for it, so this is the concrete type's API until that is amended.
// Sizing is a measurement the cache manager takes; persisting it here is what
// lets `latem cache ls` and the daily prune answer "what is big and cold?"
// without walking every volume on a machine that may hold hundreds of gigabytes.
func (d *DB) UpsertCache(ctx context.Context, m CacheMeta) error {
	switch {
	case m.Name == "":
		return errors.New("store: UpsertCache: name is empty")
	case m.Runner == "":
		return errors.New("store: UpsertCache: runner is empty")
	}
	var lastUsed any
	if !m.LastUsedAt.IsZero() {
		lastUsed = toUnix(m.LastUsedAt)
	}

	_, err := d.db.ExecContext(ctx, `
		INSERT INTO cache_meta (name, runner, size_bytes, last_used_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		  runner       = excluded.runner,
		  size_bytes   = excluded.size_bytes,
		  last_used_at = excluded.last_used_at`,
		m.Name, m.Runner, m.SizeBytes, lastUsed)
	if err != nil {
		return fmt.Errorf("store: UpsertCache %s: %w", m.Name, err)
	}
	return nil
}

// Caches lists cache metadata, coldest first — the order §14.4 evicts in.
func (d *DB) Caches(ctx context.Context) ([]CacheMeta, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT name, runner, size_bytes, last_used_at FROM cache_meta
		 ORDER BY last_used_at ASC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: Caches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CacheMeta
	for rows.Next() {
		var (
			m        CacheMeta
			size     sql.NullInt64
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&m.Name, &m.Runner, &size, &lastUsed); err != nil {
			return nil, fmt.Errorf("store: Caches: %w", err)
		}
		m.SizeBytes = size.Int64
		if t := scanTime(lastUsed); t != nil {
			m.LastUsedAt = *t
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: Caches: %w", err)
	}
	return out, nil
}

// DeleteCache forgets a cache, for when §14.4 evicts the underlying volume.
// Deleting metadata for a cache that is not there is not an error: the point of
// the call is that it should be gone.
func (d *DB) DeleteCache(ctx context.Context, name string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM cache_meta WHERE name = ?`, name); err != nil {
		return fmt.Errorf("store: DeleteCache %s: %w", name, err)
	}
	return nil
}
