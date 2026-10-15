# Routes

Every route the server exposes. The code is in `cmd/agbredo/main.go`, `internal/webapi/api.go`, `internal/web/server.go` and `internal/auth/auth.go`. See `10-design.md` for how the parts fit together.

All responses carry an `X-Request-ID` header. Errors from the runner routes are plain text.

## Who calls what

| Caller | Credential | Routes |
| --- | --- | --- |
| A person in a browser | Signed session cookie, GitHub user on the allowlist | Dashboard, `/auth/*` |
| A workflow's job | GitHub OIDC token, audience = `AGBREDO_BASE_URL` | `/api/otlp-token`, `/api/agents/{id}/otlp-token` |
| Claude Code in a job | Telemetry token from an `otlp-token` route | `/v1/metrics`, `/v1/logs`, `/v1/traces` |
| GitHub | HMAC signature of the webhook secret | `/api/github/webhook` |
| The platform | None | `/healthz`, `/readyz` |

## Runner routes

These are kept stable. Deployed workflows call them. Do not rename them.

### `POST /api/otlp-token`

Gets a token for one Claude Code step to send its telemetry. `Authorization: Bearer <OIDC token>`. Creates the run if it is new, and the deployment if it is new.

- The server checks the token (issuer, signature, expiry, audience). The deployment is the one with the token's repository and workflow file; owner and repository ignore case. If there is none, the server asks GitHub, as the App, for the App's installation on the repository, and creates the deployment (see `10-design.md`, Flow). Then it checks that GitHub has that run attempt. The ref is not checked; it is recorded on the run.
- Body and `200` reply: as for `POST /api/agents/{id}/otlp-token` below.
- Errors: `401` missing or bad token, `400` bad body or `source`, or a workflow file name Agbredo cannot keep, `403` the GitHub App is not installed on the repository, the deployment is disabled, or the job does not match GitHub's run, `404` the deployment was deleted while the request was handled (rare; the next request creates a new one), `409` the run has already finished, `500` GitHub or the database failed, for example GitHub did not answer the installation lookup. A refused or failed request creates nothing.

### `POST /api/agents/{id}/otlp-token`

The same, for a named deployment. Workflows written before `POST /api/otlp-token` call it with their `agent-id`. It works as before and never creates a deployment. `Authorization: Bearer <OIDC token>`. Creates the run if it is new.

- The server checks the token (issuer, signature, expiry, audience), that the repository and workflow file match the deployment (owner and repository ignore case), and that GitHub has that run attempt. The ref is not checked; it is recorded on the run.
- Body: `{"source": "review"}`, up to 64 KiB. Unknown fields are refused. The `source` is the name of the agent run. It is 1 to 80 letters, digits, dots, underscores or hyphens. Use a different name for each agent run.
- `200` with `{"token": "agb_...", "expires_at": "...", "traceparent": "00-...-...-01"}`. The `traceparent` has the same trace ID for every step of a run attempt. The token lasts 6 hours, or until the run finishes. It belongs to one run attempt and one agent run. Only its hash is stored.
- Errors: `401` missing or bad token, `400` bad body or `source`, `403` the job does not match the deployment, or the deployment is disabled, `404` unknown deployment, `409` the run has already finished.

### `POST /api/agents/{id}/observations`

The former report route. It is gone: it answers `410 Gone` with a plain-text note to use `integrations/claude-code-otlp`, before any check of the token or body.

### `POST /v1/metrics`, `POST /v1/logs` and `POST /v1/traces`

OpenTelemetry from Claude Code. `Authorization: Bearer agb_...`, the token from `/otlp-token`. Claude Code adds these paths to `OTEL_EXPORTER_OTLP_ENDPOINT` itself.

- `Content-Type: application/json` (set `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`). Protobuf is not accepted. `Content-Encoding: gzip` is accepted. Up to 4 MiB, after decompression.
- The token says which run and which agent run the data belongs to. Nothing in the payload is trusted for that.
- Metrics: the server reads `claude_code.token.usage` and `claude_code.cost.usage`, delta or cumulative, with their `model` and `effort` attributes. Logs: it counts `api_error` events, and rate-limit errors among them. Traces: it reads only the `effort` attribute of `claude_code.llm_request` spans. A repeated export is counted once.
- If `OTLP_FORWARD_URL` is set, the accepted payload is forwarded to the telemetry store with the run's labels added (see `10-design.md`). Otherwise the rest is dropped.
- `200` with `{}`.
- Errors: `401` no token, or not an `agb_` token, `415` not JSON, `400` bad gzip or JSON, `413` too large, `403` unknown or expired token, or the run has finished.

