-- Applied at every start. No migrations: change this file and nuke the DB.
CREATE TABLE IF NOT EXISTS events (
  delivery_id    text PRIMARY KEY,          -- X-GitHub-Delivery; a redelivery is ignored
  received_at    timestamptz NOT NULL DEFAULT now(),
  repository     text NOT NULL,             -- owner/name
  run_id         bigint NOT NULL,
  run_attempt    int NOT NULL,
  action         text NOT NULL,             -- requested | in_progress | completed
  status         text NOT NULL,
  conclusion     text,                      -- null until completed
  workflow_name  text NOT NULL,
  html_url       text NOT NULL,
  head_branch    text,
  event          text,
  actor          text,
  run_started_at timestamptz,
  updated_at     timestamptz NOT NULL,      -- workflow_run.updated_at
  pipelines      jsonb NOT NULL,            -- [{"id": "...", "version": "..."}]
  payload        jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS events_run ON events (run_id, run_attempt, updated_at, received_at);

-- PRs and Issues opened by a Run. We know the Run from the Agbredo-Run marker,
-- the last line of the body, read from the pull_request or issues webhook.
CREATE TABLE IF NOT EXISTS opened (
  delivery_id    text PRIMARY KEY,          -- X-GitHub-Delivery; a redelivery is ignored
  received_at    timestamptz NOT NULL DEFAULT now(),
  repository     text NOT NULL,             -- owner/name
  kind           text NOT NULL,             -- pull_request | issue
  number         int NOT NULL,
  title          text NOT NULL,
  html_url       text NOT NULL,
  opened_at      timestamptz NOT NULL,      -- created_at of the PR or Issue
  run_id         bigint NOT NULL,
  run_attempt    int NOT NULL
);
CREATE INDEX IF NOT EXISTS opened_run ON opened (run_id, run_attempt, opened_at);
