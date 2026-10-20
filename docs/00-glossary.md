# Glossary

The words we use for the domain. Code, database and UI should use the same words with the same meaning. Let's keep this file free of implementation details.

- Pipeline: One implementation, kept in a central repository as a reusable workflow. Identified by its path, e.g. `xpmatteo/agbredo-pipelines/.github/workflows/foobar.yml`.  Pipelines are composed of various Steps. The logic of activation of the Steps depends on the logic in the GitHub action
- Step: a step of job of a GitHub action 
- Deterministic Step: a Step that is implemented through conventional automation (not AI-based)
- Agent: a Step that is implemented with an AI agent.  Two Agent Steps are two Agents, even in the same job.
- Version: A Pipeline's version. It's preferred to use readable identifiers, eg v2, v3
- Deployment: A Pipeline used in a repository.  Identified by Repository + Pipeline ID
- Run: a GitHub "run attempt" of a Deployment
- Agent Run: one execution of an Agent in a Run
- Observation: any information coming to Agbredo from a Run. Typically: Webhooks and Claude Code Telemetry

