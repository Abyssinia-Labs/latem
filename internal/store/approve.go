package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Abyssinia-Labs/latem/pkg/protocol"
)

// ErrNotAwaitingApproval means the run was not parked for a human. It is a
// caller error rather than a race: nothing else moves a run into
// awaiting_approval, so a run that is somewhere else was never waiting on this.
var ErrNotAwaitingApproval = errors.New("store: run is not awaiting approval")

// ErrNoCommit means the run has no resolved sha, so there is nothing to pin.
//
// A parked run written by this version of the agent always has one — gate 9
// resolves before gate 8 parks, precisely so that it does. The case that
// remains is a row parked by an older agent and approved after the upgrade:
// nobody can say which commit that approval would have meant, and guessing
// "whatever the ref points at when it eventually builds" is the force-push hole
// this whole mechanism exists to close.
var ErrNoCommit = errors.New("store: run has no resolved commit to approve")

// Approve records a local approval and queues the run (INV-5, amendment A27).
//
// The transaction is the security property, not a tidiness preference. Reading
// the run's sha and writing it into approved_sha are one step, so there is no
// instant at which a concurrent writer could change the commit between the
// value the caller was shown and the value that gets pinned.
//
// What it writes:
//
//   - approved_at and approved_sha, the record that a human said yes and to
//     exactly which commit;
//   - state = queued, because §11 has no awaiting_approval → claimed edge and
//     approving a run says it may be built, not that it must be built next;
//   - expires_at = the queue_ttl deadline, replacing the approval_ttl one;
//   - the queue row, so the agent's next poll finds it.
//
// Approving a run that is already queued is a no-op success and writes nothing.
// Approving twice is something a person does — the second `latem approve`
// should not read as an error — and the first one already pinned the commit, so
// there is nothing a second could add.
func (d *DB) Approve(ctx context.Context, runID string, approvedAt, expiresAt time.Time) (Run, error) {
	var out Run
	err := d.tx(ctx, func(t *sql.Tx) error {
		var (
			state protocol.RunState
			sha   sql.NullString
		)
		err := t.QueryRowContext(ctx,
			`SELECT state, sha FROM runs WHERE id = ?`, runID).Scan(&state, &sha)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s", ErrNotFound, runID)
		}
		if err != nil {
			return fmt.Errorf("store: Approve %s: %w", runID, err)
		}

		switch state {
		case protocol.StateQueued:
			r, getErr := getRun(ctx, t, runID)
			if getErr != nil {
				return getErr
			}
			out = r
			return nil
		case protocol.StateAwaitingApproval:
		default:
			return fmt.Errorf("%w: run %s is %s", ErrNotAwaitingApproval, runID, state)
		}

		if sha.String == "" {
			return fmt.Errorf("%w: run %s", ErrNoCommit, runID)
		}

		// expires_at is set here rather than left to the caller for the reason
		// [DB.Transition] cannot do it: a run that kept its approval_ttl would
		// be swept as an expired queue entry by the very next ticker.
		res, err := t.ExecContext(ctx, `
			UPDATE runs
			   SET state = ?, expires_at = ?, approved_at = ?, approved_sha = sha
			 WHERE id = ? AND state = ?`,
			string(protocol.StateQueued), nullTime(&expiresAt), nullTime(&approvedAt),
			runID, string(protocol.StateAwaitingApproval))
		if err != nil {
			return fmt.Errorf("store: Approve %s: %w", runID, err)
		}
		if err := affectedOne(res, runID, "Approve"); err != nil {
			return err
		}

		if _, err := t.ExecContext(ctx, `
			INSERT INTO queue (run_id, priority, enqueued_at) VALUES (?, 0, ?)
			ON CONFLICT(run_id) DO UPDATE SET priority = excluded.priority`,
			runID, toUnix(approvedAt)); err != nil {
			return fmt.Errorf("store: Approve %s: enqueue: %w", runID, err)
		}

		r, err := getRun(ctx, t, runID)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

// getRun reads one run inside an open transaction.
func getRun(ctx context.Context, t *sql.Tx, runID string) (Run, error) {
	row := t.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: reading run %s: %w", runID, err)
	}
	return r, nil
}
