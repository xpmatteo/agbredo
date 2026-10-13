# Agbredo — Agent Fleet Control Plane

Agbredo is a Go control plane with PostgreSQL and server-rendered HTML. Existing agents execute Claude Code in GitHub Actions using their existing Claude Pro subscription and `CLAUDE_CODE_OAUTH_TOKEN` Actions secret. Agbredo observes execution status, available token usage, estimated API-equivalent consumption and rate-limit failures. The Go application never receives or stores Claude credentials and does not execute models.

Start with **Observe existing workflows**. The [reusable reporting integration](integrations/claude-code/README.md) wraps the existing execution command with runner-local OpenTelemetry collection and GitHub OIDC reporting. Keep existing workflows, triggers, installation and authentication. No provider replacement, workflow renaming or dispatch inputs are required. Consumption estimates are not actual subscription charges; absent measurements remain unavailable. Claude Code **2.1.296** was verified locally; capability evidence and verification commands are included with the integration.

The original deterministic issue-approval example remains available in optional **Managed dispatch and approvals** mode. It calls no model and reports zero model usage. Its initial workflow proposes an issue and exits; an approved continuation asks Agbredo to create the exact issue through the GitHub App. These example workflows are unchanged and are not a replacement for existing Claude Code agents.

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

For an existing installation, switch configuration to `AGBREDO_*` variables and install the renamed workflow templates. Finish pending runs and approval reconciliation before upgrading: workflow inputs, correlation names, and issue recovery markers now use the new project name. Existing PostgreSQL volumes retain their database names and credentials; use those in `AGBREDO_DATABASE_URL` and keep the Compose PostgreSQL settings and health check aligned with that database.

## Configure the GitHub App

Create a GitHub App dedicated to Agbredo. Set its homepage to your public Agbredo URL, its user authorization callback to `https://YOUR-HOST/auth/callback`, and its webhook URL to `https://YOUR-HOST/api/github/webhook`. Set a random webhook secret and enable the **Workflow run** webhook event. Enable user authorization for dashboard login; device flow and user access-token expiration settings are not required by this implementation.

Request these repository permissions:

| Permission | Access | Purpose |
| --- | --- | --- |
| Actions | Read for observation; read and write for managed mode | Inspect executions; optionally dispatch managed workflows |
| Contents | Read | Read workflow/repository information |
| Issues | Read and write for managed mode only | Create the approved example issue and find its recovery marker |
| Metadata | Read | Required GitHub App repository metadata access |

No organization or account permissions are needed. Install the App on only the intended agent repositories. Record the numeric installation ID from the installation settings URL (the number following `/installations/`). The installation ID is configured per agent; it is not a shared runner secret.

Set `AGBREDO_GITHUB_APP_ID`, `AGBREDO_GITHUB_CLIENT_ID`, `AGBREDO_GITHUB_CLIENT_SECRET`, and `AGBREDO_GITHUB_WEBHOOK_SECRET` (at least 32 random characters). Generate a private key and supply either `AGBREDO_GITHUB_PRIVATE_KEY_FILE` locally or `AGBREDO_GITHUB_PRIVATE_KEY` with the complete PEM and real newlines on Railway. Keep private keys and client secrets out of source control. App installation access tokens are short-lived and managed by `ghinstallation`.

Generate a session secret with `openssl rand -hex 32`, set `AGBREDO_SESSION_KEY`, and list administrator GitHub login names in `AGBREDO_ADMIN_USERS` separated by commas. Dashboard routes require GitHub OAuth login and an allowlisted user. Approval decisions also check authorization in the service layer. Browser mutations use CSRF tokens. GitHub webhooks require a verified signature, and runner API requests require short-lived GitHub OIDC credentials.

Dashboard sessions use signed, HTTP-only cookies and expire after eight hours. HTTPS deployments also mark the cookies secure. The administrator allowlist is checked on each authenticated request; changing it requires restarting the application. Rotating `AGBREDO_SESSION_KEY` invalidates existing sessions.

Browser pages use `Referrer-Policy: same-origin`: same-site form submissions retain their origin for verification, while links to other sites omit the referrer. `no-referrer` can make browsers send `Origin: null` on ordinary form POSTs and caused valid registration forms to be rejected.

