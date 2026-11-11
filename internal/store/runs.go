package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// runColumns is the projection every read shares, in the order [scanRun]
// expects. One list rather than five means a column added to §22 is a compile
// error in one place instead of a silent nil in four.
const runColumns = `id, repo, job, ref, ref_normalized, sha, runner, state,
	exit_code, error_code, error_message, origin_kind, origin_actor,
	cancel_reason, inputs_json, created_at, started_at, finished_at,
	expires_at, approved_at, approved_sha, number, workflow_json`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanRun materialises one row. Nullable columns become the zero value of the
// Go field, so a caller never has to distinguish "" from NULL for a ref that
// was never normalized.
func scanRun(s scanner) (Run, error) {
	var (
		r            Run
		refNorm      sql.NullString
		sha          sql.NullString
		runner       sql.NullString
		exitCode     sql.NullInt64
		errCode      sql.NullString
		errMsg       sql.NullString
		originActor  sql.NullString
		cancelReason sql.NullString
		inputsJSON   string
		createdAt    int64
		startedAt    sql.NullInt64
		finishedAt   sql.NullInt64
		expiresAt    sql.NullInt64
		approvedAt   sql.NullInt64
		approvedSHA  sql.NullString
		workflowJSON sql.NullString
	)

	err := s.Scan(
		&r.ID, &r.Repo, &r.Job, &r.Ref, &refNorm, &sha, &runner, &r.State,
		&exitCode, &errCode, &errMsg, &r.OriginKind, &originActor,
		&cancelReason, &inputsJSON, &createdAt, &startedAt, &finishedAt,
		&expiresAt, &approvedAt, &approvedSHA, &r.Number, &workflowJSON,
	)
	if err != nil {
		return Run{}, err
	}

	r.RefNormalized = refNorm.String
	r.SHA = sha.String
	r.Runner = runner.String
	if exitCode.Valid {
		c := int(exitCode.Int64)
		r.ExitCode = &c
	}
	r.ErrorCode = protocol.ErrorCode(errCode.String)
	r.ErrorMsg = errMsg.String
	r.OriginActor = originActor.String
	r.CancelReason = cancelReason.String
	r.CreatedAt = fromUnix(createdAt)
	r.StartedAt = scanTime(startedAt)
	r.FinishedAt = scanTime(finishedAt)
	r.ExpiresAt = scanTime(expiresAt)
	r.ApprovedAt = scanTime(approvedAt)
	r.ApprovedSHA = approvedSHA.String

	if inputsJSON != "" {
		if err := json.Unmarshal([]byte(inputsJSON), &r.Inputs); err != nil {
			return Run{}, fmt.Errorf("store: run %s has undecodable inputs_json: %w", r.ID, err)
		}
	}
	if workflowJSON.Valid {
		r.Workflow = &WorkflowRun{}
		if err := json.Unmarshal([]byte(workflowJSON.String), r.Workflow); err != nil {
			return Run{}, fmt.Errorf("store: run %s has undecodable workflow_json: %w", r.ID, err)
		}
	}
	return r, nil
}

// scanRuns drains a result set.
func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading runs: %w", err)
	}
	return out, nil
}

// redactWorkflow encodes a workflow run's fields for workflow_json, through
// the redactor like everything else a broker supplied (INV-4). The event
// payload and needs' outputs are other machines' data; a value this machine
// holds as a secret is scrubbed from them before they reach disk, and an
// encoding the scrub broke is refused rather than stored undecodable.
func (d *DB) redactWorkflow(w *WorkflowRun) (sql.NullString, error) {
	if w == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encoding workflow_json: %w", err)
	}
	out := d.red.Redact(string(b))
	if !json.Valid([]byte(out)) {
		return sql.NullString{}, errors.New("a secret this machine holds is in the workflow run's data, " +
			"and scrubbing it left the data unreadable")
	}
	return sql.NullString{String: out, Valid: true}, nil
}

// redactInputs scrubs every input value and encodes the map.
//
// This is INV-4 at the persistence boundary. Inputs are broker-supplied
// strings; a webhook that puts a token in an input value would otherwise write
// it to a file that outlives the run, that `latem show` prints, and that a
// support bundle collects. Keys are left alone: gate 3 has already constrained
// them to identifiers, and a redacted key would break lookup by name.
//
// json.Marshal sorts map keys, so the stored text is stable for a given map —
// which keeps the column diffable and the tests exact.
func (d *DB) redactInputs(in map[string]string) (string, error) {
	if len(in) == 0 {
		return "{}", nil
	}
	clean := make(map[string]string, len(in))
	for k, v := range in {
		clean[k] = d.red.Redact(v)
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("store: encoding inputs: %w", err)
	}
	return string(b), nil
}

