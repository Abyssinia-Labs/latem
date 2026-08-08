package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	// The driver is cgo-free on purpose: the agent ships as a static
	// cross-compiled binary, and a cgo sqlite makes that build a per-platform
	// toolchain problem (RUNNER_SPEC §3).
	_ "modernc.org/sqlite"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ErrExists means a run with that id is already stored. Run ids come from
// outside the trust boundary, so a collision is a caller error worth naming
// rather than an overwrite worth performing silently.
var ErrExists = errors.New("store: run already exists")

// ErrIllegalTransition means the requested state change is not an edge in the
// RUNNER_SPEC §11 state machine. Returning it is the point of [DB.Transition]:
// an ordering bug should surface as a refused write, not as a run history that
// says a run was queued after it finished.
var ErrIllegalTransition = errors.New("store: illegal state transition")

// Redactor scrubs a value that is about to be persisted.
//
// It is declared here, rather than imported from internal/logstream, because
// the store needs one method and the obligation behind it (INV-4) — not the log
// pipeline's Aho-Corasick matcher, its sinks, or its rotation. logstream.Redactor
// satisfies this interface; store_test.go pins that so the two cannot drift.
type Redactor interface {
	// Redact returns line with every registered secret and matched credential
	// pattern replaced.
	Redact(line string) string
}

// DirMode is the mode of ~/.latem. FileMode is the mode of the database
// itself. The run history is not secret — it is redacted before it lands here —
// but it records what was built, when, and by whom, and on a shared build
// machine that is nobody else's business.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// DefaultListLimit bounds [DB.List] when the caller does not. An unbounded
// list is fine on a new agent and a year later returns every run the machine
// has ever done, which is a slow surprise in a CLI.
const DefaultListLimit = 100

// busyTimeout is how long a writer waits for another writer's transaction.
// Ten seconds is far longer than any statement here takes; it exists so a
// claim contending with a log-sequence update retries instead of failing.
const busyTimeout = 10 * time.Second

// maxOpenConns is deliberately greater than one.
//
// A pool of one would make every write atomic by construction — and would
// make the atomicity of the claim path a property of the Go pool rather than
// of the SQL, which is the property RUNNER_SPEC §7.3 actually specifies and the
// one the concurrency tests need to be able to break.
const maxOpenConns = 8

// DB is the sqlite implementation of [Store].
type DB struct {
	db  *sql.DB
	red Redactor
}

var _ Store = (*DB)(nil)

// DefaultPath is ~/.latem/state.db (RUNNER_SPEC §22).
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("store: locating home directory: %w", err)
	}
	return filepath.Join(home, ".latem", "state.db"), nil
}

// Open opens (creating if needed) the database at path.
//
// It does not migrate: [DB.Migrate] is a separate call because RUNNER_SPEC §6
// runs it as an explicit startup step, after the flock that guarantees one
// agent per config directory. Migrating from inside Open would move schema
// changes to whoever opened the file first, including a CLI process racing the
// agent.
//
// red is required. A nil redactor would silently satisfy the interface and
// write unscrubbed inputs (INV-4), so it is refused here rather than defaulted
// to a no-op.
func Open(ctx context.Context, path string, red Redactor) (*DB, error) {
	if red == nil {
		return nil, errors.New("store: Open requires a redactor: inputs_json and cancel_reason are scrubbed before persistence (INV-4)")
	}
	if path == "" {
		return nil, errors.New("store: Open requires a path")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolving %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), DirMode); err != nil {
		return nil, fmt.Errorf("store: creating state directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: opening %q: %w", abs, err)
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: opening %q: %w", abs, err)
	}
	// The file exists only once a connection has touched it, so the mode is
	// fixed here rather than before Open. sqlite gives -wal and -shm the same
	// mode as the main file.
	if err := os.Chmod(abs, FileMode); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: securing %q: %w", abs, err)
	}

	d := &DB{db: sqlDB, red: red}
	if err := d.verifyWAL(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// dsn builds the connection string.
//
// _txlock=immediate is the subtle one. Without it every transaction begins
// deferred, takes its write lock on the first write, and two transactions that
// read before writing deadlock — for which sqlite returns SQLITE_BUSY
// immediately and does NOT invoke the busy handler, so busy_timeout cannot
// save it. Beginning immediate turns that deadlock into a wait, which is what
// the claim path needs to be merely contended rather than flaky.
//
// synchronous=NORMAL is the standard WAL pairing: it fsyncs at checkpoints
// rather than at every commit. A process crash — the case RUNNER_SPEC §6's
// reconciliation is written for — loses nothing. Only an OS or power failure
// can lose the last commits, and the run those commits described comes back as
// non-terminal and is reconciled to interrupted, which is the correct answer.
func dsn(path string) string {
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Set("_busy_timeout", fmt.Sprint(busyTimeout.Milliseconds()))
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_foreign_keys", "on")

	u := url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}
	return u.String()
}

