# Glossary

The words we use for the domain. Code, database and dashboard should use the same words with the same meaning. Where the code does not match yet, the "Code today" column says so.

## Core terms

| Term | Meaning | Code today |
| --- | --- | --- |
| Pipeline | One implementation, kept in a central repository as a reusable workflow. Identified by its path, e.g. `org/agbredo-agents/.github/workflows/triage.yml`. | Not in code. |
| Version | A pipeline's version: the git ref of the central repository it ran from (tag or SHA). GitHub supplies it in the OIDC `job_workflow_ref` claim, so the pipeline cannot fake it. | Not stored. `core.Deployment.Revision` is an edit counter on the registration, used to detect edit conflicts. It is not this. |
| Deployment | A pipeline used in one repository, through one caller workflow. Identified by repository + workflow file. One caller workflow calls exactly one pipeline. Created by the first valid run; no registration by hand. | `core.Deployment` and the `agents` table. Today they also hold the ref, are registered by hand, and have no pipeline. |
| Run | One GitHub run attempt of a deployment, with its exact version and configuration. | `core.Run`, `runs` table. One per attempt. |
| Agent | A named role inside a pipeline, e.g. `triage` or `reviewer`. A step in the pipeline's YAML starts it. | Not in code. `core.Agent` no longer exists; the old type is now `core.Deployment`. |
| Agent run | One execution of an agent inside a run: one Claude Code invocation. A run has zero or more agent runs. | "Source": the `source` input, `Run.Sources`, `run_tokens.source`. |
| Observation | Anything that arrives about a run: reporter reports, telemetry, GitHub webhooks and polls. | Split into `core.Report`, `core.Event` and telemetry. The report URL already says `observations`. |

## More terms

| Term | Meaning | Code today |
| --- | --- | --- |
| Attempt | GitHub's word. "Re-run jobs" keeps the GitHub run ID and adds 1 to `run_attempt`. Each attempt is its own Run. The dashboard may group attempts that share a GitHub run ID, but that group has no domain name. | `Run.RunAttempt`, `Run.GitHubRunID`. |
| Configuration | What a run ran with: trigger event, ref, threshold at the time, and the Claude Code version, model and effort. | Threshold is copied into the run. Claude Code version is stored. Trigger event, model and effort are not. |
| Threshold | Estimated cost per run above which a run is flagged. It does not stop anything. | `BudgetMicros`, `OverBudget`. |
| Estimated cost | Cost of the tokens at API prices. Not the subscription charge. | `CostMicros`. |
| Escalation | An agent run hands work to a person, usually by opening a GitHub issue or PR. | Not in code. |
| Central repository | A repository that holds pipelines. | Not in code. |
| Caller workflow | The small workflow file in the target repository. It holds the triggers and permissions and calls one pipeline. | The workflow file of a registered `core.Deployment`. |
| Reporter | The code in a pipeline that sends reports. | `integrations/claude-code`. |
| Telemetry | Claude Code's own OpenTelemetry export, sent to Agbredo with a telemetry token, one per agent run. | `run_tokens`, `otlp_*` tables. |
| Actor | GitHub login that started the GitHub run. | `Run.Actor`. |
| Triggering actor | GitHub login that started this attempt. | `Run.TriggeringActor`. |

## Not part of the model

- Claude Code subagents. They run inside one agent run. We see them only through telemetry.
- Activation. It meant the same as Run. Use Run.
