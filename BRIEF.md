

Agbredo is a lightweight dashboard meant to observe activity of Pipelines.  The Pipelines are implemented as Github actions that may start LLM agents.  The goals of Agbredo are:

 - see if the agents are running
 - see what they do
 - see how much they spend
 - see when an LLM agent is requesting human intervention.  One way they might do that is through opening a GH issue or PR
 - reuse GH auth/auth and UI as much as possible
 - be SIMPLE. Avoid complicated algorithms as much as possible

My (Matt's) personal goals is to learn how build such a dashboard, so that I can implement a similar one 

Implementation:

 - Language: Go with server-rendered HTML
 - Persistence: PostgreSQL
 - Telemetry collected through OpenObserve
 - Deployment: to Railway
 - The agents use Claude Code with my Pro subscription
 