// verifyWAL fails Open if the journal mode did not take.
//
// Silently falling back to a rollback journal would cost the property the rest
// of this package assumes: readers concurrent with a writer. The symptom would
// be intermittent SQLITE_BUSY under load on a machine nobody can reproduce.
func (d *DB) verifyWAL(ctx context.Context) error {
	var mode string
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: reading journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	return nil
}

// Close releases the connection pool.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// tx runs fn in a transaction, rolling back on error or panic.
//
// Every transaction is BEGIN IMMEDIATE (see [dsn]), so entering one is taking
// the write lock. That is intentional even for the read-modify-write helpers:
// they exist precisely because the read and the write must not be separable.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = t.Rollback() }()

	if err := fn(t); err != nil {
		return err
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// --- time -------------------------------------------------------------------

// Times are stored as Unix nanoseconds.
//
// §22 says INTEGER and no more. Nanoseconds are chosen over seconds because
// runs_repo_created_idx orders by created_at and a burst of runs created in the
// same second would otherwise have no defined order — a paging CLI would show
// duplicates and omissions. int64 nanoseconds run out in the year 2262.
func toUnix(t time.Time) int64 { return t.UTC().UnixNano() }

func fromUnix(n int64) time.Time { return time.Unix(0, n).UTC() }

// nullTime renders an optional deadline for binding: NULL, not zero, so
// "no deadline" and "the epoch" stay distinguishable.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return toUnix(*t)
}

func scanTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromUnix(n.Int64)
	return &t
}

// nullString maps "" to NULL so an absent value reads as absent rather than as
// an empty one. The columns that do this are all nullable in §22.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// --- state lists ------------------------------------------------------------

// sqlStates renders states as a quoted SQL list.
//
// The lists in §22's partial indexes must appear in query text as literals, not
// as bound parameters: sqlite can only use a partial index when the query's
// WHERE clause implies the index's own predicate, and it decides that by
// comparing expressions it can see. A bound parameter is opaque to that
// analysis, so `state IN (?,?)` silently costs the TTL sweep its index.
//
// Deriving the literals from pkg/protocol rather than hardcoding them is what
// keeps the runtime queries and the migration from drifting apart.
func sqlStates(states []protocol.RunState) string {
	quoted := make([]string, len(states))
	for i, s := range states {
		quoted[i] = "'" + mustLiteral(string(s)) + "'"
	}
	return strings.Join(quoted, ",")
}

// mustLiteral panics if a state constant is not a bare identifier.
//
// These strings come from a Go constant today, so nothing can be injected. The
// check is here because the moment one of them is derived from anything else,
// this function is a SQL injection, and a panic at package init is a better way
// to learn that than a corrupted database.
func mustLiteral(s string) string {
	if s == "" {
		panic("store: empty state constant")
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r == '_' {
			continue
		}
		panic(fmt.Sprintf("store: state %q is not a bare identifier and cannot be inlined into SQL", s))
	}
	return s
}

// terminalIndexOrder is the terminal set, in the order RUNNER_SPEC §22 spells
// it inside runs_state_idx.
//
// The order is not cosmetic. sqlite decides whether a query may use a partial
// index by comparing the query's WHERE clause against the index's predicate
// expression by expression, and two IN lists holding the same values in a
// different order are different expressions. Sorting this list instead of
// matching the migration would leave the index unused and nothing would say so.
//
// [mustCoverTerminalStates] is what keeps the *set* honest: protocol owns which
// states are terminal, this owns how they are written.
var terminalIndexOrder = mustCoverTerminalStates([]protocol.RunState{
	protocol.StateSucceeded,
	protocol.StateFailed,
	protocol.StateCancelled,
	protocol.StateRejected,
	protocol.StateTimedOut,
	protocol.StateInterrupted,
})

// mustCoverTerminalStates panics unless order is exactly protocol's terminal
// set. It runs at package init, so a state added to pkg/protocol without a
// matching migration here fails the first test that touches this package
// rather than silently shrinking what runs_state_idx covers.
func mustCoverTerminalStates(order []protocol.RunState) []protocol.RunState {
	want := protocol.TerminalStates()
	slices.Sort(want)

	got := slices.Clone(order)
	slices.Sort(got)

	if !slices.Equal(got, want) {
		panic(fmt.Sprintf(
			"store: terminal states %v do not match protocol.TerminalStates() %v; "+
				"a state was added without a migration for runs_state_idx (RUNNER_SPEC §22)", got, want))
	}
	return order
}

// waitingStates are the states that carry a deadline (§11). They are the
// subject of runs_expiry_idx and of [DB.SweepExpired].
func waitingStates() []protocol.RunState {
	return []protocol.RunState{protocol.StateQueued, protocol.StateAwaitingApproval}
}

var (
	// nonTerminalPredicate matches runs_state_idx's own predicate textually, so
	// the planner can use that index for the in-flight queries.
	nonTerminalPredicate = "state NOT IN (" + sqlStates(terminalIndexOrder) + ")"
	// waitingPredicate matches runs_expiry_idx's predicate, for the same reason.
	waitingPredicate = "state IN (" + sqlStates(waitingStates()) + ")"
)
