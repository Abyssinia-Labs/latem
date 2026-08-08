package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// migrationFS carries the schema in the binary.
//
// Embedding is what makes `latem agent` on a fresh machine a single file: no
// migration directory to install, no path to get wrong, and no way for the
// schema to be a different version from the code that queries it.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationDir = "migrations"

// migration is one file, already parsed.
type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies every embedded migration not yet recorded, in version order.
//
// It is safe to call on an empty database, on a fully migrated one, and on a
// populated one mid-way through the sequence — which is the only interesting
// case, because it is the one that runs on a machine with a month of history
// after an upgrade. Each file applies in its own transaction, so a failure
// leaves the database at the last version that applied cleanly rather than
// half-way through one.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version    INTEGER PRIMARY KEY,
		  name       TEXT NOT NULL,
		  applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range all {
		if slices.Contains(applied, m.version) {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) appliedVersions(ctx context.Context) ([]int, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: reading schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: reading schema_migrations: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading schema_migrations: %w", err)
	}
	return out, nil
}

// applyMigration runs one file and records it in the same transaction. Doing
// both atomically is the whole reason the ledger exists: a migration that
// applied but was not recorded would be applied again on the next start, and
// `CREATE TABLE runs` is not idempotent.
func (d *DB) applyMigration(ctx context.Context, m migration) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
		_, err := t.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, toUnix(time.Now()))
		if err != nil {
			return fmt.Errorf("store: recording migration %s: %w", m.name, err)
		}
		return nil
	})
}

// loadMigrations parses the embedded directory. Names are NNNN_description.sql
// and the numeric prefix is the version, so ordering is explicit in the
// filename rather than implicit in lexical sort rules nobody remembers.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("store: reading embedded migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := map[int]string{}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %s and %s share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(migrationFS, path.Join(migrationDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: reading migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}

	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	return out, nil
}

func parseVersion(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("store: migration %q is not NNNN_description.sql", name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("store: migration %q has no positive version prefix", name)
	}
	return v, nil
}
