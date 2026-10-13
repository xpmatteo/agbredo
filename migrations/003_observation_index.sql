CREATE INDEX observed_github_runs ON runs ((document->>'github_run_id'))
 WHERE document->'agent'->>'mode'='observe';
