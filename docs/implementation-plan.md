# First milestone

Current integration: existing Claude Code workflows run independently using their existing subscription OAuth secret. A runner-local composite action collects OpenTelemetry and sends typed observations using GitHub OIDC. New agents default to observation mode; Go never runs Claude or stores its credentials. Estimates of API-equivalent consumption are separate from unknown subscription charges. See `integrations/claude-code/README.md` for setup, verification evidence and limitations. The original managed approval milestone below remains optional.

1. Define a PostgreSQL schema, immutable run snapshots, event IDs and approval payloads.
2. Add GitHub App dispatch, OAuth dashboard login, and GitHub OIDC runner verification.
3. Persist run events transactionally; reconcile webhook and polling lifecycle signals.
4. Persist approval decisions and continuation dispatch jobs together. Mediate issue creation through a single durable execution attempt, with marker-based recovery for ambiguous GitHub responses.
5. Render the registry, run timelines, approval queue and fleet dashboard with Go templates.
6. Add deterministic example workflows, database integration tests, Docker Compose and Railway deployment documentation.

The repository has no remote URL. The module is temporarily `agbredo`; change it and internal imports once the actual repository URL is configured.

GitHub does not offer an idempotency key for issue creation. An ambiguous create response must never cause a blind second create attempt. Recovery searches for the approval's unique issue marker; unresolved attempts stay visible for investigation.
