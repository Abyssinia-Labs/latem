-- 0003_broker_state: reporting state becomes per transport (amendment A28).
--
-- 0001 gave the runs table one `reported_state` and one `last_acked_seq`,
-- written in §22 as though a machine had one broker. It has two. The two words
-- mean different things on each:
--
--   * internal/broker/local marks reported_state the moment a transition is
--     persisted and advances last_acked_seq on every batch write, because for
--     that transport "durably accepted" is "written to this disk".
--   * internal/broker/remote needs both to mean "what the control plane has
--     durably accepted", which a partition can leave arbitrarily far behind.
--
-- With both transports live the single columns collide, and they collide in the
-- direction that loses data. store.Unreported returns nothing for the remote
-- transport because the local one has already marked every row, so a run that
-- finished during an outage is never replayed; and the resume frame claims the
-- control plane holds frames it has never seen, so ingestion restarts past the
-- gap. The frames lost are precisely the ones covering the outage — the failure
-- LogSink and finding F6 exist to prevent.
--
-- A keyed table rather than reported_state_local / reported_state_remote
-- columns: per-transport columns do not scale past two and encode the list of
-- transports in the schema, where adding one is a migration instead of a
-- constant.
--
-- The old values are moved under broker = 'local' before the columns go, so a
-- machine with a month of history keeps its local reporting state and does not
-- replay it all at the first start after the upgrade. Only rows that carry
-- information are moved: a run with a NULL reported_state and a zero resume
-- point is indistinguishable from a run with no row at all, and both read as
-- "nothing has been acknowledged", so materialising them would be pure bloat.
--
-- The columns are then DROPPED rather than left behind. Leaving them would
-- create the second source of truth this migration exists to eliminate, and the
-- next reader would have no way to tell which one was live. sqlite has
-- supported ALTER TABLE … DROP COLUMN since 3.35; the bundled modernc.org/sqlite
-- reports 3.53, and neither column is indexed or named in a partial index
-- predicate, which are the two conditions under which sqlite refuses the drop.

CREATE TABLE broker_state (
  run_id          TEXT NOT NULL REFERENCES runs(id),
  broker          TEXT NOT NULL,  -- "local" | "remote"; see store.BrokerLocal
  reported_state  TEXT,           -- last state THIS transport acknowledged
  last_acked_seq  INTEGER NOT NULL DEFAULT 0,  -- THIS transport's resume point
  PRIMARY KEY (run_id, broker)
);

INSERT INTO broker_state (run_id, broker, reported_state, last_acked_seq)
  SELECT id, 'local', reported_state, last_acked_seq
    FROM runs
   WHERE reported_state IS NOT NULL OR last_acked_seq <> 0;

ALTER TABLE runs DROP COLUMN reported_state;
ALTER TABLE runs DROP COLUMN last_acked_seq;
