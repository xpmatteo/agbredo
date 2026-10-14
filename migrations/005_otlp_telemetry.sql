-- Claude Code telemetry sent straight to Agbredo. A token belongs to one run
-- attempt and one source. Only its hash is stored.
CREATE TABLE run_tokens (
 token_hash text PRIMARY KEY,
 run_id text NOT NULL REFERENCES runs(id),
 source text NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX run_tokens_run ON run_tokens(run_id);

-- Hashes of exports already counted, so a retried export is not counted twice.
CREATE TABLE otlp_seen (
 run_id text NOT NULL REFERENCES runs(id),
 source text NOT NULL,
 key text NOT NULL,
 PRIMARY KEY (run_id,source,key)
);

-- The last value of each cumulative counter, to turn it into a delta.
CREATE TABLE otlp_series (
 run_id text NOT NULL REFERENCES runs(id),
 source text NOT NULL,
 series text NOT NULL,
 last_time bigint NOT NULL,
 last_value double precision NOT NULL,
 PRIMARY KEY (run_id,source,series)
);