// Create inserts a run.
//
// The id is validated even though it arrived validated, because it is about to
// become a primary key that the rest of the agent turns into paths and
// container names (protocol.ValidateID). Two validations of an untrusted value
// are cheaper than one missing one.
func (d *DB) Create(ctx context.Context, r Run) error {
	if err := protocol.ValidateID(r.ID); err != nil {
		return fmt.Errorf("store: Create: %w", err)
	}
	switch {
	case r.Repo == "":
		return errors.New("store: Create: repo is empty")
	case r.Job == "":
		return errors.New("store: Create: job is empty")
	case r.Ref == "":
		return errors.New("store: Create: ref is empty")
	case !r.State.Valid():
		return fmt.Errorf("store: Create: unknown state %q", r.State)
	case r.Number != 0:
		// Refused rather than ignored. A caller that set this believes it
		// chose the build number, and silently overwriting it would leave the
		// upload carrying a value nobody in the code chose (A36).
		return fmt.Errorf("store: Create: Number is assigned by the store, not supplied (got %d)", r.Number)
	}
	if err := checkErrorCode(r.ErrorCode); err != nil {
		return fmt.Errorf("store: Create: %w", err)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}

	inputs, redactErr := d.redactInputs(r.Inputs)
	if redactErr != nil {
		return redactErr
	}
	workflow, err := d.redactWorkflow(r.Workflow)
	if err != nil {
		return fmt.Errorf("store: Create %s: %w", r.ID, err)
	}

	return d.tx(ctx, func(t *sql.Tx) error {
		// The transaction is BEGIN IMMEDIATE, so this existence check cannot be
		// raced by a concurrent insert. Checking beats parsing a driver-specific
		// constraint error out of the insert.
		var exists int
		switch err := t.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, r.ID).Scan(&exists); {
		case err == nil:
			return fmt.Errorf("%w: %s", ErrExists, r.ID)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: Create: %w", err)
		}

		// The run's number, issued inside the same transaction as the row so a
		// crash between the two is not a possible state. BEGIN IMMEDIATE makes
		// the read-modify-write safe against a concurrent Create (A36).
		number, err := nextSequence(ctx, t, seqRunNumber)
		if err != nil {
			return err
		}

		_, err = t.ExecContext(ctx, `
			INSERT INTO runs (
			  id, repo, job, ref, ref_normalized, sha, runner, state,
			  exit_code, error_code, error_message, origin_kind, origin_actor,
			  cancel_reason, inputs_json, created_at, started_at, finished_at,
			  expires_at, approved_at, approved_sha, number, workflow_json
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Repo, r.Job, r.Ref, nullString(r.RefNormalized), nullString(r.SHA),
			nullString(r.Runner), string(r.State),
			nullInt(r.ExitCode), nullString(string(r.ErrorCode)), nullString(d.red.Redact(r.ErrorMsg)),
			r.OriginKind, nullString(r.OriginActor),
			nullString(d.red.Redact(r.CancelReason)), inputs, toUnix(r.CreatedAt),
			nullTime(r.StartedAt), nullTime(r.FinishedAt), nullTime(r.ExpiresAt),
			nullTime(r.ApprovedAt), nullString(r.ApprovedSHA), number, workflow,
		)
		if err != nil {
			return fmt.Errorf("store: Create %s: %w", r.ID, err)
		}
		return nil
	})
}

// Get returns one run, or [ErrNotFound].
func (d *DB) Get(ctx context.Context, runID string) (Run, error) {
	row := d.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: Get %s: %w", runID, err)
	}
	return r, nil
}

// List returns runs newest first.
//
// The ordering is (created_at DESC, id DESC) rather than created_at alone so
// paging is a total order: two runs created in the same instant would otherwise
// swap places between calls and a paging caller would see one twice and the
// other never.
func (d *DB) List(ctx context.Context, f Filter) ([]Run, error) {
	var (
		where []string
		args  []any
	)
	if f.Repo != "" {
		where = append(where, "repo = ?")
		args = append(args, f.Repo)
	}
	if len(f.States) > 0 {
		holders := make([]string, len(f.States))
		for i, s := range f.States {
			holders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "state IN ("+strings.Join(holders, ",")+")")
	}
	if f.SHA != "" {
		// Spelled as runs_commit_idx spells it, so that SQLite uses it.
		where = append(where, "coalesce(sha, json_extract(workflow_json, '$.sha')) = ?")
		args = append(args, f.SHA)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	args = append(args, limit)

	q := `SELECT ` + runColumns + ` FROM runs`
	if len(where) > 0 {
		// The clauses joined here are literals written above; the repo name and
		// the states are bound parameters. Nothing from the filter reaches the
		// query text.
		q += ` WHERE ` + strings.Join(where, " AND ") //nolint:gosec // no caller-supplied value is concatenated into this SQL
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: List: %w", err)
	}
	return scanRuns(rows)
}

// WorkflowRunJobs returns this machine's runs of jobs of one workflow run,
// newest first (A84). The predicate is runs_workflow_run_idx's, spelled as the
// index spells it so that SQLite uses it: a download reads the few jobs of its
// own workflow run, never a scan of every run kept here. limit bounds it all
// the same, and DefaultListLimit stands in for none.
func (d *DB) WorkflowRunJobs(ctx context.Context, repo, workflowRunID string, jobs []string, limit int) ([]Run, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	holders := make([]string, len(jobs))
	args := []any{repo, workflowRunID}
	for i, j := range jobs {
		holders[i] = "?"
		args = append(args, j)
	}
	args = append(args, limit)
	// Only the column list and placeholders are concatenated; every value is
	// a bound parameter.
	q := `SELECT ` + runColumns + ` FROM runs` + //nolint:gosec // see above
		` WHERE repo = ? AND workflow_json IS NOT NULL
	   AND json_extract(workflow_json, '$.workflow_run_id') = ?
	   AND job IN (` + strings.Join(holders, ",") + `)
	 ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: WorkflowRunJobs: %w", err)
	}
	return scanRuns(rows)
}

// NonTerminal returns every run a crash could have left mid-flight, oldest
// first, for the startup reconciliation at RUNNER_SPEC §6 step 5.
//
// The predicate is the complement of protocol's terminal set and is spelled to
// match runs_state_idx's own predicate, so this stays cheap on a machine with
// a long history.
func (d *DB) NonTerminal(ctx context.Context) ([]Run, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE `+nonTerminalPredicate+` ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: NonTerminal: %w", err)
	}
	return scanRuns(rows)
}

