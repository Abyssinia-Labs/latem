-- 0001_init: the RUNNER_SPEC §22 schema, verbatim.
--
-- The two partial indexes are the load-bearing part. runs_state_idx covers
-- exactly the non-terminal states, so the hot query — "what is in flight?" —
-- never pays for a year of finished runs. runs_expiry_idx covers exactly the
-- two waiting states, so the TTL sweep is an indexed range scan rather than a
-- table walk. Both lists are duplicated from pkg/protocol; migration_test.go
-- asserts they still agree, because a state added there without a migration
-- here silently changes which rows these indexes cover.

CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  repo              TEXT NOT NULL,
  job               TEXT NOT NULL,
  ref               TEXT NOT NULL,  -- as supplied, post-validation
  ref_normalized    TEXT,           -- refs/heads/main — what policy matched (INV-5)
  sha               TEXT,
  runner            TEXT,
  state             TEXT NOT NULL,
  exit_code         INTEGER,
  error_code        TEXT,
  error_message     TEXT,           -- redacted
  origin_kind       TEXT NOT NULL,
  origin_actor      TEXT,
  cancel_reason     TEXT,           -- redacted, display only
  inputs_json       TEXT NOT NULL DEFAULT '{}',
  created_at        INTEGER NOT NULL,
  started_at        INTEGER,
  finished_at       INTEGER,
  expires_at        INTEGER,        -- approval_ttl / queue_ttl deadline (§11)
  reported_state    TEXT,           -- last state acked by the broker
  last_acked_seq    INTEGER NOT NULL DEFAULT 0,  -- log resume point (§7.2)
  UNIQUE(id)
);
CREATE INDEX runs_state_idx ON runs(state) WHERE state NOT IN
  ('succeeded','failed','cancelled','rejected','timed_out','interrupted');
CREATE INDEX runs_repo_created_idx ON runs(repo, created_at DESC);
CREATE INDEX runs_expiry_idx ON runs(expires_at)
  WHERE state IN ('queued','awaiting_approval');

CREATE TABLE queue (
  run_id      TEXT PRIMARY KEY REFERENCES runs(id),
  priority    INTEGER NOT NULL DEFAULT 0,
  enqueued_at INTEGER NOT NULL
);

CREATE TABLE artifacts (
  run_id     TEXT NOT NULL REFERENCES runs(id),
  path       TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  sha256     TEXT NOT NULL,
  expires_at INTEGER,
  PRIMARY KEY (run_id, path)
);

CREATE TABLE cache_meta (
  name          TEXT PRIMARY KEY,   -- volume or directory name
  runner        TEXT NOT NULL,
  size_bytes    INTEGER,
  last_used_at  INTEGER
);
