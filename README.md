# Agbredo — Agent Fleet Control Plane

Agbredo is a Go control plane with PostgreSQL and server-rendered HTML. Existing agents execute Claude Code in GitHub Actions using their existing Claude Pro subscription and `CLAUDE_CODE_OAUTH_TOKEN` Actions secret. Agbredo observes execution status, available token usage, estimated API-equivalent consumption and rate-limit failures. The Go application never receives or stores Claude credentials and does not execute models.

The [reusable reporting integration](integrations/claude-code/README.md) wraps the existing execution command with runner-local OpenTelemetry collection and GitHub OIDC reporting. Keep existing workflows, triggers, installation and authentication. No provider replacement, workflow renaming or dispatch inputs are required. Consumption estimates are not actual subscription charges; absent measurements remain unavailable. Claude Code **2.1.296** was verified locally; capability evidence and verification commands are included with the integration.

The simpler way is [`integrations/claude-code-otlp`](integrations/claude-code-otlp/README.md): one small step before each Claude Code step, and Claude Code sends its own telemetry to Agbredo. It shows usage per step. The wrapper above keeps working.

[`docs/design.md`](docs/design.md) describes the design in a page, and [`docs/routes.md`](docs/routes.md) lists every route. [`examples/hello-agent`](examples/hello-agent/) holds pretend agents that report without calling a model.

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

Upgrading from an earlier version: migration `004_report_only.sql` runs on startup and drops the former approval and dispatch tables (`approvals`, `dispatch_jobs`). The `AGBREDO_APPROVAL_TTL` variable is no longer read. Reports from deployed workflows keep working, because the report URL (`/api/agents/{id}/observations`) and the stored run data are unchanged. Existing PostgreSQL volumes retain their database names and credentials; use those in `AGBREDO_DATABASE_URL` and keep the Compose PostgreSQL settings and health check aligned with that database.

## Configure the GitHub App

Create a GitHub App dedicated to Agbredo. Set its homepage to your public Agbredo URL, its user authorization callback to `https://YOUR-HOST/auth/callback`, and its webhook URL to `https://YOUR-HOST/api/github/webhook`. Set a random webhook secret and enable the **Workflow run** webhook event. Enable user authorization for dashboard login; device flow and user access-token expiration settings are not required by this implementation.

Request these repository permissions:

| Permission | Access | Purpose |
| --- | --- | --- |
| Actions | Read and write | Inspect executions (only read is used today) |
| Contents | Read | Read workflow/repository information |
| Issues | Read and write | Not used today; kept in case Agbredo writes to GitHub again |
| Metadata | Read | Required GitHub App repository metadata access |

No organization or account permissions are needed. Install the App on only the intended agent repositories. Record the numeric installation ID from the installation settings URL (the number following `/installations/`). The installation ID is configured per agent; it is not a shared runner secret.

Set `AGBREDO_GITHUB_APP_ID`, `AGBREDO_GITHUB_CLIENT_ID`, `AGBREDO_GITHUB_CLIENT_SECRET`, and `AGBREDO_GITHUB_WEBHOOK_SECRET` (at least 32 random characters). Generate a private key and supply either `AGBREDO_GITHUB_PRIVATE_KEY_FILE` locally or `AGBREDO_GITHUB_PRIVATE_KEY` with the complete PEM and real newlines on Railway. Keep private keys and client secrets out of source control. App installation access tokens are short-lived and managed by `ghinstallation`.

Generate a session secret with `openssl rand -hex 32`, set `AGBREDO_SESSION_KEY`, and list administrator GitHub login names in `AGBREDO_ADMIN_USERS` separated by commas. Dashboard routes require GitHub OAuth login and an allowlisted user. Browser mutations use CSRF tokens. GitHub webhooks require a verified signature, and runner API requests require short-lived GitHub OIDC credentials.

Dashboard sessions use signed, HTTP-only cookies and expire after eight hours. HTTPS deployments also mark the cookies secure. The administrator allowlist is checked on each authenticated request; changing it requires restarting the application. Rotating `AGBREDO_SESSION_KEY` invalidates existing sessions.

Browser pages use `Referrer-Policy: same-origin`: same-site form submissions retain their origin for verification, while links to other sites omit the referrer. `no-referrer` can make browsers send `Origin: null` on ordinary form POSTs and caused valid registration forms to be rejected.

HTTP requests log a request ID, method, path, status and duration as JSON. Form verification failures also log a specific `reason`: `origin_unavailable`, `origin_mismatch`, `csrf_token_missing`, `csrf_token_mismatch`, or `invalid_form`. The response explains the cause and includes the same request ID (`X-Request-ID`) for correlating logs. Request bodies, query strings, cookies, credentials and CSRF tokens are not logged. If the page address differs from `AGBREDO_BASE_URL`, open the configured address; if a form belongs to an earlier login session, reload its form page.

