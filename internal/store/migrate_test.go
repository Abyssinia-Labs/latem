package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

func objectSQL(t *testing.T, d *DB, name string) string {
	t.Helper()

	var stmt sql.NullString
	err := d.db.QueryRowContext(t.Context(),
		`SELECT sql FROM sqlite_master WHERE name = ?`, name).Scan(&stmt)
	if err != nil {
		t.Fatalf("reading schema for %q: %v", name, err)
	}
	return stmt.String
}

func objectNames(t *testing.T, d *DB) []string {
	t.Helper()

	rows, err := d.db.QueryContext(t.Context(), `SELECT name FROM sqlite_master ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMigrate_CreatesTheSpecSchema checks that every §22 object exists. It is
// a list rather than a spot check because a migration that half-applied is the
// failure mode worth catching, and a missing index is invisible until the
// machine is slow.
func TestMigrate_CreatesTheSpecSchema(t *testing.T) {
	d := newTestDB(t)
	names := objectNames(t, d)

	for _, want := range []string{
		"runs", "queue", "artifacts", "cache_meta",
		"runs_state_idx", "runs_repo_created_idx", "runs_expiry_idx",
		"schema_migrations",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("schema object %q missing; have %v", want, names)
		}
	}
}

// TestMigrate_RunsColumns pins the runs table to §22 exactly. A column added
// to the Go struct without a migration, or dropped from the migration without
// the struct, fails here instead of at the first query that needs it.
func TestMigrate_RunsColumns(t *testing.T) {
	d := newTestDB(t)

	rows, err := d.db.QueryContext(t.Context(), `SELECT name FROM pragma_table_info('runs') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}

	want := []string{
		"id", "repo", "job", "ref", "ref_normalized", "sha", "runner", "state",
		"exit_code", "error_code", "error_message", "origin_kind", "origin_actor",
		"cancel_reason", "inputs_json", "created_at", "started_at", "finished_at",
		"expires_at", "reported_state", "last_acked_seq",
	}
	if !slices.Equal(got, want) {
		t.Errorf("runs columns =\n %v\nwant\n %v", got, want)
	}

	// The projection every read shares must name exactly those columns, in order.
	var projected []string
	for _, c := range strings.Split(runColumns, ",") {
		projected = append(projected, strings.TrimSpace(c))
	}
	if !slices.Equal(projected, want) {
		t.Errorf("runColumns =\n %v\nwant\n %v", projected, want)
	}
}

// stateList pulls the quoted state names out of an index definition.
var stateListRE = regexp.MustCompile(`\(([^()]*'[^()]*)\)`)

func indexStates(t *testing.T, ddl string) []string {
	t.Helper()

	m := stateListRE.FindStringSubmatch(ddl)
	if m == nil {
		t.Fatalf("no state list found in %q", ddl)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(part), "'"))
	}
	sort.Strings(out)
	return out
}

// TestMigrate_PartialIndexesMatchProtocol is the drift guard RUNNER_SPEC §22
// asks for: runs_state_idx is defined as the complement of the terminal set,
// so a state added to pkg/protocol without a migration here silently changes
// which rows the hot index covers — and nothing else would notice.
func TestMigrate_PartialIndexesMatchProtocol(t *testing.T) {
	d := newTestDB(t)

	terminal := make([]string, 0, len(protocol.TerminalStates()))
	for _, s := range protocol.TerminalStates() {
		terminal = append(terminal, string(s))
	}
	sort.Strings(terminal)

	if got := indexStates(t, objectSQL(t, d, "runs_state_idx")); !slices.Equal(got, terminal) {
		t.Errorf("runs_state_idx excludes %v, but protocol.TerminalStates() is %v", got, terminal)
	}

	var waiting []string
	for _, s := range waitingStates() {
		if !s.IsWaiting() {
			t.Errorf("%s is in waitingStates() but RunState.IsWaiting() says otherwise", s)
		}
		waiting = append(waiting, string(s))
	}
	sort.Strings(waiting)

	if got := indexStates(t, objectSQL(t, d, "runs_expiry_idx")); !slices.Equal(got, waiting) {
		t.Errorf("runs_expiry_idx covers %v, want the waiting states %v", got, waiting)
	}

	// The runtime predicates are generated from the same source and must appear
	// in the index definitions *verbatim*, down to the order of the IN list:
	// sqlite compares the query's WHERE clause against the index predicate
	// expression by expression, so a reordered list is a different expression
	// and the index silently goes unused.
	for _, tc := range []struct{ predicate, ddl string }{
		{nonTerminalPredicate, objectSQL(t, d, "runs_state_idx")},
		{waitingPredicate, objectSQL(t, d, "runs_expiry_idx")},
	} {
		if !strings.Contains(normalizeSQL(tc.ddl), normalizeSQL(tc.predicate)) {
			t.Errorf("predicate %q does not appear in index definition %q", tc.predicate, tc.ddl)
		}
	}
}

