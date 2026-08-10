package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Abyssinia-Labs/latem/internal/logstream"
	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// fakeRedactor is a literal-substitution stand-in for the real redactor.
//
// The point of using a fake here is that the assertions can be exact: a test
// asserting "the token is not in the column" passes trivially if the redactor
// is broken and stores nothing, whereas "the column contains REDACTED where the
// token was" only passes if the value went through a redactor. The real
// implementation is exercised in one test below, to pin the interface.
type fakeRedactor struct{ secrets []string }

func (f *fakeRedactor) Redact(line string) string {
	for _, s := range f.secrets {
		if s == "" {
			continue
		}
		line = strings.ReplaceAll(line, s, "REDACTED")
	}
	return line
}

func newTestDB(t *testing.T, secrets ...string) *DB {
	t.Helper()
	return newTestDBAt(t, filepath.Join(t.TempDir(), "state.db"), secrets...)
}

func newTestDBAt(t *testing.T, path string, secrets ...string) *DB {
	t.Helper()

	d, err := Open(t.Context(), path, &fakeRedactor{secrets: secrets})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

// testRun is a minimally valid run. Tests override the fields they are about.
func testRun(id string, state protocol.RunState) Run {
	return Run{
		ID:         id,
		Repo:       "myapp",
		Job:        "build",
		Ref:        "main",
		State:      state,
		OriginKind: "cli",
		CreatedAt:  time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
	}
}

func mustCreate(t *testing.T, d *DB, r Run) Run {
	t.Helper()
	if err := d.Create(t.Context(), r); err != nil {
		t.Fatalf("Create %s: %v", r.ID, err)
	}
	return r
}

// TestOpen_RefusesNilRedactor guards INV-4 at the one place it could be
// silently skipped. A nil interface satisfies the signature and would write
// inputs_json verbatim, which is a leak with no symptom.
func TestOpen_RefusesNilRedactor(t *testing.T) {
	if _, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"), nil); err == nil {
		t.Fatal("Open with a nil redactor succeeded; inputs would be persisted unscrubbed")
	}
}

func TestOpen_RefusesEmptyPath(t *testing.T) {
	if _, err := Open(t.Context(), "", &fakeRedactor{}); err == nil {
		t.Fatal("Open with an empty path succeeded")
	}
}

// TestOpen_EnablesWAL fails if the journal mode silently did not take. Without
// WAL a reader blocks a writer, and the symptom is intermittent SQLITE_BUSY
// under concurrency rather than anything that points at the journal mode.
func TestOpen_EnablesWAL(t *testing.T) {
	d := newTestDB(t)

	var mode string
	if err := d.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

// TestOpen_CreatesDirectoryAndSecuresFile covers the fresh-machine path: no
// ~/.latem yet, and a state file no other user on a shared build box can read.
func TestOpen_CreatesDirectoryAndSecuresFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.db")
	newTestDBAt(t, path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state.db was not created: %v", err)
	}
	if got := info.Mode().Perm(); got != FileMode {
		t.Errorf("state.db mode = %o, want %o", got, FileMode)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dir.Mode().Perm(); got != DirMode {
		t.Errorf("state directory mode = %o, want %o", got, DirMode)
	}
}

func TestOpen_UnwritablePath(t *testing.T) {
	// A directory where the file should be: sqlite cannot open it, and Open
	// must say so rather than return a handle that fails on first use.
	dir := filepath.Join(t.TempDir(), "state.db")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), dir, &fakeRedactor{}); err == nil {
		t.Fatal("Open succeeded against a directory")
	}
}

// TestRedactor_SatisfiedByLogstream pins the narrow local interface to the real
// one. If logstream.Redactor's method set changes, this stops compiling here
// rather than at whichever call site wires them together later.
func TestRedactor_SatisfiedByLogstream(t *testing.T) {
	var red Redactor = logstream.NewRedactor()

	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	if got := red.Redact("token=" + token); strings.Contains(got, token) {
		t.Fatalf("the real redactor left the token intact: %q", got)
	}

	d := newTestDB(t)
	d.red = red
	mustCreate(t, d, func() Run {
		r := testRun("real-redactor", protocol.StateQueued)
		r.Inputs = map[string]string{"TOKEN": token}
		return r
	}())

	got, err := d.Get(t.Context(), "real-redactor")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Inputs["TOKEN"], token) {
		t.Fatalf("inputs_json holds the token: %q", got.Inputs["TOKEN"])
	}
}

// TestSQLStates_RejectsUnquotableState documents why inlining states into SQL
// is safe: the guard panics on anything that is not a bare identifier, so a
// future state constant carrying a quote is a startup panic and not an
// injection.
func TestSQLStates_RejectsUnquotableState(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("sqlStates accepted a state containing a quote")
		}
	}()
	_ = sqlStates([]protocol.RunState{protocol.RunState("x'; DROP TABLE runs; --")})
}

func TestCloseIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	var nilDB *DB
	if err := nilDB.Close(); err != nil {
		t.Fatalf("Close on a nil DB: %v", err)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 7, 12, 34, 56, 789_012_345, time.UTC)
	if got := fromUnix(toUnix(want)); !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
	if nullTime(nil) != nil {
		t.Error("a nil deadline must bind as NULL")
	}
	var zero time.Time
	if nullTime(&zero) != nil {
		t.Error("a zero deadline must bind as NULL, not as the epoch")
	}
}

