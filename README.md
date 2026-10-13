# Rilian — Agent Fleet Control Plane

Rilian is a single Go application with PostgreSQL and server-rendered HTML. It registers GitHub Actions agents, dispatches their workflows, persists events and spending, and gates a specific GitHub issue creation behind an administrator's approval.

The example uses deterministic Python, calls no model, and reports zero tokens and zero model cost. An initial workflow proposes an issue and exits. An approved continuation authenticates with GitHub OIDC, retrieves the immutable proposal, and asks Rilian to execute it through the GitHub App. Rilian records the result. Runners never receive an issue-writing credential, so they cannot bypass the approval by using that credential themselves.

## Local development

Requirements: Go 1.26.5 or newer, Docker Compose, and a GitHub App for real GitHub integration. The production Docker build uses the current Go 1.27 stable release family ([Go release notes](https://go.dev/doc/go1.27)). No frontend build is needed.

```sh
docker compose up -d
cp .env.example .env
# Edit .env: set GitHub App credentials, private key path, session key and allowlist.
set -a
. ./.env
set +a
go run ./cmd/rilian serve
```

Rilian reads environment variables; it does not load `.env` automatically. `serve` applies the embedded migrations on startup. For an explicit migration step or a built executable:

```sh
go build -o rilian ./cmd/rilian
./rilian migrate
./rilian serve
```

Open `http://localhost:8080`. For actual workflow events and webhooks, expose the application through a public HTTPS tunnel, set `RILIAN_BASE_URL` to that exact origin, and use it in the GitHub App settings. GitHub-hosted runners cannot reach your `localhost`.

The repository currently has no Git remote. Its module path is temporarily `rilian`. Once the actual repository URL exists, update `go.mod` and the internal `rilian/...` imports to that repository path; no fictional module domain is used.

## Configure the GitHub App

Create a GitHub App dedicated to Rilian. Set its homepage to your public Rilian URL, its user authorization callback to `https://YOUR-HOST/auth/callback`, and its webhook URL to `https://YOUR-HOST/api/github/webhook`. Set a random webhook secret and enable the **Workflow run** webhook event. Enable user authorization for dashboard login; device flow and user access-token expiration settings are not required by this implementation.

Request these repository permissions:

| Permission | Access | Purpose |
| --- | --- | --- |
| Actions | Read and write | Dispatch workflows, inspect executions and reconcile status |
| Contents | Read | Read workflow/repository information |
| Issues | Read and write | Create the approved example issue and find its recovery marker |
| Metadata | Read | Required GitHub App repository metadata access |

No organization or account permissions are needed. Install the App on only the intended agent repositories. Record the numeric installation ID from the installation settings URL (the number following `/installations/`). The installation ID is configured per agent; it is not a shared runner secret.

Set `RILIAN_GITHUB_APP_ID`, `RILIAN_GITHUB_CLIENT_ID`, `RILIAN_GITHUB_CLIENT_SECRET`, and `RILIAN_GITHUB_WEBHOOK_SECRET` (at least 32 random characters). Generate a private key and supply either `RILIAN_GITHUB_PRIVATE_KEY_FILE` locally or `RILIAN_GITHUB_PRIVATE_KEY` with the complete PEM and real newlines on Railway. Keep private keys and client secrets out of source control. App installation access tokens are short-lived and managed by `ghinstallation`.

Generate a session secret with `openssl rand -hex 32`, set `RILIAN_SESSION_KEY`, and list administrator GitHub login names in `RILIAN_ADMIN_USERS` separated by commas. Dashboard routes require GitHub OAuth login and an allowlisted user. Approval decisions also check authorization in the service layer. Browser mutations use CSRF tokens. GitHub webhooks require a verified signature, and runner API requests require short-lived GitHub OIDC credentials.

Dashboard sessions use signed, HTTP-only cookies and expire after eight hours. HTTPS deployments also mark the cookies secure. The administrator allowlist is checked on each authenticated request; changing it requires restarting the application. Rotating `RILIAN_SESSION_KEY` invalidates existing sessions.

## Connect the example agent

Copy these files into the repository on which you installed the App:

```text
examples/github-agent/agent.py            -> examples/github-agent/agent.py
examples/github-agent/rilian-agent.yml    -> .github/workflows/rilian-agent.yml
examples/github-agent/rilian-continue.yml -> .github/workflows/rilian-continue.yml
```

Commit the files to the default branch. GitHub requires dispatchable workflows to be present on the default branch. Keep both workflow templates' `run-name` and workflow input names unchanged: Rilian uses the correlation ID in that name to find GitHub executions because workflow dispatch returns no execution ID. The example needs only the GitHub runner's built-in Python 3. No model API key or permanent workflow secret is needed.

Sign in to Rilian, open **Agents**, and register:

| Field | Example |
| --- | --- |
| Name | Issue proposal agent |
| GitHub owner / repository | Your actual repository owner and name |
| Initial workflow | `rilian-agent.yml` |
| Continuation workflow | `rilian-continue.yml` |
| Git reference | `main`, or your actual default branch |
| Installation ID | Numeric ID for this App's installation on the repository |
| Per-run budget | `1.00` USD |
| Enabled | Checked |

The workflows request `id-token: write` and `contents: read` only. A fresh OIDC token is requested for each Rilian API call with `RILIAN_BASE_URL` as its audience. Rilian verifies the issuer, signature, expiry, audience, repository, workflow file, configured reference, and correlated GitHub execution before accepting the caller. Workflow inputs are passed as environment variables rather than injected into shell code.

This milestone supports direct workflow jobs in the registered repository. Do not move the agent job into an external reusable workflow: the OIDC verifier rejects reusable-workflow identities. Keep the example's jobs directly in the two registered workflow files.

Launch from the agent page. The run timeline should show its start and zero-cost issue proposal, then `waiting_approval`. The GitHub initial workflow finishes without creating an issue. Open the approval, inspect the exact title/body/repository, and approve or reject it. Rejection cancels the run and dispatches no continuation. Approval dispatches the separate continuation, which validates the stored approval hash and calls Rilian. Rilian rechecks administrator authorization, proposal identity, expiry, current agent configuration and spending before writing the issue through the App. The run then shows the result link and complete audit trail.

Changing an agent's configuration after a run is launched makes its old approval stale. Launch a new run rather than trying to execute an approval against changed configuration. The immutable run snapshot retains the historical configuration and agent version.

## Spending and failure recovery

Costs are integer microdollar deltas (`cost_micros`; 1 USD = 1,000,000), with separate reported input/output token deltas. Stable event IDs prevent duplicate spending when events are retried. Spending greater than the snapshot budget marks the run over budget and blocks approval/execution of subsequent controlled actions. This is a cooperative mechanism; Rilian cannot cap independent model API spending. The example truthfully reports all usage as zero.

Dispatch jobs are persisted alongside run creation or approval decisions, and background processing retries temporary dispatch failures. Webhooks plus polling reconcile delayed workflow states; an initial workflow ending successfully does not complete a run that is waiting for approval. A continuation that fails before claiming its action can be retried from the approval page, while its approval remains valid.

GitHub's issue-creation API has no idempotency key. Rilian durably claims a single execution attempt before making that request and adds an approval-specific marker to the issue body. Concurrent execution calls and repeated approvals cannot trigger a second create request. If the GitHub response or final database update is lost, Rilian searches for that marker to recover the result. Use the approval page's retry/reconcile action to repeat the lookup.

If no marker can be found after an ambiguous request, the approval remains visibly unresolved (`execution_unknown` or `executing`) and Rilian **does not blindly create another issue**. Inspect the GitHub repository and Rilian logs before deciding how to proceed. This preserves duplicate prevention at the cost of requiring investigation for an uncertain external outcome. Do not delete the issue marker until recovery has finished.

## Railway deployment

1. Push this repository to GitHub and create a Railway project from it. `railway.toml` selects the production Dockerfile and `/readyz` readiness check.
2. Add Railway's managed PostgreSQL service. Set `RILIAN_DATABASE_URL` to `${{Postgres.DATABASE_URL}}` using the actual database service name in your project.
3. Generate a public HTTPS domain for the application and set `RILIAN_BASE_URL` to its exact origin, without a trailing slash.
4. Set all App/OAuth/webhook credentials, `RILIAN_GITHUB_PRIVATE_KEY` (multiline PEM), `RILIAN_SESSION_KEY`, and `RILIAN_ADMIN_USERS` as Railway variables. Omit the local private-key-file variable. `RILIAN_APPROVAL_TTL` defaults to `24h`.
5. Update the GitHub App callback and webhook URLs to this domain; install the App on the example repository.
6. Deploy, sign in, register the example and exercise both an approval and a rejection with separate runs.

Railway's `PORT` overrides `RILIAN_PORT` (default `8080`); `DATABASE_URL` is also accepted when `RILIAN_DATABASE_URL` is unset. `RILIAN_APPROVAL_TTL` accepts durations from `1m` to `168h`. The application listens on all interfaces, migrates the database on startup, logs JSON with `slog`, and shuts down gracefully on termination. The final Docker image runs as a non-root user and contains the compiled executable with embedded application assets; no Node pipeline is present.

`GET /healthz` checks application liveness. `GET /readyz` checks database readiness. PostgreSQL is the sole persistent store; retain it and its backups across deployments. Run one application replica for this first milestone.

## Tests and remaining live verification

```sh
go test ./...
python3 -m unittest discover -s examples/github-agent -p 'test_*.py'
```

The PostgreSQL integration suite creates a private schema for each test, applies the migrations there, and drops only that test's schema afterward. It never resets the database or its public schema. The test database role needs permission to create schemas and migration objects; a separate development test database keeps test activity isolated:

```sh
docker compose exec postgres createdb -U rilian rilian_test
RILIAN_TEST_DATABASE_URL='postgres://rilian:rilian-local@localhost:5432/rilian_test?sslmode=disable' go test ./...
```

Automated tests use external GitHub test doubles. Live GitHub Actions, App OAuth and Railway deployment require your credentials and repository configuration; no live integration success is claimed here. After completing the setup above, verify a launched run receives events, rejection creates no issue, approval creates one issue, repeated continuation/execution cannot create another, and the final run and approval audit contain the expected result link. Also confirm a failed continuation can be retried before execution and an expired approval is denied.

The primary routes are `/`, `/agents`, `/agents/{id}`, `/runs`, `/runs/{id}`, `/approvals`, and `/approvals/{id}`. Machine endpoints are `/api/runs/{id}/events`, `/api/approvals/{id}`, `/api/approvals/{id}/execute`, and `/api/github/webhook`.
