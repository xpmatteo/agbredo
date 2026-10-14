# Design

Agbredo shows what a fleet of existing GitHub Actions agents did and what they cost. It only receives reports. It does not start workflows, approve actions or write to GitHub.

## Parts

- **Reporter** (runs in the agent's workflow). Wraps the agent's command, or runs before and after an unchanged step. It sends a `started` and a `finished` report for each source, authenticated with the job's GitHub OIDC token. See `integrations/claude-code/`. `examples/hello-agent/` is a minimal reporter.
- **Server** (Go, one process). Serves the dashboard, takes reports and GitHub webhooks, and polls GitHub every 30 seconds for runs that have not finished.
- **PostgreSQL.** The only store. Records are JSON documents in `jsonb` columns.

## Flow

1. An agent's workflow runs on its own trigger (schedule, push, manual).
2. The reporter posts to `POST /api/agents/{id}/observations`.
3. The server checks the OIDC token (issuer, signature, expiry, audience = `AGBREDO_BASE_URL`). It requires that the repository, workflow file and ref match the registered agent. It then asks GitHub for that run attempt, using the GitHub App, and requires that it matches.
4. The first valid report creates the run. Each report adds one event. Repeated reports are ignored, so totals are not counted twice.
5. GitHub `workflow_run` webhooks and the poll set the run's status from GitHub (running, completed, failed, cancelled). A finished state is never reopened.

A run is registered only when a report arrives. Nothing scans GitHub for old runs.

## Data

| Table | Holds |
| --- | --- |
| `agents` | Registered workflow: repository, workflow file, ref, App installation, estimate threshold. Versioned. Deleting hides it. |
| `runs` | One per workflow run attempt. Keeps a copy of the agent as it was. A trigger blocks changes to that copy. Totals: estimated cost, tokens, API and rate-limit failures. |
| `events` | One per report or status change. The ID is stable, which makes retries safe. |
| `webhook_deliveries` | Delivery IDs already handled. |

Costs are whole microdollars. A missing measurement is stored as missing, not as zero.

## Trust

- **Dashboard:** GitHub OAuth login, an allowlist of GitHub users (`AGBREDO_ADMIN_USERS`), signed session cookie, CSRF token on every form.
- **Runners:** only the short-lived OIDC token. No shared secret.
- **Webhooks:** HMAC signature with the App's webhook secret.
- **Claude credentials:** never seen by the server. A report is a fixed list of fields (counts, exit code, version). It carries no prompts, output or environment.

## Limits

- Costs are estimates of API use. The real subscription charge and the remaining quota are unknown.
- The threshold only marks a run. It cannot stop an agent.
- Only jobs written directly in the registered workflow file can report. Reusable workflows are rejected.
- Run one server replica.
- The report URL still says `observations` and stored agents may carry a `mode` field. Both come from an earlier version and are kept so deployed workflows keep working.
