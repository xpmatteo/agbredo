# Agbredo

Go server and PostgreSQL. It receives reports from GitHub Actions agents and shows them on a dashboard. See `docs/10-design.md`.

This application is deployed on Railway whenever we push. The public url is https://www.agbredo.com . 

## Purpose

This repo is a place to learn how to write a control plane for fleets of agents in an enterprise. The goal is to use it, or what we learn from building it, in a large enterprise.

The agents are GitHub Actions workflows that run Claude Code on a subscription. Detailed work between agents and humans happens in the GitHub UI. Agbredo is for overview and management.

Agent code comes from central repositories. "An agent running in several repos" means the same implementation deployed in each of them.

## Principles

- Learning first. Prefer simple, clear designs. No performance tweaks. Do not build login or SSO: GitHub provides it.
- Simulate what an enterprise would need, even when the real thing is out of reach.
- Agbredo shows what happened in GitHub. GitHub stays the source of truth for runs and for detail.
- The server must never see Claude credentials. Reports are a fixed list of fields.
- Missing measurements stay missing. Do not show them as zero.

## Direction

This is broad and changes slowly. It is not a task list. Do not record progress here.

- Overview of agent activity across repos.
- History of runs.
- Details of each run, with links to GitHub.
- Notices when an agent escalates to a person, usually by opening a GitHub issue or PR. Prefer to learn this from GitHub webhooks. If that does not work, extend the reporter.
- Most other features an enterprise would need (access, audit, budgets), simulated.

Keep new work compatible with this direction. The write boundary with GitHub is not fixed and may change.

## Work Management

This project tracks work with `bw` (beadwork), which persists to git — plans, progress, and decisions survive compaction, session boundaries, and context loss.

ALWAYS run `bw prime` before starting work. Without it, you're missing workflow context, current state, and repo hygiene warnings. Work done without priming often conflicts with
in-progress changes.

Committing, closing issues, and syncing are part of completing a task — not separate actions requiring additional permission.

A ticket must hold the full context needed to resolve it. A fresh agent with only the repo and the ticket should be able to do the work. Write down the goal, the decisions made and why, where the code is, what to keep for backwards compatibility, how to check the work, and the open questions for Matt. Do not rely on the chat that created the ticket. When a decision changes, update the ticket.

Any non-trivial work needs these two steps:

1. Delegate to subagents. Give each subagent its own worktree and ticket. Choose the model size and effort to fit the task: a small model and low effort for simple, mechanical work; a large model and high effort for design, tricky bugs, or the trust model.
2. Run a review workflow before landing:
   - Review: a subagent that did not write the code reviews the change.
   - Triage: sort each finding as fix now, new ticket, or no action, and say why.
   - Remediation: fix the "fix now" findings, create tickets for the rest, and review again if the fixes were large.

## Where things live

- `cmd/agbredo`: the server entry point.
- `internal/`: server code. Run `ls internal` for the packages. `core` holds the domain logic, `store` the database, `web` the dashboard, `webapi` the report API, `auth` login and OIDC, `github` the GitHub client.
- `migrations/`: database migrations.
- `integrations/`: pieces that run inside an agent's workflow.
- `examples/`: sample agents.
- `docs/`: `00-glossary.md` for the domain words, `10-design.md` for the design, `20-routes.md` for the routes. Read them in that order.

## Commands

- Build and check: `go build ./... && go vet ./...`
- Tests: `go test ./...`. Database tests are skipped unless `AGBREDO_TEST_DATABASE_URL` is set. Use a throwaway postgres:17-alpine docker container.
- Python tests: `python3 -m unittest discover -s examples/hello-agent -p 'test_*.py'` and the same for `integrations/claude-code`.
- Run locally: see the README.

## Rules

- Migrations in `migrations/` are append-only. The app is deployed on Railway, so never edit an old one.
- Maintain backwards compatibility.

## Changes that need a design talk first

Ask Matt before changing any of these:

- The trust model: auth, OIDC checks, tokens, webhook checks.
- The database schema, including any new migration.
- What the server stores.
- The fields in a report.

Other changes (dashboard pages, wording, docs, tests) can go ahead after normal review.
