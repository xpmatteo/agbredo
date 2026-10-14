# Design

The routes are listed in `02-routes.md`.

Agbredo shows what a fleet of existing GitHub Actions agents did and what they cost.
## Parts

- **Reporter** (runs in the agent's workflow). Wraps the agent's command, or runs before and after an unchanged step. It sends a `started` and a `finished` report for each source, authenticated with the job's GitHub OIDC token. See `integrations/claude-code/`. `examples/hello-agent/` is a minimal reporter.
- **Telemetry step** (runs in the agent's workflow, before each Claude Code step). Gets a token from the server with the job's OIDC token, then points Claude Code's own OpenTelemetry export at the server. No wrapper or sidecar. See `integrations/claude-code-otlp/`.
- **Server** (Go, one process). Serves the dashboard, takes reports and GitHub webhooks, and polls GitHub every 30 seconds for runs that have not finished.
- **PostgreSQL.** The store for the dashboard. Records are JSON documents in `jsonb` columns.
- **Telemetry store** (optional, OpenObserve on Railway's private network). Keeps the full telemetry for later analysis. Only Agbredo can reach it. See `internal/forward/`.

## Flow

1. An agent's workflow runs on its own trigger (schedule, push, manual).
2. The reporter posts to `POST /api/agents/{id}/observations`.
3. The server checks the OIDC token (issuer, signature, expiry, audience = `AGBREDO_BASE_URL`). It requires that the repository, workflow file and ref match the registered agent. It then asks GitHub for that run attempt, using the GitHub App, and requires that it matches.
4. The first valid report creates the run. Each report adds one event. Repeated reports are ignored, so totals are not counted twice.
5. GitHub `workflow_run` webhooks and the poll set the run's status from GitHub (running, completed, failed, cancelled). A finished state is never reopened.

### Telemetry flow

1. The telemetry step posts to `POST /api/agents/{id}/otlp-token` with the OIDC token and a `source` name. The server runs the checks in step 3 above, creates the run if it is new, and returns a random token. The token is valid for 6 hours or until the run finishes. Only its hash is stored. It is tied to one run attempt and one source.
2. The step puts the token in the environment as `OTEL_EXPORTER_OTLP_HEADERS`. Claude Code then posts OTLP as JSON to `/v1/metrics`, `/v1/logs` and `/v1/traces`. The step also sets `TRACEPARENT` from the token reply. Its trace ID comes from the run ID, so all Claude Code steps of a run attempt share one trace. Traces are a Claude Code beta (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`).
3. The token alone says which run and which source the data belongs to. The server does not read that from the payload.
4. In PostgreSQL the server keeps only token counts (input, output, cache read, cache creation), the cost estimate, the Claude Code version, and counts of API errors and rate-limit errors. Nothing is counted from traces. It never logs the payload.
5. If `OTLP_FORWARD_URL` is set, the server forwards each accepted payload, unchanged except for added resource attributes: `agbredo.agent.id`, `agbredo.agent.name`, `agbredo.run.id`, `agbredo.source`, `github.repository`, `github.workflow`, `github.run_id`, `github.run_attempt`, `github.run_url`, `github.actor`, `github.triggering_actor`. They come from the token and the run, not from the agent. Attributes starting with `agbredo.` or `github.` that the agent sent are removed. Forwarding runs in the background. If the store is slow or down, the telemetry is dropped and a warning is logged. The agent's request never fails because of it.
6. Delta and cumulative counters are both handled. A repeated export is counted once. A counter that goes down is ignored. Counts are kept per source on the run (`sources`), and added to the run totals.

This path and the reports above can be used together or alone. Run status always comes from GitHub, never from telemetry.

A run is registered only when a report arrives or a token is requested. Nothing scans GitHub for old runs.

## Data

| Table | Holds |
| --- | --- |
| `agents` | Registered workflow: repository, workflow file, ref, App installation, estimate threshold. Versioned. Deleting hides it. |
| `runs` | One per workflow run attempt. Keeps a copy of the agent as it was. A trigger blocks changes to that copy. Totals: estimated cost, tokens, API and rate-limit failures. The GitHub logins of `actor` (who started the run) and `triggering_actor` (who started this attempt), from GitHub when the run is first seen. |
| `run_tokens` | Hash, run, source and expiry of each telemetry token. |
| `otlp_seen`, `otlp_series` | Hashes of exports already counted, and the last value of each cumulative counter. |
| `events` | One per report or status change. The ID is stable, which makes retries safe. |
| `webhook_deliveries` | Delivery IDs already handled. |

Costs are whole microdollars. A missing measurement is stored as missing, not as zero.

## Trust

- **Dashboard:** GitHub OAuth login, an allowlist of GitHub users (`AGBREDO_ADMIN_USERS`), signed session cookie, CSRF token on every form.
- **Runners:** only the short-lived OIDC token. No shared secret.
- **Webhooks:** HMAC signature with the App's webhook secret.
- **Telemetry tokens:** random, per run attempt and per source, 6 hours at most, refused once the run has finished. The agent's own process can read its token. With it, it can only send telemetry for its own run.
- **Claude credentials:** never seen by the server. A report is a fixed list of fields (counts, exit code, version). It carries no prompts, output or environment. Telemetry arrives as raw OTLP over HTTPS. The server keeps only the fields listed above in PostgreSQL, and forwards the rest to the telemetry store if one is set. The telemetry step turns off prompt, response and tool logging. It turns on the Claude account fields (`user.account_uuid`, along with `user.email` and `organization.id` which Claude Code always sends), so the store records which Claude account did the work.
- **Telemetry store:** on the private network only. Agbredo logs in with Basic auth (`OTLP_AUTH_TOKEN`). Runners never get its credentials.

## Limits

- Costs are estimates of API use. The real subscription charge and the remaining quota are unknown.
- The threshold only marks a run. It cannot stop an agent.
- Only jobs written directly in the registered workflow file can report. Reusable workflows are rejected.
- Telemetry is accepted only as OTLP JSON (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`), not protobuf.
- Telemetry sent after GitHub marks the run finished is refused. Claude Code sends its last export when it exits, which is before the run finishes. A job that is killed before that loses the last part of its usage.
- Forwarding is best effort. Telemetry dropped while the store is down is not sent again. A repeated export is forwarded again, though it is counted once in PostgreSQL.
- Retention in the telemetry store is not decided yet.
- Run one server replica.
- The report URL still says `observations` and stored agents may carry a `mode` field. Both come from an earlier version and are kept so deployed workflows keep working.