// normalizeSQL collapses the whitespace the DDL is formatted with so a
// predicate can be compared against it textually.
func normalizeSQL(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestMigrate_Twice covers the ordinary agent restart. The second call must be
// a no-op: CREATE TABLE runs is not idempotent, so a ledger that failed to
// record an applied migration would make every restart after the first fail.
func TestMigrate_Twice(t *testing.T) {
	d := newTestDB(t) // migrates once

	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := d.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := len(mustLoadMigrations(t)); n != want {
		t.Errorf("schema_migrations has %d rows after two migrations, want %d", n, want)
	}
}

// TestMigrate_PopulatedDatabase is the case that actually happens on upgrade:
// a machine with history, migrating forward. Data must survive, and the second
// pass must not re-run anything.
func TestMigrate_PopulatedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	d := newTestDBAt(t, path)

	run := mustCreate(t, d, testRun("populated-1", protocol.StateRunning))
	if err := d.Enqueue(t.Context(), run.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordArtifacts(t.Context(), run.ID, []protocol.ArtifactRef{{
		Path: "build/app.ipa", SizeBytes: 1 << 20, SHA256: "abc",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen exactly as a restarted agent would, and migrate again.
	reopened, err := Open(t.Context(), path, &fakeRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if err := reopened.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate on a populated database: %v", err)
	}

	got, err := reopened.Get(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("run lost across migration: %v", err)
	}
	if got.State != protocol.StateRunning {
		t.Errorf("state = %q after migration, want %q", got.State, protocol.StateRunning)
	}
	arts, err := reopened.Artifacts(t.Context(), run.ID)
	if err != nil || len(arts) != 1 {
		t.Errorf("artifacts = %v, %v; want one row preserved", arts, err)
	}
}

// TestMigrate_ForwardFromPartialLedger simulates an agent that has version 1
// applied and finds a later migration in a new binary. The already-applied file
// must be skipped rather than re-run.
func TestMigrate_ForwardFromPartialLedger(t *testing.T) {
	d := newTestDB(t)

	if _, err := d.db.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	// With the ledger row gone, re-running must fail loudly rather than
	// silently doing nothing — which is the evidence that a recorded migration
	// is genuinely skipped rather than being idempotent by luck.
	if err := d.Migrate(t.Context()); err == nil {
		t.Fatal("re-applying migration 1 succeeded; the ledger is not what prevents re-application")
	}
}

func mustLoadMigrations(t *testing.T) []migration {
	t.Helper()

	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations are embedded; the binary would start against an empty database")
	}
	return all
}

func TestLoadMigrations_AreOrderedAndNamed(t *testing.T) {
	all := mustLoadMigrations(t)

	for i, m := range all {
		if i > 0 && m.version <= all[i-1].version {
			t.Errorf("migration %s is out of order after %s", m.name, all[i-1].name)
		}
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %s is empty", m.name)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{name: "0001_init.sql", want: 1},
		{name: "0012_add_column.sql", want: 12},
		{name: "init.sql", wantErr: true},
		{name: "0000_zero.sql", wantErr: true},
		{name: "x_init.sql", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVersion(tc.name)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseVersion(%q) error = %v, wantErr %v", tc.name, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("parseVersion(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestMigrate_ContextCancelled makes sure a cancelled startup does not leave a
// half-built schema behind that the next start would trip over.
func TestMigrate_ContextCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	d, err := Open(t.Context(), path, &fakeRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := d.Migrate(ctx); err == nil {
		t.Fatal("Migrate with a cancelled context succeeded")
	}
	if slices.Contains(objectNames(t, d), "runs") {
		t.Error("a cancelled migration left the runs table behind")
	}
}
