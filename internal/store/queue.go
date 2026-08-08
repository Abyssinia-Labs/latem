package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// Enqueue adds a run to the local queue, or updates its priority.
//
// A repeat enqueue changes the priority and keeps the original enqueued_at.
// Resetting the timestamp would send the run to the back of its priority band
// every time something re-enqueued it, so a retry loop could starve everything
// queued behind it while looking, from the outside, like it was making progress.
//
// The run must exist and must not be terminal. Enqueuing a finished run is
// harmless in the sense that ClaimNext keys on state and would never take it —
// which is exactly why it is refused here: the row would sit in the queue
// forever, looking like work.
func (d *DB) Enqueue(ctx context.Context, runID string, priority int) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		var state protocol.RunState
		err := t.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s", ErrNotFound, runID)
		}
		if err != nil {
			return fmt.Errorf("store: Enqueue %s: %w", runID, err)
		}
		if state.IsTerminal() {
			return fmt.Errorf("store: Enqueue %s: run is %s", runID, state)
		}

		_, err = t.ExecContext(ctx, `
			INSERT INTO queue (run_id, priority, enqueued_at) VALUES (?, ?, ?)
			ON CONFLICT(run_id) DO UPDATE SET priority = excluded.priority`,
			runID, priority, toUnix(time.Now()))
		if err != nil {
			return fmt.Errorf("store: Enqueue %s: %w", runID, err)
		}
		return nil
	})
}

// Dequeue removes a run from the queue without changing its state.
//
// Not part of [Store]. It exists for the case where a run must leave the queue
// while staying where it is — a concurrency group deferring work, an operator
// pulling a run back — and so that the queue table has a documented way out
// that is not "transition it somewhere".
func (d *DB) Dequeue(ctx context.Context, runID string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("store: Dequeue %s: %w", runID, err)
	}
	return nil
}

// QueueDepth reports how many runs are waiting to be claimed. Not part of
// [Store]; the heartbeat in §20 reports it and `latem status` prints it.
func (d *DB) QueueDepth(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM queue q JOIN runs r ON r.id = q.run_id
		 WHERE r.state = ?`, string(protocol.StateQueued)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: QueueDepth: %w", err)
	}
	return n, nil
}

// claimOrder is the queue discipline: priority first, then arrival, then id.
//
// The id is not decoration. Without a total order two runs enqueued in the same
// nanosecond would tie, and the row sqlite picked for the LIMIT 1 subquery
// could differ between two agents running the same query — which is a fine way
// to get a queue that appears to skip a run.
const claimOrder = `ORDER BY q.priority DESC, q.enqueued_at ASC, q.run_id ASC`

// ClaimNext atomically claims the highest-priority queued run.
//
// Atomicity is the whole contract, and it comes from the statement, not from
// the transaction: one UPDATE, whose WHERE clause both selects the candidate
// and re-asserts that it is still queued. Two agents — or two runner goroutines
// — executing this concurrently are serialized by sqlite's single writer, and
// the loser's WHERE clause no longer matches, so it claims nothing rather than
// claiming the same run. A SELECT followed by an UPDATE would look identical in
// a test and be wrong in production, which is why the shape is specified in
// RUNNER_SPEC §7.3 rather than left to the implementation.
//
// The runners filter matches on runs.runner, which is NULL for almost every
// queued run: the runner is chosen at gate 13 (label match), and that happens
// during preparing — after this. So the filter means "runs already pinned to
// one of my runners, plus everything unpinned", not "runs whose runs_on my
// labels satisfy". This store cannot answer the latter: runs_on lives in
// jobs.yaml at the resolved commit, which does not exist on disk until the
// workspace is checked out (INV-5), which happens after the claim. A nil or
// empty runners claims from the whole queue.
//
// Returns [ErrNotFound] when there is nothing to claim.
func (d *DB) ClaimNext(ctx context.Context, runners []string) (Run, error) {
	filter, filterArgs := runnerFilter(runners)

	// state=claimed, then the subquery's state=queued, then the runner names,
	// then the outer re-assertion that the row is still queued.
	args := make([]any, 0, len(filterArgs)+3)
	args = append(args, string(protocol.StateClaimed), string(protocol.StateQueued))
	args = append(args, filterArgs...)
	args = append(args, string(protocol.StateQueued))

	// G202 flags the concatenation. Every fragment joined here is either a
	// constant in this file or a string of `?` placeholders generated by
	// runnerFilter; the runner names themselves are bound parameters and never
	// appear in the text. Suppressed with a reason per the project lint policy.
	//nolint:gosec // no caller-supplied value is concatenated into this SQL
	query := `
		UPDATE runs SET state = ?, expires_at = NULL
		 WHERE id = (
		       SELECT q.run_id FROM queue q JOIN runs r ON r.id = q.run_id
		        WHERE r.state = ?` + filter + `
		        ` + claimOrder + `
		        LIMIT 1)
		   AND state = ?
		RETURNING ` + runColumns

	var claimed Run
	err := d.tx(ctx, func(t *sql.Tx) error {
		row := t.QueryRowContext(ctx, query, args...)
		r, err := scanRun(row)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no claimable run", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: ClaimNext: %w", err)
		}

		if _, err := t.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, r.ID); err != nil {
			return fmt.Errorf("store: ClaimNext %s: dequeue: %w", r.ID, err)
		}
		claimed = r
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return claimed, nil
}

// runnerFilter renders the optional runs.runner restriction.
func runnerFilter(runners []string) (string, []any) {
	if len(runners) == 0 {
		return "", nil
	}
	holders := make([]string, len(runners))
	args := make([]any, len(runners))
	for i, r := range runners {
		holders[i] = "?"
		args[i] = r
	}
	return " AND (r.runner IS NULL OR r.runner IN (" + strings.Join(holders, ",") + "))", args
}

// Claim atomically claims one specific run.
//
// Same conditional-UPDATE shape as [ClaimNext] and for the same reason. It
// returns [ErrNotClaimable] — not an error the caller should treat as a fault —
// when the run has already left the queue, and [ErrNotFound] when there is no
// such run at all. Distinguishing the two matters: the first is a lost race
// between two runner goroutines, the second is a bad id.
func (d *DB) Claim(ctx context.Context, runID string) error {
	return d.tx(ctx, func(t *sql.Tx) error {
		res, err := t.ExecContext(ctx,
			`UPDATE runs SET state = ?, expires_at = NULL WHERE id = ? AND state = ?`,
			string(protocol.StateClaimed), runID, string(protocol.StateQueued))
		if err != nil {
			return fmt.Errorf("store: Claim %s: %w", runID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: Claim %s: %w", runID, err)
		}
		if n == 0 {
			var state protocol.RunState
			switch err := t.QueryRowContext(ctx, `SELECT state FROM runs WHERE id = ?`, runID).Scan(&state); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("%w: run %s", ErrNotFound, runID)
			case err != nil:
				return fmt.Errorf("store: Claim %s: %w", runID, err)
			default:
				return fmt.Errorf("%w: run %s is %s", ErrNotClaimable, runID, state)
			}
		}

		if _, err := t.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
			return fmt.Errorf("store: Claim %s: dequeue: %w", runID, err)
		}
		return nil
	})
}