func TestDefaultPath(t *testing.T) {
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".latem", "state.db"); !strings.HasSuffix(got, want) {
		t.Errorf("DefaultPath = %q, want it to end in %q (RUNNER_SPEC §22)", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("DefaultPath = %q, want an absolute path", got)
	}
}

// TestClosedDB_ErrorsRatherThanPanics covers the shutdown race: the daily
// ticker, a broker replay, and a CLI can all be mid-call when the agent closes
// the database. Each must come back with an error, because a panic in a
// goroutine takes the agent down with in-flight runs attached to it.
func TestClosedDB_ErrorsRatherThanPanics(t *testing.T) {
	d := newTestDB(t)
	mustCreate(t, d, testRun("r", protocol.StateQueued))
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	ops := map[string]func() error{
		"Migrate":         func() error { return d.Migrate(ctx) },
		"Create":          func() error { return d.Create(ctx, testRun("other", protocol.StateQueued)) },
		"Get":             func() error { _, err := d.Get(ctx, "r"); return err },
		"List":            func() error { _, err := d.List(ctx, Filter{}); return err },
		"Transition":      func() error { return d.Transition(ctx, "r", protocol.StateClaimed, protocol.Update{}) },
		"Enqueue":         func() error { return d.Enqueue(ctx, "r", 0) },
		"ClaimNext":       func() error { _, err := d.ClaimNext(ctx, []string{"docker"}); return err },
		"Claim":           func() error { return d.Claim(ctx, "r") },
		"SweepExpired":    func() error { _, err := d.SweepExpired(ctx, now); return err },
		"Unreported":      func() error { _, err := d.Unreported(ctx, BrokerLocal); return err },
		"MarkReported":    func() error { return d.MarkReported(ctx, BrokerLocal, "r", protocol.StateQueued) },
		"SetLastAckedSeq": func() error { return d.SetLastAckedSeq(ctx, BrokerLocal, "r", 1) },
		"LastAckedSeq":    func() error { _, err := d.LastAckedSeq(ctx, BrokerLocal, "r"); return err },
		"SetRunner":       func() error { return d.SetRunner(ctx, "r", "docker") },
		"SetExpiry":       func() error { return d.SetExpiry(ctx, "r", now) },
		"SetCancelReason": func() error { return d.SetCancelReason(ctx, "r", "why") },
		"RecordArtifacts": func() error { return d.RecordArtifacts(ctx, "r", []protocol.ArtifactRef{{Path: "a", SHA256: "b"}}) },
		"Artifacts":       func() error { _, err := d.Artifacts(ctx, "r"); return err },
		"NonTerminal":     func() error { _, err := d.NonTerminal(ctx); return err },
		"QueueDepth":      func() error { _, err := d.QueueDepth(ctx); return err },
		"Dequeue":         func() error { return d.Dequeue(ctx, "r") },
		"UpsertCache":     func() error { return d.UpsertCache(ctx, CacheMeta{Name: "c", Runner: "docker"}) },
		"Caches":          func() error { _, err := d.Caches(ctx); return err },
		"DeleteCache":     func() error { return d.DeleteCache(ctx, "c") },
		"sweepPlan":       func() error { _, err := d.sweepPlan(ctx, now); return err },
		"appliedVersions": func() error { _, err := d.appliedVersions(ctx); return err },
		"verifyWAL":       func() error { return d.verifyWAL(ctx) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Errorf("%s against a closed database returned no error", name)
			}
		})
	}
}

// TestNotImplemented_RefusesEveryCall pins WS-00's stub: every method must
// report errors.ErrUnsupported so a caller wired against the placeholder fails
// recognisably instead of appearing to persist things.
func TestNotImplemented_RefusesEveryCall(t *testing.T) {
	var s Store = NotImplemented{}
	ctx := t.Context()

	ops := map[string]func() error{
		"Migrate":         func() error { return s.Migrate(ctx) },
		"Create":          func() error { return s.Create(ctx, Run{}) },
		"Get":             func() error { _, err := s.Get(ctx, "r"); return err },
		"List":            func() error { _, err := s.List(ctx, Filter{}); return err },
		"Transition":      func() error { return s.Transition(ctx, "r", protocol.StateQueued, protocol.Update{}) },
		"Enqueue":         func() error { return s.Enqueue(ctx, "r", 0) },
		"ClaimNext":       func() error { _, err := s.ClaimNext(ctx, nil); return err },
		"Claim":           func() error { return s.Claim(ctx, "r") },
		"SweepExpired":    func() error { _, err := s.SweepExpired(ctx, time.Now()); return err },
		"Unreported":      func() error { _, err := s.Unreported(ctx, BrokerLocal); return err },
		"MarkReported":    func() error { return s.MarkReported(ctx, BrokerLocal, "r", protocol.StateQueued) },
		"SetLastAckedSeq": func() error { return s.SetLastAckedSeq(ctx, BrokerLocal, "r", 1) },
		"LastAckedSeq":    func() error { _, err := s.LastAckedSeq(ctx, BrokerLocal, "r"); return err },
		"RecordArtifacts": func() error { return s.RecordArtifacts(ctx, "r", nil) },
		"Artifacts":       func() error { _, err := s.Artifacts(ctx, "r"); return err },
		"NonTerminal":     func() error { _, err := s.NonTerminal(ctx); return err },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("NotImplemented.%s = %v, want errors.ErrUnsupported", name, err)
		}
	}
	// Close is the exception: closing something that was never open succeeds.
	if err := s.Close(); err != nil {
		t.Errorf("NotImplemented.Close = %v, want nil", err)
	}
}