## Connect existing Claude Code agents

Follow the [reporting integration setup](integrations/claude-code/README.md#minimal-workflow-change). Register the existing workflow in Agbredo. Add GitHub OIDC permission alongside existing job permissions and wrap only its execution step. The Claude OAuth token stays in the existing Actions secret and runner environment. Do not add Claude credentials to the Go environment, database, workflow dispatch inputs or reporting payload.

The estimate threshold flags estimated API consumption only. Agents continue running normally, and workflow completion comes from GitHub lifecycle events.

## Try the example agents

`examples/hello-agent` holds pretend agents (`hello`, `play`, `rate-limited`, `spendthrift`, `silent`, `cancelled`). They call no model and send made-up numbers, so you can see every kind of run on the dashboard. Copy these files into a repository where the App is installed:

```text
examples/hello-agent/agent.py            -> examples/hello-agent/agent.py
examples/hello-agent/agbredo-example.yml -> .github/workflows/agbredo-example.yml
```

Register an agent with workflow `agbredo-example.yml`, set the repository variables `AGBREDO_URL` and `AGBREDO_AGENT_ID`, then run the workflow by hand and choose a scenario. Its job only needs `id-token: write` and `contents: read`. A fresh OIDC token is requested for each report with `AGBREDO_BASE_URL` as its audience. Agbredo verifies the issuer, signature, expiry, audience, repository, workflow file and configured reference, and then asks GitHub for the same run attempt. Direct jobs are supported; reusable workflows are rejected by the verifier.

The `otlp` scenario shows the telemetry path. It runs the `integrations/claude-code-otlp` action, then sends made-up Claude Code telemetry, including one repeated export that is counted once. Copy `integrations/claude-code-otlp` as well to use it.

Run `python3 examples/hello-agent/agent.py play` on your machine to see the reports it would send.

## Spending and recovery

Estimated consumption is stored as integer microdollars (`cost_micros`; 1 USD = 1,000,000), with separate reported token counts and optional cache counters. Stable event IDs prevent duplicate accounting on retries. Actual Claude subscription charges and remaining subscription quota are unknown. The dashboard shows an advisory threshold flag and never interrupts agents.

Webhooks plus polling (every 30 seconds) reconcile the state of runs that were reported but not yet finished, so a lost webhook only delays the update.

## Railway deployment

1. Push this repository to GitHub and create a Railway project from it. `railway.toml` selects the production Dockerfile and `/readyz` readiness check.
2. Add Railway's managed PostgreSQL service. Set `AGBREDO_DATABASE_URL` to `${{Postgres.DATABASE_URL}}` using the actual database service name in your project.
3. Generate a public HTTPS domain for the application and set `AGBREDO_BASE_URL` to its exact origin, without a trailing slash.
4. Set all App/OAuth/webhook credentials, `AGBREDO_GITHUB_PRIVATE_KEY` (multiline PEM), `AGBREDO_SESSION_KEY`, and `AGBREDO_ADMIN_USERS` as Railway variables. Omit the local private-key-file variable.
5. Update the GitHub App callback and webhook URLs to this domain; install the App on the agent repository.
6. Deploy, sign in, register the example agent and run a few of its scenarios.

Railway's `PORT` overrides `AGBREDO_PORT` (default `8080`); `DATABASE_URL` is also accepted when `AGBREDO_DATABASE_URL` is unset. The application listens on all interfaces, migrates the database on startup, logs JSON with `slog`, and shuts down gracefully on termination. The final Docker image runs as a non-root user and contains the compiled executable with embedded application assets; no Node pipeline is present.

`GET /healthz` checks application liveness. `GET /readyz` checks database readiness. PostgreSQL is the sole persistent store; retain it and its backups across deployments. Run one application replica for this first milestone.

## Tests and remaining live verification

```sh
go test ./...
python3 -m unittest discover -s examples/hello-agent -p 'test_*.py'
python3 -m unittest discover -s integrations/claude-code -p 'test_*.py'
```

The PostgreSQL integration suite creates a private schema for each test, applies the migrations there, and drops only that test's schema afterward. It never resets the database or its public schema. The test database role needs permission to create schemas and migration objects; a separate development test database keeps test activity isolated:

```sh
docker compose exec postgres createdb -U agbredo agbredo_test
AGBREDO_TEST_DATABASE_URL='postgres://agbredo:agbredo-local@localhost:5432/agbredo_test?sslmode=disable' go test ./...
```

Automated tests use external GitHub test doubles. Live GitHub Actions, App OAuth and Railway deployment require your credentials and repository configuration. To check a deployment, run the example workflow with each scenario and confirm the run page shows the expected status, estimates and events.

The primary routes are `/`, `/agents`, `/agents/{id}`, `/runs` and `/runs/{id}`. Machine endpoints are `/api/agents/{id}/observations` (reports from runners) and `/api/github/webhook`.
