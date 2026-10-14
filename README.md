# Agbredo — Agent Fleet Control Plane

Agbredo is a Go control plane with PostgreSQL and server-rendered HTML. Existing agents execute Claude Code in GitHub Actions using their existing Claude Pro subscription and `CLAUDE_CODE_OAUTH_TOKEN` Actions secret. Agbredo observes execution status, available token usage, estimated API-equivalent consumption and rate-limit failures. The Go application never receives or stores Claude credentials and does not execute models.

Usage reaches Agbredo one way: [`integrations/claude-code-otlp`](integrations/claude-code-otlp/README.md), one small step before each Claude Code step, after which Claude Code sends its own telemetry to Agbredo. It shows usage per agent run. Keep existing workflows, triggers, installation and authentication. Run status comes from GitHub webhooks and polls. Consumption estimates are not actual subscription charges; absent measurements remain unavailable.

[`docs/10-design.md`](docs/10-design.md) describes the design in a page, and [`docs/20-routes.md`](docs/20-routes.md) lists every route.

## Local development

Requirements: Go 1.26.5 or newer, Docker Compose, and a GitHub App for real GitHub integration. The production Docker build uses the current Go 1.27 stable release family ([Go release notes](https://go.dev/doc/go1.27)). No frontend build is needed.

```sh
docker compose up -d
cp .env.example .env
# Edit .env: set GitHub App credentials, private key path, session key and allowlist.
set -a
. ./.env
set +a
go run ./cmd/agbredo serve
```

Agbredo reads environment variables; it does not load `.env` automatically. `serve` applies the embedded migrations on startup. For an explicit migration step or a built executable:

```sh
go build -o agbredo ./cmd/agbredo
./agbredo migrate
./agbredo serve
```

Open `http://localhost:8080`. For actual workflow events and webhooks, expose the application through a public HTTPS tunnel, set `AGBREDO_BASE_URL` to that exact origin, and use it in the GitHub App settings. GitHub-hosted runners cannot reach your `localhost`.

The repository currently has no Git remote. Its module path is temporarily `agbredo`. Once the actual repository URL exists, update `go.mod` and the internal `agbredo/...` imports to that repository path; no fictional module domain is used.

Upgrading from an earlier version: migration `004_report_only.sql` runs on startup and drops the former approval and dispatch tables (`approvals`, `dispatch_jobs`). The `AGBREDO_APPROVAL_TTL` variable is no longer read. Existing PostgreSQL volumes retain their database names and credentials; use those in `AGBREDO_DATABASE_URL` and keep the Compose PostgreSQL settings and health check aligned with that database.

The report route (`POST /api/agents/{id}/observations`) and its reporter (`integrations/claude-code`) are gone; workflows that still call it get `410 Gone` with a short note to use the telemetry step. Use the telemetry step instead. Events stored by that route (type `claude.execution`) still load and show in the run's timeline as plain events. No migration removes them.

## Configure the GitHub App

Create a GitHub App dedicated to Agbredo. Set its homepage to your public Agbredo URL, its user authorization callback to `https://YOUR-HOST/auth/callback`, and its webhook URL to `https://YOUR-HOST/api/github/webhook`. Set a random webhook secret and enable the **Workflow run** webhook event. Enable user authorization for dashboard login; device flow and user access-token expiration settings are not required by this implementation.

Set **Where can this GitHub App be installed?** to **Only on this account**. This is required. Deployments are created by their first run, so the App installation is the only gate on which repositories can send telemetry to Agbredo. A public App could be installed by any GitHub user, who could then create deployments and send telemetry.

Request these repository permissions:

| Permission | Access | Purpose |
| --- | --- | --- |
| Actions | Read and write | Inspect executions (only read is used today) |
| Contents | Read | Read workflow/repository information |
| Issues | Read and write | Not used today; kept in case Agbredo writes to GitHub again |
| Metadata | Read | Required GitHub App repository metadata access |

No organization or account permissions are needed. Install the App on only the intended agent repositories: a repository with the App installed can send telemetry to Agbredo, and the first run of each of its workflows that asks for a token creates a deployment. Agbredo asks GitHub for the installation itself, as the App, when a deployment is created; runners never send it.

Set `AGBREDO_GITHUB_APP_ID`, `AGBREDO_GITHUB_CLIENT_ID`, `AGBREDO_GITHUB_CLIENT_SECRET`, and `AGBREDO_GITHUB_WEBHOOK_SECRET` (at least 32 random characters). Generate a private key and supply either `AGBREDO_GITHUB_PRIVATE_KEY_FILE` locally or `AGBREDO_GITHUB_PRIVATE_KEY` with the complete PEM and real newlines on Railway. Keep private keys and client secrets out of source control. App installation access tokens are short-lived and managed by `ghinstallation`.

`AGBREDO_DEFAULT_THRESHOLD_USD` (optional) is the per-run estimate threshold, in dollars, given to a deployment created by its first run, for example `2.50`. It is `5` when unset. Each deployment's threshold can then be changed on its page.

Generate a session secret with `openssl rand -hex 32`, set `AGBREDO_SESSION_KEY`, and list administrator GitHub login names in `AGBREDO_ADMIN_USERS` separated by commas. Dashboard routes require GitHub OAuth login and an allowlisted user. Browser mutations use CSRF tokens. GitHub webhooks require a verified signature, and runner API requests require short-lived GitHub OIDC credentials.

Dashboard sessions use signed, HTTP-only cookies and expire after eight hours. HTTPS deployments also mark the cookies secure. The administrator allowlist is checked on each authenticated request; changing it requires restarting the application. Rotating `AGBREDO_SESSION_KEY` invalidates existing sessions.

Browser pages use `Referrer-Policy: same-origin`: same-site form submissions retain their origin for verification, while links to other sites omit the referrer. `no-referrer` can make browsers send `Origin: null` on ordinary form POSTs and caused valid registration forms to be rejected.

HTTP requests log a request ID, method, path, status and duration as JSON. Form verification failures also log a specific `reason`: `origin_unavailable`, `origin_mismatch`, `csrf_token_missing`, `csrf_token_mismatch`, or `invalid_form`. The response explains the cause and includes the same request ID (`X-Request-ID`) for correlating logs. Request bodies, query strings, cookies, credentials and CSRF tokens are not logged. If the page address differs from `AGBREDO_BASE_URL`, open the configured address; if a form belongs to an earlier login session, reload its form page.

## Connect existing Claude Code agents

Follow the [telemetry step setup](integrations/claude-code-otlp/README.md#use). There is nothing to register in Agbredo: install the GitHub App on the repository, and the workflow's first run that asks for a token creates its deployment. Rename it, describe it, change its threshold or disable it on the dashboard. A disabled deployment gets no token for new runs. Add GitHub OIDC permission alongside existing job permissions and put the telemetry step before each Claude Code step. The Claude OAuth token stays in the existing Actions secret and runner environment. Do not add Claude credentials to the Go environment, database or workflow dispatch inputs.

The estimate threshold flags estimated API consumption only. Agents continue running normally, and workflow completion comes from GitHub lifecycle events.

## Try the example agent

`.github/workflows/agbredo-example.yml` runs the telemetry step and then one of two scenarios, chosen when you run it by hand:

- `claude` (the default): a real Claude Code step that says hi. It needs the secret `CLAUDE_CODE_OAUTH_TOKEN`.
- `otlp`: [`examples/fake-telemetry/send.py`](examples/fake-telemetry/send.py) pretends to be Claude Code. It calls no model and sends made-up telemetry, including one repeated export that Agbredo counts once, a model and effort level on its metrics, and one trace span with the effort level.

To use it in a repository where the App is installed, copy the workflow to `.github/workflows/agbredo-example.yml` and, for `otlp`, `examples/fake-telemetry/send.py` to the same path. In that repository the line `uses: ./integrations/claude-code-otlp` must become `uses: xpmatteo/agbredo/integrations/claude-code-otlp@v1`, because the local path exists only in this repository. Set the repository variable `AGBREDO_URL`, then run the workflow by hand. Its first run creates the deployment `owner/repository · agbredo-example.yml`. Its job only needs `id-token: write` and `contents: read`. The telemetry step requests an OIDC token with `AGBREDO_BASE_URL` as its audience. Runs from any branch or tag belong to the deployment. Agbredo verifies the issuer, signature, expiry, audience, repository and workflow file, and then asks GitHub for the same run attempt. Direct jobs and reusable workflows are both supported; the identity is always the caller workflow.

Run `python3 examples/fake-telemetry/send.py` on your machine to see the payloads it would send.

## Spending and recovery

Estimated consumption is stored as integer microdollars (`cost_micros`; 1 USD = 1,000,000), with separate reported token counts and optional cache counters. A repeated telemetry export is counted once. Actual Claude subscription charges and remaining subscription quota are unknown. The dashboard shows an advisory threshold flag and never interrupts agents.

Webhooks plus polling (every 30 seconds) reconcile the state of runs that Agbredo knows but that have not finished, so a lost webhook only delays the update.

## Railway deployment

1. Push this repository to GitHub and create a Railway project from it. `railway.toml` selects the production Dockerfile and `/readyz` readiness check.
2. Add Railway's managed PostgreSQL service. Set `AGBREDO_DATABASE_URL` to `${{Postgres.DATABASE_URL}}` using the actual database service name in your project.
3. Generate a public HTTPS domain for the application and set `AGBREDO_BASE_URL` to its exact origin, without a trailing slash.
4. Set all App/OAuth/webhook credentials, `AGBREDO_GITHUB_PRIVATE_KEY` (multiline PEM), `AGBREDO_SESSION_KEY`, and `AGBREDO_ADMIN_USERS` as Railway variables. Omit the local private-key-file variable.
5. Update the GitHub App callback and webhook URLs to this domain; install the App on the agent repository.
6. Deploy, sign in, and run the example agent. Its first run creates its deployment.

Optional: to keep the full Claude Code telemetry for later analysis, run an OTLP store such as OpenObserve on Railway's private network. Set `OTLP_FORWARD_URL` to its OTLP base, without `/v1/...` (for example `http://openobserve:5080/api/default`), and `OTLP_AUTH_TOKEN` to the base64 of `user:password`. Agbredo then forwards the metrics, logs and traces it accepts. When `OTLP_FORWARD_URL` is unset, nothing is forwarded.

Railway's `PORT` overrides `AGBREDO_PORT` (default `8080`); `DATABASE_URL` is also accepted when `AGBREDO_DATABASE_URL` is unset. The application listens on all interfaces, migrates the database on startup, logs JSON with `slog`, and shuts down gracefully on termination. The final Docker image runs as a non-root user and contains the compiled executable with embedded application assets; no Node pipeline is present.

`GET /healthz` checks application liveness. `GET /readyz` checks database readiness. PostgreSQL is the sole persistent store; retain it and its backups across deployments. Run one application replica for this first milestone.

## Tests and remaining live verification

```sh
go test ./...
python3 -m unittest discover -s examples/fake-telemetry -p 'test_*.py'
```

The PostgreSQL integration suite creates a private schema for each test, applies the migrations there, and drops only that test's schema afterward. It never resets the database or its public schema. The test database role needs permission to create schemas and migration objects; a separate development test database keeps test activity isolated:

```sh
docker compose exec postgres createdb -U agbredo agbredo_test
AGBREDO_TEST_DATABASE_URL='postgres://agbredo:agbredo-local@localhost:5432/agbredo_test?sslmode=disable' go test ./...
```

Automated tests use external GitHub test doubles. Live GitHub Actions, App OAuth and Railway deployment require your credentials and repository configuration. To check a deployment, run the example workflow with each scenario and confirm the run page shows the expected status, agent run, estimates and events.

The primary routes are `/`, `/agents`, `/agents/{id}`, `/runs` and `/runs/{id}`. Machine endpoints are `/api/otlp-token` and, for older workflows, `/api/agents/{id}/otlp-token` (telemetry tokens for runners), `/v1/metrics`, `/v1/logs` and `/v1/traces` (telemetry from Claude Code) and `/api/github/webhook`.
