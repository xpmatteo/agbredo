-- A deployment is identified by repository + workflow file, so each live pair
-- is registered once. GitHub owner and repository names ignore case; workflow
-- file names do not. Hidden (deleted) deployments are left out, so a deleted
-- deployment can be registered again. This fails if live duplicates exist:
-- they are removed by hand first. It changes no data.
CREATE UNIQUE INDEX agents_repository_workflow ON agents (
 lower(document->>'owner'),
 lower(document->>'repository'),
 (document->>'workflow')
) WHERE NOT deleted;
