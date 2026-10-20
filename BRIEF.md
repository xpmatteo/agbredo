
# Introduction

Agbredo is a lightweight dashboard meant to observe activity of Pipelines.  The Pipelines are implemented as Github actions that may start LLM agents.  The goals of Agbredo are:

 - see if the agents are running
 - see what they do
 - see how much they spend
 - see what actions they perform on the GH repo, eg opening a GH issue or PR
 - reuse GH auth/auth and UI as much as possible
 - be SIMPLE. Avoid complicated algorithms as much as possible

My (Matt's) personal goals is to learn how build such a dashboard, so that I can implement a similar one.

## Implementation:

 - Language: Go with server-rendered HTML
 - Persistence: PostgreSQL
 - Claude Code Telemetry saved to OpenObserve, authenticated with a OpenObserve token stored as a GH secret
 - Agbredo receives information from GH webhooks
 - Agbredo can query OpenObserve
 - Deployment: to Railway
 - The agents use Claude Code with my Pro subscription
 - The usage cost is the estimated API cost returned by the Claude telemetry
 - The public URL is https://www.agbredo.com
 - The dashboard is protected through GH authentication
 - An environment variable AGBREDO_USERS lists the GH login names who are allowed to see the dashboard
 - The integration with GH is through the Agbredo GH app
 - The transcript of a Claude session is saved as an artifact in the GH workflow run
 - How do we link telemetry to a run, and how do we authenticate it? The workflow sets OTEL_RESOURCE_ATTRIBUTES with GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT, step id and the job name.

## The first slice

Receive workflow_run webhooks, store them in Postgres, show a list of runs with their status and a link to GitHub.

Which runs: only Pipeline runs. A run is a Pipeline run when its `workflow_run` webhook has a non-empty `referenced_workflows`. Each entry gives a Pipeline ID (its `path`) and Version (its `ref`, or `sha` if there is no `ref`). The run stores all entries. Pipelines can come from any repository. We rely only on the reusable workflow, not on conventions in the caller workflow. Agbredo ignores all other runs.

Known gaps, accepted for now:
- CI that calls a reusable workflow (e.g. a shared lint workflow) shows up as a Pipeline run.
- When a run uses more than one reusable workflow (e.g. a Pipeline that calls another one), `referenced_workflows` has several entries and does not say which one is the top one. We store them all and show them all.

## Other information

This is the second attempt.  I deleted the previous implementation in this repo because it was getting too complicated, and I want to start fresh to see if we can build a simpler solution.

The only things worth keeping from the previous effort is the docs/00-glossary.md and Railway.toml and possibly .env.example

I plan to reuse the Railway deployment.  I will nuke the DB.  I will reuse the existing GH App.

## Lessons from attempt 1

The first attempt grew to about 10k lines of Go in 3 days. Where the complexity came from:

- It started as a bigger product (dispatch, approvals, gated issue creation). When that was dropped, its machinery stayed: OIDC checks, frozen copies of the deployment on each run, revisions, thresholds, a polling worker.
- Agbredo was its own telemetry server: OIDC token exchange, per-run tokens, OTLP parsing, counter dedup, forwarding to OpenObserve. About 2k lines.
- The same fact came from several sources (token request, webhook, poll), so we needed rules for which one wins.
- PR linking guessed links by time, with levels of certainty, reasons, findings and a retry worker.
- Every feature had a long list of attacker cases and limits.
- Backward compatibility was required while nobody used the app: old routes, old JSON keys, append-only migrations.
- Each review added guards for rare cases, because "fix now" was the easy answer.

So, this time:

- Not now: telemetry ingestion, OIDC, polling, frozen copies, thresholds, backward compatibility. Add them only when a real need shows up.
- One source per fact. Run status comes only from `workflow_run` webhooks. If a webhook is lost, we accept it.
- Trust model: HMAC signature on webhooks; GH login plus `AGBREDO_USERS` on the dashboard. Nothing more for now.
- Link a PR or Issue to a Run only through an explicit marker. No guessing. The marker is the last line of the PR or Issue body: `Agbredo-Run: <GITHUB_RUN_ID>/<GITHUB_RUN_ATTEMPT>`. Agbredo reads it from the `pull_request` or `issues` webhook.
- Pipeline rule: Agents write content (title, body, file changes); Deterministic Steps open PRs and Issues and add the marker. See `pipelines/actions/agent-instructions-audit/scripts/open-pr.sh` for an example.
- In reviews, the default answer is "new ticket" or "no action". "Fix now" is only for real bugs in the current slice.


## Open questions

- ~~How do we link a PR or Issue to a Run?~~ Answered: the `Agbredo-Run:` marker, see "Lessons from attempt 1".

- Deployment is now "Repository + Pipeline ID". The old one was "repository + caller workflow file". Can one repo use the same pipeline from two caller workflows? If so, those two become one Deployment.
- Version: "readable identifiers, eg v2". What do we show when a run uses a branch or a SHA?
