-- 0005_workflow_runs: a workflow run's job (amendment A52).
--
-- Two changes, both forced by protocol 2.0.
--
-- skipped is a new terminal state — a workflow job whose if: was false as it
-- started — so runs_state_idx, defined as the complement of the terminal set,
-- has to be rebuilt to exclude it. Left alone, every skipped job would sit in
-- the hot index forever and "what is in flight?" would list them.
--
-- workflow_json holds what a workflow run's job carries beyond a run: the
-- workflow file, the commit to build, the event, its needs' results and
-- outputs, and the workflow run's id and numbers. One column of JSON rather
-- than seven, because none of it is queried — it is read back whole when a run
-- that was queued before a restart is built. The job's GitHub token is NOT
-- here: A52 holds it in memory only, and nothing writes it to disk.

DROP INDEX runs_state_idx;
CREATE INDEX runs_state_idx ON runs(state) WHERE state NOT IN
  ('succeeded','failed','cancelled','rejected','timed_out','interrupted','skipped');

ALTER TABLE runs ADD COLUMN workflow_json TEXT;
