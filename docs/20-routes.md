# Routes

Every route the server exposes. The code is in `cmd/agbredo/main.go`, `internal/webapi/api.go`, `internal/web/server.go` and `internal/auth/auth.go`. See `10-design.md` for how the parts fit together.

All responses carry an `X-Request-ID` header. Errors from the runner routes are plain text.

## Who calls what

| Caller | Credential | Routes |
| --- | --- | --- |
| A person in a browser | Signed session cookie, GitHub user on the allowlist | Dashboard, `/auth/*` |
| A workflow's job | GitHub OIDC token, audience = `AGBREDO_BASE_URL` | `/otlp-token` |
| Claude Code in a job | Telemetry token from `/otlp-token` | `/v1/metrics`, `/v1/logs`, `/v1/traces` |
| GitHub | HMAC signature of the webhook secret | `/api/github/webhook` |
| The platform | None | `/healthz`, `/readyz` |

## Runner routes

These are kept stable. Deployed workflows call them. Do not rename them.

### `POST /api/agents/{id}/otlp-token`

Gets a token for one Claude Code step to send its telemetry. `Authorization: Bearer <OIDC token>`. Creates the run if it is new.

- The server checks the token (issuer, signature, expiry, audience), that the repository and workflow file match the deployment, and that GitHub has that run attempt. The ref is not checked; it is recorded on the run.
- Body: `{"source": "review"}`, up to 64 KiB. Unknown fields are refused. The `source` is the name of the agent run. It is 1 to 80 letters, digits, dots, underscores or hyphens. Use a different name for each agent run.
- `200` with `{"token": "agb_...", "expires_at": "...", "traceparent": "00-...-...-01"}`. The `traceparent` has the same trace ID for every step of a run attempt. The token lasts 6 hours, or until the run finishes. It belongs to one run attempt and one agent run. Only its hash is stored.
- Errors: `401` missing or bad token, `400` bad body or `source`, `403` the job does not match the deployment, `404` unknown deployment, `409` the run has already finished.

### `POST /api/agents/{id}/observations`

The former report route. It is gone: it answers `410 Gone` with a plain-text note to use `integrations/claude-code-otlp`, before any check of the token or body.

### `POST /v1/metrics`, `POST /v1/logs` and `POST /v1/traces`

OpenTelemetry from Claude Code. `Authorization: Bearer agb_...`, the token from `/otlp-token`. Claude Code adds these paths to `OTEL_EXPORTER_OTLP_ENDPOINT` itself.

- `Content-Type: application/json` (set `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`). Protobuf is not accepted. `Content-Encoding: gzip` is accepted. Up to 4 MiB, after decompression.
- The token says which run and which agent run the data belongs to. Nothing in the payload is trusted for that.
- Metrics: the server reads `claude_code.token.usage` and `claude_code.cost.usage`, delta or cumulative. Logs: it counts `api_error` events, and rate-limit errors among them. Traces: nothing is counted. A repeated export is counted once.
- If `OTLP_FORWARD_URL` is set, the accepted payload is forwarded to the telemetry store with the run's labels added (see `10-design.md`). Otherwise the rest is dropped.
- `200` with `{}`.
- Errors: `401` no token, or not an `agb_` token, `415` not JSON, `400` bad gzip or JSON, `413` too large, `403` unknown or expired token, or the run has finished.

## GitHub

### `POST /api/github/webhook`

GitHub App webhooks. Needs `X-Hub-Signature-256` (HMAC of the body with `AGBREDO_GITHUB_WEBHOOK_SECRET`) and `X-GitHub-Delivery`. Body up to 1 MiB.

- Only `workflow_run` events do anything. The server reads the run's state from GitHub and updates a run it already knows. `ping` and other events get `204`.
- A delivery ID is handled once.
- `204` on success. Errors: `401` bad signature, `400` missing delivery ID or bad payload, `413` too large.

## Dashboard

For people. A browser session is needed: GitHub OAuth, then a signed cookie. A user who is not on `AGBREDO_ADMIN_USERS` gets nowhere. An unauthenticated `GET` is sent to `/auth/login`. Other methods get `401`. Form posts need the CSRF token and a matching origin.

| Route | Does |
| --- | --- |
| `GET /` | Overview: active runs, failures, estimated consumption, recent runs |
| `GET /agents` | List deployments, and the form to register one |
| `POST /agents` | Register a deployment. `400` if its repository + workflow file is already registered |
| `GET /agents/{id}` | One deployment, and its form |
| `POST /agents/{id}` | Save a deployment. Refused if the revision is old, or with `400` if another deployment has its repository + workflow file |
| `POST /agents/{id}/delete` | Hide a deployment. Its runs stay |
| `GET /runs` | All runs |
| `GET /runs/{id}` | One run: totals, ref, Claude Code steps, event timeline |
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
