CREATE TABLE agents (
 id text PRIMARY KEY,
 document jsonb NOT NULL,
 deleted boolean NOT NULL DEFAULT false
);
CREATE TABLE runs (
 id text PRIMARY KEY,
 agent_id text NOT NULL REFERENCES agents(id),
 correlation_id text NOT NULL UNIQUE,
 status text NOT NULL CHECK(status IN ('queued','running','waiting_approval','completed','failed','cancelled')),
 document jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX runs_status ON runs(status);
CREATE TABLE approvals (
 id text PRIMARY KEY,
 run_id text NOT NULL UNIQUE REFERENCES runs(id),
 status text NOT NULL CHECK(status IN ('pending','approved','executing','execution_unknown','executed','rejected','expired')),
 document jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX approvals_status ON approvals(status);
CREATE TABLE events (
 run_id text NOT NULL REFERENCES runs(id),
 id text NOT NULL,
 document jsonb NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (run_id,id)
);
CREATE INDEX events_timeline ON events(run_id,received_at);
CREATE TABLE dispatch_jobs (
 id text PRIMARY KEY,
 run_id text NOT NULL REFERENCES runs(id),
 approval_id text REFERENCES approvals(id),
 attempts integer NOT NULL DEFAULT 0,
 available_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,
 finished boolean NOT NULL DEFAULT false,
 last_error text NOT NULL DEFAULT ''
);
CREATE TABLE webhook_deliveries (
 id text PRIMARY KEY,
 received_at timestamptz NOT NULL DEFAULT now()
);
