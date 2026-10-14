# Glossary

The words we use for the domain. Code, database and dashboard should use the same words with the same meaning. When they do not, the "Code today" column says so.

Status: draft. Under discussion.

## Agreed terms

| Term | Meaning | Code today |
| --- | --- | --- |
| Agent | One implementation, identified by its supplied name. | No such thing yet. `core.Agent` and the `agents` table hold a Deployment. |
| Version | That agent's supplied implementation version. | Not stored. `core.Agent.Version` is an edit counter on the registration, used to detect edit conflicts. The Claude Code version is also stored per run. |
| Deployment | The agent in a particular repository/workflow. | `core.Agent`, `agents` table: owner, repository, workflow file, ref, App installation, threshold. |
| Run | One GitHub run attempt, with its exact version and configuration. | `core.Run`, `runs` table: one per run attempt. Keeps a copy of the registration as it was. |

## Candidate terms

Not agreed yet. Listed so we can discuss them.

| Term | Possible meaning | Code today |
| --- | --- | --- |
| Source | One Claude Code step inside a run, named in the workflow. | `source` in reports and telemetry tokens; `Run.Sources`. |
| Report | A fixed list of fields the reporter sends at the start and end of a source. | `core.Report`; the URL still says `observations`. |
| Event | One entry in a run's history: a report or a status change. | `core.Event`, `events` table. |
| Telemetry | Claude Code's own OpenTelemetry export, sent to Agbredo with a telemetry token. | `run_tokens`, `otlp_*` tables. |
| Reporter | The code in the workflow that sends reports. | `integrations/claude-code`. |
| Estimated cost | Cost of the tokens at API prices. Not the subscription charge. | `CostMicros`. |
| Threshold | Estimated cost per run above which a run is flagged. | `BudgetMicros`, `OverBudget`. |
| Owner | The GitHub owner (user or org) of the deployment's repository. | `core.Agent.Owner`. |
| Actor | GitHub login that started the run. | `Run.Actor`. |
| Triggering actor | GitHub login that started this attempt. | `Run.TriggeringActor`. |
| Fleet | All agents and their deployments. | `core.Fleet`. |
| Activation | Used in CLAUDE.md ("history of activations"). Same as Run? | Not in code. |
| Escalation | An agent hands work to a person, usually by opening an issue or PR. | Not in code. |
| Central repository | Repository that holds agent implementations. | Not in code. |
| Caller workflow | The small workflow in the target repository that calls the agent's reusable workflow. | Not in code. |
