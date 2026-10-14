-- Agbredo only receives reports from existing workflows. Remove the dispatch
-- and approval machinery of the former managed mode.
DROP TRIGGER IF EXISTS immutable_approval ON approvals;
DROP FUNCTION IF EXISTS protect_approval_payload();
DROP TABLE IF EXISTS dispatch_jobs;
DROP TABLE IF EXISTS approvals;

ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK(status IN ('queued','running','completed','failed','cancelled'));

DROP INDEX IF EXISTS observed_github_runs;
CREATE INDEX runs_github_run ON runs ((document->>'github_run_id'));