HTTP requests log a request ID, method, path, status and duration as JSON. Form verification failures also log a specific `reason`: `origin_unavailable`, `origin_mismatch`, `csrf_token_missing`, `csrf_token_mismatch`, or `invalid_form`. The response explains the cause and includes the same request ID (`X-Request-ID`) for correlating logs. Request bodies, query strings, cookies, credentials and CSRF tokens are not logged. If the page address differs from `AGBREDO_BASE_URL`, open the configured address; if a form belongs to an earlier login session, reload its form page.

## Connect existing Claude Code agents

Follow the [reporting integration setup](integrations/claude-code/README.md#minimal-workflow-change). Register the existing workflow in observation mode; leave its continuation workflow empty. Add GitHub OIDC permission alongside existing job permissions and wrap only its execution step. The Claude OAuth token stays in the existing Actions secret and runner environment. Do not add Claude credentials to the Go environment, database, workflow dispatch inputs or reporting payload.

The observation threshold flags estimated API consumption only. Agents continue running normally, and workflow completion comes from GitHub lifecycle events. No actions or approvals are introduced by this mode.

## Connect the optional managed example

Copy these files into the repository on which you installed the App:

```text
examples/github-agent/agent.py            -> examples/github-agent/agent.py
examples/github-agent/agbredo-agent.yml    -> .github/workflows/agbredo-agent.yml
examples/github-agent/agbredo-continue.yml -> .github/workflows/agbredo-continue.yml
```

Commit the files to the default branch. GitHub requires dispatchable workflows to be present on the default branch. Keep both workflow templates' `run-name` and workflow input names unchanged: Agbredo uses the correlation ID in that name to find GitHub executions because workflow dispatch returns no execution ID. The example needs only the GitHub runner's built-in Python 3. No model API key or permanent workflow secret is needed.

Sign in to Agbredo, open **Agents**, and register:

| Field | Example |
| --- | --- |
| Name | Issue proposal agent |
| Mode | Managed dispatch and approvals |
| GitHub owner / repository | Your actual repository owner and name |
| Initial workflow | `agbredo-agent.yml` |
| Continuation workflow | `agbredo-continue.yml` |
| Git reference | `main`, or your actual default branch |
| Installation ID | Numeric ID for this App's installation on the repository |
| Per-run budget | `1.00` USD |
| Enabled | Checked |

The workflows request `id-token: write` and `contents: read` only. A fresh OIDC token is requested for each Agbredo API call with `AGBREDO_BASE_URL` as its audience. Agbredo verifies the issuer, signature, expiry, audience, repository, workflow file, configured reference, and correlated GitHub execution before accepting the caller. Workflow inputs are passed as environment variables rather than injected into shell code.

This milestone supports direct workflow jobs in the registered repository. Do not move the agent job into an external reusable workflow: the OIDC verifier rejects reusable-workflow identities. Keep the example's jobs directly in the two registered workflow files.

Launch from the agent page. The run timeline should show its start and zero-cost issue proposal, then `waiting_approval`. The GitHub initial workflow finishes without creating an issue. Open the approval, inspect the exact title/body/repository, and approve or reject it. Rejection cancels the run and dispatches no continuation. Approval dispatches the separate continuation, which validates the stored approval hash and calls Agbredo. Agbredo rechecks administrator authorization, proposal identity, expiry, current agent configuration and spending before writing the issue through the App. The run then shows the result link and complete audit trail.

Changing an agent's configuration after a run is launched makes its old approval stale. Launch a new run rather than trying to execute an approval against changed configuration. The immutable run snapshot retains the historical configuration and agent version.

## Spending and failure recovery

Estimated consumption is stored as integer microdollars (`cost_micros`; 1 USD = 1,000,000), with separate reported token counts and optional cache counters. Stable event IDs prevent duplicate accounting on retries. Actual Claude subscription charges and remaining subscription quota are unknown. Observation mode displays an advisory threshold flag and never interrupts agents. In managed mode, exceeding the snapshot budget blocks subsequent controlled actions; this cooperative mechanism cannot cap independent model consumption. The deterministic managed example reports true zero usage.

Dispatch jobs are persisted alongside run creation or approval decisions, and background processing retries temporary dispatch failures. Webhooks plus polling reconcile delayed workflow states; an initial workflow ending successfully does not complete a run that is waiting for approval. A continuation that fails before claiming its action can be retried from the approval page, while its approval remains valid.

GitHub's issue-creation API has no idempotency key. Agbredo durably claims a single execution attempt before making that request and adds an approval-specific marker to the issue body. Concurrent execution calls and repeated approvals cannot trigger a second create request. If the GitHub response or final database update is lost, Agbredo searches for that marker to recover the result. Use the approval page's retry/reconcile action to repeat the lookup.

If no marker can be found after an ambiguous request, the approval remains visibly unresolved (`execution_unknown` or `executing`) and Agbredo **does not blindly create another issue**. Inspect the GitHub repository and Agbredo logs before deciding how to proceed. This preserves duplicate prevention at the cost of requiring investigation for an uncertain external outcome. Do not delete the issue marker until recovery has finished.

## Railway deployment

1. Push this repository to GitHub and create a Railway project from it. `railway.toml` selects the production Dockerfile and `/readyz` readiness check.
2. Add Railway's managed PostgreSQL service. Set `AGBREDO_DATABASE_URL` to `${{Postgres.DATABASE_URL}}` using the actual database service name in your project.
3. Generate a public HTTPS domain for the application and set `AGBREDO_BASE_URL` to its exact origin, without a trailing slash.
4. Set all App/OAuth/webhook credentials, `AGBREDO_GITHUB_PRIVATE_KEY` (multiline PEM), `AGBREDO_SESSION_KEY`, and `AGBREDO_ADMIN_USERS` as Railway variables. Omit the local private-key-file variable. `AGBREDO_APPROVAL_TTL` defaults to `24h`.
5. Update the GitHub App callback and webhook URLs to this domain; install the App on the example repository.
6. Deploy, sign in, register the example and exercise both an approval and a rejection with separate runs.

Railway's `PORT` overrides `AGBREDO_PORT` (default `8080`); `DATABASE_URL` is also accepted when `AGBREDO_DATABASE_URL` is unset. `AGBREDO_APPROVAL_TTL` accepts durations from `1m` to `168h`. The application listens on all interfaces, migrates the database on startup, logs JSON with `slog`, and shuts down gracefully on termination. The final Docker image runs as a non-root user and contains the compiled executable with embedded application assets; no Node pipeline is present.

`GET /healthz` checks application liveness. `GET /readyz` checks database readiness. PostgreSQL is the sole persistent store; retain it and its backups across deployments. Run one application replica for this first milestone.

## Tests and remaining live verification

```sh
go test ./...
python3 -m unittest discover -s examples/github-agent -p 'test_*.py'
```

The PostgreSQL integration suite creates a private schema for each test, applies the migrations there, and drops only that test's schema afterward. It never resets the database or its public schema. The test database role needs permission to create schemas and migration objects; a separate development test database keeps test activity isolated:

```sh
docker compose exec postgres createdb -U agbredo agbredo_test
AGBREDO_TEST_DATABASE_URL='postgres://agbredo:agbredo-local@localhost:5432/agbredo_test?sslmode=disable' go test ./...
```

Automated tests use external GitHub test doubles. Live GitHub Actions, App OAuth and Railway deployment require your credentials and repository configuration; no live integration success is claimed here. After completing the setup above, verify a launched run receives events, rejection creates no issue, approval creates one issue, repeated continuation/execution cannot create another, and the final run and approval audit contain the expected result link. Also confirm a failed continuation can be retried before execution and an expired approval is denied.

The primary routes are `/`, `/agents`, `/agents/{id}`, `/runs`, `/runs/{id}`, `/approvals`, and `/approvals/{id}`. Machine endpoints are `/api/agents/{id}/observations`, `/api/runs/{id}/events`, `/api/approvals/{id}`, `/api/approvals/{id}/execute`, and `/api/github/webhook`.