// SetRunner records which runner owns a run.
//
// Not part of [Store]: the runner is decided at gate 13 (label match), which
// happens during preparing — after the claim — so it has no natural home in
// either Create or ClaimNext. See the ClaimNext comment for why the column is
// nonetheless consulted there.
func (d *DB) SetRunner(ctx context.Context, runID, runner string) error {
	if runner == "" {
		return errors.New("store: SetRunner: runner is empty")
	}
	res, err := d.db.ExecContext(ctx, `UPDATE runs SET runner = ? WHERE id = ?`, runner, runID)
	if err != nil {
		return fmt.Errorf("store: SetRunner %s: %w", runID, err)
	}
	return affectedOne(res, runID, "SetRunner")
}

// SetExpiry sets or clears a waiting run's deadline (§11, §22).
//
// Not part of [Store], and it has to exist somewhere: protocol.Update carries
// no deadline, so [DB.Transition] can clear an expiry on the way out of a
// waiting state but cannot set one on the way in. Only the caller knows the
// configured approval_ttl or queue_ttl. A zero time clears the deadline.
func (d *DB) SetExpiry(ctx context.Context, runID string, at time.Time) error {
	var arg any
	if !at.IsZero() {
		arg = toUnix(at)
	}
	res, err := d.db.ExecContext(ctx, `UPDATE runs SET expires_at = ? WHERE id = ?`, arg, runID)
	if err != nil {
		return fmt.Errorf("store: SetExpiry %s: %w", runID, err)
	}
	return affectedOne(res, runID, "SetExpiry")
}

// SetCancelReason stores the display-only reason a run was cancelled.
//
// Not part of [Store] for the same reason as [DB.SetExpiry]: protocol.Update
// has no field for it, and the reason arrives on a CancelRequest rather than
// with the transition. It is scrubbed here because it crossed the trust
// boundary as free text (§22, INV-4).
func (d *DB) SetCancelReason(ctx context.Context, runID, reason string) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE runs SET cancel_reason = ? WHERE id = ?`, nullString(d.red.Redact(reason)), runID)
	if err != nil {
		return fmt.Errorf("store: SetCancelReason %s: %w", runID, err)
	}
	return affectedOne(res, runID, "SetCancelReason")
}

// checkErrorCode refuses a code that is not a run outcome.
//
// The strings are API — the web app maps them to human copy (§21) — so an
// invented one reaches a control plane that has no text for it. E_AGENT_DOWN is
// the specific case the spec calls out: it is what `latem run --wait` exits
// with when no agent is listening, a CLI condition rather than something that
// ever happened to a run, and protocol.ErrorCode.Valid() reports false for it.
func checkErrorCode(c protocol.ErrorCode) error {
	if c == "" || c.Valid() {
		return nil
	}
	return fmt.Errorf("%q is not a run-outcome error code (RUNNER_SPEC §21)", c)
}

// seqToInt64 converts a log sequence for storage.
//
// sqlite has no unsigned integer type. A seq past MaxInt64 would be stored as a
// negative number and read back as a resume point in the distant past, which
// would re-send the whole log. It cannot happen — that is more frames than a
// machine can emit — so the honest handling is to refuse rather than to wrap.
func seqToInt64(seq uint64) (int64, error) {
	if seq > math.MaxInt64 {
		return 0, fmt.Errorf("store: log sequence %d exceeds the sqlite integer range", seq)
	}
	return int64(seq), nil
}

// affectedOne turns "no row matched" into [ErrNotFound]. Every single-row
// update in this package goes through it so that updating a run that does not
// exist is an error rather than a success that did nothing.
func affectedOne(res sql.Result, runID, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s %s: %w", op, runID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	return nil
}
