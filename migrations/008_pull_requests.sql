-- Pull requests that agents open, from pull_request webhooks: bot PRs in a
-- repository with a live deployment, and PRs that carry an Agbredo-Run
-- trailer. id is "<repository id>:<number>". link says how sure Agbredo is of
-- the run that opened it: declared (a valid trailer), inferred (the only run
-- whose time window holds the PR's opening) or unlinked. A linked PR has a run;
-- an unlinked one has none. The document keeps the facts as at opening (title,
-- author, refs, trailer, reason, findings), not what happens to the PR later.
CREATE TABLE pull_requests (
 id text PRIMARY KEY,
 owner text NOT NULL,
 repository text NOT NULL,
 number integer NOT NULL,
 run_id text REFERENCES runs(id),
 link text NOT NULL CHECK (link IN ('declared', 'inferred', 'unlinked')),
 document jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((link = 'unlinked') = (run_id IS NULL))
);
CREATE INDEX pull_requests_run ON pull_requests(run_id);
CREATE INDEX pull_requests_repository ON pull_requests(lower(owner), lower(repository));
