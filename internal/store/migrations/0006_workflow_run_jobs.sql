-- 0006_workflow_run_jobs: the runs on this machine of one workflow run
-- (amendment A84).
--
-- A download-artifact may read what a job it needs uploaded, and the control
-- plane places it on the machine that ran that job. The downloading run finds
-- the uploading one by its workflow run's id, which is in workflow_json.
-- Without an index that is a scan of every run this machine has kept, on every
-- such download; with this one it is the few jobs of one workflow run. An
-- expression rather than a new column, so the runs already stored are in it
-- with nothing to backfill.
CREATE INDEX runs_workflow_run_idx
  ON runs(repo, json_extract(workflow_json, '$.workflow_run_id'), created_at DESC)
  WHERE workflow_json IS NOT NULL;
