
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

## Other information

This is the second attempt.  I deleted the previous implementation in this repo because it was getting too complicated, and I want to start fresh to see if we can build a simpler solution.

The only things worth keeping from the previous effort is the docs/00-glossary.md and Railway.toml and possibly .env.example

I plan to reuse the Railway deployment.  I will nuke the DB.  I will reuse the existing GH App.


## Open questions

- How do we link a PR or Issue to a Run? 

- Deployment is now "Repository + Pipeline ID". The old one was "repository + caller workflow file". Can one repo use the same pipeline from two caller workflows? If so, those two become one Deployment.
- Version: "readable identifiers, eg v2". What do we show when a run uses a branch or a SHA?
