# Agbredo

Go server and PostgreSQL. It receives reports from GitHub Actions agents and shows them on a dashboard. See `docs/design.md`.

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

## Where things live

- `cmd/agbredo`: the server entry point.
- `internal/`: server code. Run `ls internal` for the packages. `core` holds the domain logic, `store` the database, `web` the dashboard, `webapi` the report API, `auth` login and OIDC, `github` the GitHub client.
- `migrations/`: database migrations.
- `integrations/`: pieces that run inside an agent's workflow.
- `examples/`: sample agents.
- `docs/`: `design.md` for the design, `routes.md` for the routes, `glossary.md` for the domain words.

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
