-- 0007_runs_by_commit: the runs on this machine of one commit (amendment
-- A87).
--
-- `latem mcp` answers an agent's "what did this machine build of the commit I
-- pushed". A run's sha is NULL until its ref resolves (§8, gate 9), but a
-- workflow job knows its commit from the moment it is queued, in
-- workflow_json; the expression is the one the lookup spells, so a job still
-- in the queue is found too. Without an index that is a scan of every run this
-- machine has kept; with this one it is the few runs of that commit.
CREATE INDEX runs_commit_idx
  ON runs(coalesce(sha, json_extract(workflow_json, '$.sha')), created_at DESC);