## GitHub

### `POST /api/github/webhook`

GitHub App webhooks. Needs `X-Hub-Signature-256` (HMAC of the body with `AGBREDO_GITHUB_WEBHOOK_SECRET`) and `X-GitHub-Delivery`. Body up to 1 MiB.

- Every delivery with a good signature is archived raw in the telemetry store, if `OTLP_FORWARD_URL` is set, and indexed in `webhook_deliveries` as `handled`, `ignored` or `failed` (see `10-design.md`, Webhook flow). A delivery refused with `401`, `413`, or `400` for its headers (missing or long delivery ID, event name over 100 characters) is neither.
- Only `workflow_run` and `pull_request` events change anything. For `workflow_run`, the server reads the run from the signed payload, without asking GitHub, and records it under the live deployment of its repository and workflow file, creating the run if it is new, even when the deployment is disabled. A run of a workflow with no deployment is ignored. The payload must have the shape of a `workflow_run` event (see `10-design.md`, Runs from webhooks), or it gets `400`.
- For `pull_request` with action `opened`, the server records the pull request if its repository has a live deployment and its author is a bot or its body has an `Agbredo-Run:` trailer, and links it to a run (see `10-design.md`, Pull requests). The payload must have the shape of a `pull_request` event, or it gets `400`. Other actions (`edited`, `closed`, `reopened`, ...) are ignored before the shape is checked: the pull request is recorded as it was at its opening.
- `ping` and other events get `204`.
- A handled delivery ID is applied once. A failed one is processed again when GitHub sends it again.
- `204` on success. Errors: `401` bad signature, `400` missing delivery ID, event name over 100 characters, or bad `workflow_run` or `pull_request` payload, `413` too large, `500` the database failed.

## Dashboard

For people. A browser session is needed: GitHub OAuth, then a signed cookie. A user who is not on `AGBREDO_ADMIN_USERS` gets nowhere. An unauthenticated `GET` is sent to `/auth/login`. Other methods get `401`. Form posts need the CSRF token and a matching origin.

| Route | Does |
| --- | --- |
| `GET /` | Overview: active runs, failures, estimated consumption, recent runs with their outcome (see `GET /runs`) |
| `GET /agents` | List deployments, and the form to add one by hand |
| `POST /agents` | Add a deployment by hand, before its first run. Optional: the first run creates it anyway. `400` if its repository + workflow file already has one |
| `GET /agents/{id}` | One deployment: the pipeline versions used by its most recent runs (up to 20), the unlinked pull requests of its repository (up to 20, newest first, with reason, candidate runs and findings), and its form |
| `POST /agents/{id}` | Save a deployment. Refused if the revision is old, or with `400` if another deployment has its repository + workflow file |
| `POST /agents/{id}/delete` | Hide a deployment. Its runs stay |
| `GET /runs` | All runs. The Outcome column says "Opened PR #13" for each pull request linked to the run (labelled "inferred" when it is), "No PR linked" when the run has finished with none and Agbredo was recording pull requests for the whole run, and "Unavailable" otherwise |
| `GET /runs/{id}` | One run: totals, trigger event, ref, head branch and commit, GitHub start and completion, pipeline and version, agent runs (Claude Code version, models, effort, usage) or "No telemetry received", pull requests linked to the run (number with its GitHub link, title, author, when opened, link level and trailer, findings; not what happened to them later), jobs and steps (one table per job: name with its GitHub link, conclusion, start and end; its steps in GitHub's order with their numbers as given), event timeline |
| `GET /static/style.css` | The stylesheet |

## Sign-in

| Route | Does |
| --- | --- |
| `GET /auth/login` | Starts the GitHub OAuth sign-in |
| `GET /auth/callback` | GitHub returns here. Sets the session cookie (8 hours) |
| `POST /auth/logout` | Ends the session. Needs a session and the CSRF token |

## Health

| Route | Does |
| --- | --- |
| `GET /healthz` | `200 ok` while the process runs |
| `GET /readyz` | `200 ok` if the database answers within 2 seconds, otherwise `503` |
