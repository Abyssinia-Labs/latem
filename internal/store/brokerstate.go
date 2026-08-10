package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// This file is the broker_state table of migration 0003 (amendment A28): what
// each transport has acknowledged about each run, keyed by transport.
//
// Everything here used to be two columns of runs, and the reason it is not any
// more is that "acknowledged" is a claim about a far side, and a machine has
// more than one far side. See migrations/0003_broker_state.sql.
//
// A row is created lazily. A run nobody has reported and nobody has streamed
// has no row, and reads as "nothing acknowledged, resume at 0" — which is the
// same thing a freshly inserted row would say, so the absence costs nothing and
// keeps the table proportional to what has actually been transmitted rather
// than to the machine's whole history.

// checkBroker refuses a key that would silently open a third bucket.
//
// It does not check the key against a fixed list, deliberately: the table is
// keyed precisely so that a transport can be added without a migration. What it
// does refuse is the empty string, because that is what a caller that forgot the
// argument passes, and the symptom would be a transport whose acknowledgements
// went to a bucket no reader ever looks in — that is, a transport that replayed
// its whole history on every reconnect and never said why.
func checkBroker(brk, op string) error {
	if brk == "" {
		return fmt.Errorf("store: %s: no broker key; pass store.BrokerLocal or store.BrokerRemote (RUNNER_SPEC §22, amendment A28)", op)
	}
	return nil
}

// requireRun fails with [ErrNotFound] unless the run exists.
//
// The broker_state writes are upserts, so they cannot use [affectedOne] to tell
// "no such run" from "nothing changed" — an upsert always affects a row. The
// foreign key would refuse the insert, but it would refuse it as a driver-shaped
// constraint error rather than as the sentinel every other method in this
// package returns for the same mistake.
func requireRun(ctx context.Context, t *sql.Tx, runID, op string) error {
	var exists int
	switch err := t.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, runID).Scan(&exists); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: run %s", ErrNotFound, runID)
	case err != nil:
		return fmt.Errorf("store: %s %s: %w", op, runID, err)
	}
	return nil
}

// Unreported returns the runs whose current state brk has not acknowledged,
// oldest first.
//
// Oldest first matters: replay after a reconnect should reach the broker in the
// order the runs happened, or a dashboard briefly shows this morning's build
// finishing after this afternoon's.
//
// A run with no row for this transport — nothing has ever been acked — counts
// as lagging, which is why the join is a LEFT JOIN and the NULL case is spelled
// out. It is also the common case on the first connection after an agent
// upgrade, and answering it "nothing to replay" would leave a control plane
// permanently behind.
func (d *DB) Unreported(ctx context.Context, brk string) ([]Run, error) {
	if err := checkBroker(brk, "Unreported"); err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs
		   LEFT JOIN broker_state
		     ON broker_state.run_id = runs.id AND broker_state.broker = ?
		  WHERE broker_state.reported_state IS NULL
		     OR broker_state.reported_state <> runs.state
		  ORDER BY runs.created_at ASC, runs.id ASC`, brk)
	if err != nil {
		return nil, fmt.Errorf("store: Unreported(%s): %w", brk, err)
	}
	return scanRuns(rows)
}

// MarkReported records that one broker acknowledged one state.
//
// It is written unconditionally rather than only when it advances, because a
// stale ack arriving after the run moved on must leave the run in [DB.Unreported]
// — which it does: reported_state then differs from state again and the newer
// transition is replayed. Silently keeping the newest value would instead hide
// a transition the broker never saw.
func (d *DB) MarkReported(ctx context.Context, brk, runID string, state protocol.RunState) error {
	if err := checkBroker(brk, "MarkReported"); err != nil {
		return err
	}
	if !state.Valid() {
		return fmt.Errorf("store: MarkReported: unknown state %q", state)
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		if err := requireRun(ctx, t, runID, "MarkReported"); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `
			INSERT INTO broker_state (run_id, broker, reported_state, last_acked_seq)
			VALUES (?, ?, ?, 0)
			ON CONFLICT(run_id, broker) DO UPDATE SET reported_state = excluded.reported_state`,
			runID, brk, string(state))
		if err != nil {
			return fmt.Errorf("store: MarkReported %s (%s): %w", runID, brk, err)
		}
		return nil
	})
}

// SetLastAckedSeq persists one broker's log resume point (§7.2, finding F6).
//
// The write is monotonic — MAX of the stored value and the new one — because
// acks can arrive out of order after a reconnect, and a rewind would make the
// agent re-send frames the server already has. That duplicated region is exactly
// the part of the build that was happening when the network dropped, so it is
// the worst possible thing to duplicate.
//
// Monotonicity is per transport, which is the half A28 added and the half that
// matters on a machine running both: the local transport's resume point runs
// ahead of the remote one by however much the network is behind, and before the
// key existed the MAX of the two was written to both.
func (d *DB) SetLastAckedSeq(ctx context.Context, brk, runID string, seq uint64) error {
	if err := checkBroker(brk, "SetLastAckedSeq"); err != nil {
		return err
	}
	stored, err := seqToInt64(seq)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(t *sql.Tx) error {
		if err := requireRun(ctx, t, runID, "SetLastAckedSeq"); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `
			INSERT INTO broker_state (run_id, broker, reported_state, last_acked_seq)
			VALUES (?, ?, NULL, ?)
			ON CONFLICT(run_id, broker) DO UPDATE SET
			  last_acked_seq = MAX(broker_state.last_acked_seq, excluded.last_acked_seq)`,
			runID, brk, stored)
		if err != nil {
			return fmt.Errorf("store: SetLastAckedSeq %s (%s): %w", runID, brk, err)
		}
		return nil
	})
}

// LastAckedSeq reads one broker's resume point back.
//
// A run this transport has never streamed answers 0, which is the correct
// resume point rather than an absence: ingestion begins at 1. A run that does
// not exist answers [ErrNotFound], which is why the query hangs the scalar
// subquery off `FROM runs` instead of selecting from broker_state directly —
// "no row" then means "no such run" and cannot be confused with "no frames yet".
func (d *DB) LastAckedSeq(ctx context.Context, brk, runID string) (uint64, error) {
	if err := checkBroker(brk, "LastAckedSeq"); err != nil {
		return 0, err
	}
	var seq sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT (SELECT last_acked_seq FROM broker_state
		         WHERE run_id = runs.id AND broker = ?)
		  FROM runs WHERE runs.id = ?`, brk, runID).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	case err != nil:
		return 0, fmt.Errorf("store: LastAckedSeq %s (%s): %w", runID, brk, err)
	case !seq.Valid:
		return 0, nil
	case seq.Int64 < 0:
		// Only reachable through a hand-edited database, and the honest answer is
		// to refuse: read as unsigned it would be a resume point in the far
		// future, and the whole of a build's log would be skipped silently.
		return 0, fmt.Errorf("store: run %s has a negative last_acked_seq %d for broker %s", runID, seq.Int64, brk)
	}
	return uint64(seq.Int64), nil
}
