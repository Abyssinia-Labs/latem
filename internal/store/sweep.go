package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// sweepSQL rejects every waiting run past its deadline, in one statement.
//
// Two properties are built into the text and neither is incidental.
//
// The WHERE clause is spelled to match runs_expiry_idx's own predicate — the
// same `state IN (...)` list, with the states inlined as literals rather than
// bound. sqlite uses a partial index only when it can prove the query implies
// the index's predicate, and it proves that by comparing expressions it can
// read; a bound parameter is opaque to that analysis. Get this wrong and the
// sweep still returns the right rows, silently, by walking every run the
// machine has ever done. sweep_test.go asserts the plan, not the result.
//
// The error code is chosen per row by CASE rather than by running two
// statements, so a run cannot be swept twice or be missed by a state change
// landing between them. Both codes are bound parameters taken from
// pkg/protocol, so §21's spellings live in exactly one place.
var sweepSQL = `
	UPDATE runs
	   SET state = ?,
	       error_code = CASE state WHEN ? THEN ? ELSE ? END,
	       error_message = CASE state WHEN ? THEN ? ELSE ? END,
	       finished_at = ?,
	       expires_at = NULL
	 WHERE ` + waitingPredicate + `
	   AND expires_at IS NOT NULL
	   AND expires_at <= ?
	RETURNING ` + runColumns

// sweepArgs binds sweepSQL for the given instant.
func sweepArgs(now time.Time) []any {
	const (
		queueMsg    = "queue_ttl expired before a runner claimed this run"
		approvalMsg = "approval_ttl expired before anyone approved this run"
	)
	return []any{
		string(protocol.StateRejected),
		string(protocol.StateQueued), string(protocol.ErrQueueExpired), string(protocol.ErrApprovalExpired),
		string(protocol.StateQueued), queueMsg, approvalMsg,
		toUnix(now),
		toUnix(now),
	}
}

// SweepExpired rejects waiting runs past their deadline (§11).
//
// Run it from the daily ticker and at startup. Startup is the case that
// justifies it existing at all: an agent that was off for a week comes back to
// a queue full of runs nobody wants, and executing them would be worse than
// useless — they would build week-old commits and report them as fresh.
//
// A queued run past queue_ttl becomes rejected/E_QUEUE_EXPIRED; an
// awaiting_approval run past approval_ttl becomes rejected/E_APPROVAL_EXPIRED.
// Both are terminal, which is also what keeps §22's partial index from growing
// without bound: abandoned runs leave it instead of accumulating in it forever.
//
// Runs with no deadline are never swept, so a caller that forgot to set one
// gets a run that waits rather than a run that vanishes.
func (d *DB) SweepExpired(ctx context.Context, now time.Time) ([]Run, error) {
	var swept []Run

	err := d.tx(ctx, func(t *sql.Tx) error {
		rows, err := t.QueryContext(ctx, sweepSQL, sweepArgs(now)...)
		if err != nil {
			return fmt.Errorf("store: SweepExpired: %w", err)
		}
		swept, err = scanRuns(rows)
		if err != nil {
			return err
		}

		// A swept run is terminal and must leave the queue with the same
		// transaction that ended it, or ClaimNext would keep considering a row
		// it can never claim.
		for _, r := range swept {
			if _, err := t.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, r.ID); err != nil {
				return fmt.Errorf("store: SweepExpired %s: dequeue: %w", r.ID, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return swept, nil
}

// sweepPlan returns sqlite's query plan for the sweep. It exists so a test can
// assert that the partial index is used; nothing in the agent calls it.
func (d *DB) sweepPlan(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sweepSQL, sweepArgs(now)...)
	if err != nil {
		return nil, fmt.Errorf("store: sweep plan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, fmt.Errorf("store: sweep plan: %w", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sweep plan: %w", err)
	}
	return out, nil
}
