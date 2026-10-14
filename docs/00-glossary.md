# Glossary

The words we use for the domain. Code, database and dashboard should use the same words with the same meaning. Where the code does not match yet, the "Code today" column says so.

## Core terms

| Term | Meaning | Code today |
| --- | --- | --- |
| Pipeline | One implementation, kept in a central repository as a reusable workflow. Identified by its path, e.g. `org/agbredo-agents/.github/workflows/triage.yml`. | `core.Pipeline.Path`, kept on each run as `Run.Pipeline`. It comes from the OIDC `job_workflow_ref` claim when the run is created. A run whose caller workflow calls no reusable workflow has no pipeline (`job_workflow_ref` is then the caller itself), and neither do runs from before it was recorded. |
| Version | A pipeline's version: the git ref of the central repository it ran from (tag or branch), and the commit SHA. GitHub supplies them in the OIDC `job_workflow_ref` and `job_workflow_sha` claims, so the pipeline cannot fake them. | `Run.Pipeline.Ref` and `Run.Pipeline.SHA`, stored with the pipeline. `core.Deployment.Revision` is an edit counter on the deployment, used to detect edit conflicts. It is not this. |
| Deployment | A pipeline used in one repository, through one caller workflow. Identified by repository + workflow file. One caller workflow calls exactly one pipeline. Created by the first valid run; no registration by hand. | `core.Deployment` and the `agents` table, unique on repository + workflow file (owner and repository ignore case). The first valid telemetry-token request of a new repository + workflow file creates it; a person may also add one by hand on the dashboard. The deployment does not record a pipeline; each of its runs records the one it used. Older rows still hold a `ref`, which is no longer checked. |
| Run | One GitHub run attempt of a deployment, with its exact version and configuration. | `core.Run`, `runs` table. One per attempt. |
| Agent | A named role inside a pipeline, e.g. `triage` or `reviewer`. A step in the pipeline's YAML starts it. | Not in code. |
| Agent run | One execution of an agent inside a run: one Claude Code invocation. A run has zero or more agent runs. | `Run.AgentRuns`, `AgentRunUsage`. The wire names stay "source" and "sources": the `source` input, the `source` and `sources` JSON keys, `run_tokens.source`, the `agbredo.source` label. |
| Observation | Anything that arrives about a run: telemetry, GitHub webhooks and polls. | No single type. Telemetry adds to `Run` totals and `Run.AgentRuns`. Webhooks and polls set `Run.Status` and add a `core.Event`. |
| Event | One entry in a run's history. Today, a GitHub status change of the run. | `core.Event`, `events` table. Older rows of type `claude.execution` came from the former reporter. |

## More terms

| Term | Meaning | Code today |
| --- | --- | --- |
| Attempt | GitHub's word. "Re-run jobs" keeps the GitHub run ID and adds 1 to `run_attempt`. Each attempt is its own Run. The dashboard may group attempts that share a GitHub run ID, but that group has no domain name. | `Run.RunAttempt`, `Run.GitHubRunID`. |
| Configuration | What a run ran with: trigger event, ref, threshold at the time, and the Claude Code version, model and effort. | Trigger event (`Run.TriggerEvent`) and ref (`Run.Ref`) come from the OIDC identity, and the threshold from the deployment; all three are copied into the run when it is created. Claude Code version, models (`AgentRunUsage.Models`, with tokens per model) and effort levels (`AgentRunUsage.Efforts`) are kept per agent run, from telemetry. Each is missing when nothing reported it. |
| Threshold | Estimated cost per run above which a run is flagged. It does not stop anything. | `ThresholdMicros`, `OverThreshold`. |
| Estimated cost | Cost of the tokens at API prices. Not the subscription charge. | `CostMicros`. |
| Escalation | An agent run hands work to a person, usually by opening a GitHub issue or PR. | Not in code. |
| Central repository | A repository that holds pipelines. | Not in code, beyond the start of `Pipeline.Path`. |
| Caller workflow | The small workflow file in the target repository. It holds the triggers and permissions and calls one pipeline. | The workflow file of a `core.Deployment`. |
| Telemetry | Claude Code's own OpenTelemetry export, sent to Agbredo with a telemetry token, one per agent run. | `run_tokens`, `otlp_*` tables. |
| Actor | GitHub login that started the GitHub run. | `Run.Actor`. |
| Triggering actor | GitHub login that started this attempt. | `Run.TriggeringActor`. |

## Not part of the model

- Claude Code subagents. They run inside one agent run. We see them only through telemetry.
- Activation. It meant the same as Run. Use Run.
