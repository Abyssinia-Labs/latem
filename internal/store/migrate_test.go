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
	"time"

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
		"runs", "queue", "artifacts", "cache_meta", "broker_state",
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
		"expires_at",
		// 0002, amendment A27. ALTER TABLE appends, so these come last and the
		// order here is the order on disk.
		//
		// reported_state and last_acked_seq used to sit between expires_at and
		// these two. 0003 moved them into broker_state and dropped them
		// (amendment A28): they were never properties of a run, they were
		// properties of a run *and a transport*, and leaving the columns behind
		// would have left a second source of truth for exactly the values a
		// partition depends on.
		"approved_at", "approved_sha",
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

// TestMigrate_0002OverAVersion1Database is the upgrade A27 actually ships into:
// a machine with a month of history, whose runs table has no approval columns
// at all. The rows must survive, read back as "never approved", and the new
// columns must be writable afterwards.
//
// The fixture is built by applying only migration 1 and inserting through raw
// SQL, because [DB.Create] projects the post-0002 column list and would not
// compile against the old schema — which is the point: this is the only way to
// exercise the transition from what was on disk to what is on disk now.
func TestMigrate_0002OverAVersion1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	d, err := Open(t.Context(), path, &fakeRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	all := mustLoadMigrations(t)
	if _, err := d.db.ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := d.applyMigration(t.Context(), all[0]); err != nil {
		t.Fatalf("applying 0001: %v", err)
	}

	_, err = d.db.ExecContext(t.Context(), `
		INSERT INTO runs (id, repo, job, ref, sha, state, origin_kind, inputs_json,
		                  created_at, last_acked_seq)
		VALUES ('r_old', 'myapp', 'build', 'feature/x', ?, 'awaiting_approval', 'cli', '{}', ?, 0)`,
		"0123456789abcdef0123456789abcdef01234567", toUnix(time.Now()))
	if err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating a version-1 database forward: %v", err)
	}

	got, err := d.Get(t.Context(), "r_old")
	if err != nil {
		t.Fatalf("run lost across 0002: %v", err)
	}
	if got.State != protocol.StateAwaitingApproval {
		t.Errorf("state = %s, want awaiting_approval", got.State)
	}
	if got.ApprovedAt != nil || got.ApprovedSHA != "" {
		t.Errorf("a pre-A27 row reads as approved: %v / %q", got.ApprovedAt, got.ApprovedSHA)
	}

	// The columns are not merely present, they work.
	now := time.Now().UTC().Truncate(time.Second)
	approved, err := d.Approve(t.Context(), "r_old", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Approve after migrating forward: %v", err)
	}
	if approved.ApprovedSHA == "" {
		t.Error("approve wrote no pin after the migration")
	}
}

// TestMigrate_0003PreservesLocalReportingState is the upgrade A28 ships into.
//
// A machine with a month of history has runs whose reported_state and
// last_acked_seq were written by the only transport that existed. Those values
// have to land under broker = 'local' and nowhere else: losing them would make
// the first start after the upgrade replay every run the machine has ever done
// and re-stream every log it still has, and putting them under 'remote' as well
// would tell a control plane it holds output it has never seen.
//
// The fixture applies 0001 and 0002 by hand and inserts through raw SQL,
// because [DB.Create] projects the post-0003 column list and cannot write the
// columns this migration exists to move.
func TestMigrate_0003PreservesLocalReportingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	d, err := Open(t.Context(), path, &fakeRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	all := mustLoadMigrations(t)
	if len(all) < 3 {
		t.Fatalf("expected at least three migrations, have %d", len(all))
	}
	if _, err := d.db.ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range all[:2] {
		if err := d.applyMigration(t.Context(), m); err != nil {
			t.Fatalf("applying %s: %v", m.name, err)
		}
	}

	now := toUnix(time.Now())
	// r_reported carries both values. r_quiet carries neither, and is here to
	// pin that the migration does not manufacture rows for runs that never had
	// anything to preserve.
	if _, err := d.db.ExecContext(t.Context(), `
		INSERT INTO runs (id, repo, job, ref, state, origin_kind, inputs_json,
		                  created_at, reported_state, last_acked_seq)
		VALUES ('r_reported', 'myapp', 'build', 'main', 'running', 'cli', '{}', ?, 'preparing', 812),
		       ('r_quiet',    'myapp', 'build', 'main', 'queued',  'cli', '{}', ?, NULL, 0)`,
		now, now); err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating a version-2 database forward: %v", err)
	}

	// The values moved, under 'local' and only 'local'.
	if got, err := d.LastAckedSeq(t.Context(), BrokerLocal, "r_reported"); err != nil || got != 812 {
		t.Errorf("LastAckedSeq(local) = %d, %v; want the pre-upgrade 812 preserved", got, err)
	}
	if got, err := d.LastAckedSeq(t.Context(), BrokerRemote, "r_reported"); err != nil || got != 0 {
		t.Errorf("LastAckedSeq(remote) = %d, %v; the local transport's resume point was copied to a transport that has never seen a frame", got, err)
	}

	unreportedLocal, err := d.Unreported(t.Context(), BrokerLocal)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range unreportedLocal {
		if r.ID == "r_reported" && r.State != protocol.StateRunning {
			t.Errorf("unexpected row %+v", r)
		}
	}
	// reported_state was 'preparing' and the run is 'running', so it still lags
	// — the point is that it lags by one transition rather than reading as
	// never reported at all.
	var reported string
	if err := d.db.QueryRowContext(t.Context(),
		`SELECT reported_state FROM broker_state WHERE run_id = 'r_reported' AND broker = 'local'`).
		Scan(&reported); err != nil {
		t.Fatalf("the pre-upgrade reported_state did not survive: %v", err)
	}
	if reported != "preparing" {
		t.Errorf("reported_state = %q under broker 'local', want the pre-upgrade %q", reported, "preparing")
	}

	var quietRows int
	if err := d.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM broker_state WHERE run_id = 'r_quiet'`).Scan(&quietRows); err != nil {
		t.Fatal(err)
	}
	if quietRows != 0 {
		t.Errorf("a run with nothing to preserve got %d broker_state rows", quietRows)
	}

	// And the old columns are gone rather than left as a second answer.
	for _, col := range []string{"reported_state", "last_acked_seq"} {
		var n int
		if err := d.db.QueryRowContext(t.Context(),
			`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = ?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("runs still has a %s column; two sources of truth for the same fact is the defect A28 removed", col)
		}
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
