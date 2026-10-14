# Claude Code telemetry to Agbredo

A small action which lets Claude Code send its OpenTelemetry straight to Agbredo. It is the only way usage reaches Agbredo. It replaced an older wrapper and sidecar (`integrations/claude-code/`), which has been removed along with its report route.

## Use

1. Install Agbredo's GitHub App on the repository. Set the repository variable `AGBREDO_URL`. There is nothing to register: the first run that gets a token creates the deployment in Agbredo, named after the repository and workflow file. Runs from any branch or tag belong to it.
2. Give the job `id-token: write`, next to its other permissions.
3. Put the action before each Claude Code step. Refer to it in this repository, so there is nothing to copy. Give each one its own `source` name. The input is called `source` for backwards compatibility; it names the agent run. Letters, digits, dots, underscores and hyphens are allowed.

```yaml
- uses: xpmatteo/agbredo/integrations/claude-code-otlp@main
  with:
    url: ${{ vars.AGBREDO_URL }}
    source: review
- uses: anthropics/claude-code-action@v1
  with:
    claude_code_oauth_token: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
    prompt: Your existing prompt
```

`@main` keeps the action in step with the server, which is deployed from `main`. Workflows in this repository use `./integrations/claude-code-otlp`.

The Claude step stays as it was. The action only sets environment variables for the steps after it. Each agent run shows up on the run page under its `source` name.

The `agent-id` input is optional and kept for workflows written before deployments created themselves. With it, the action asks `POST /api/agents/{id}/otlp-token` for that deployment, as before. Without it, it asks `POST /api/otlp-token`, and Agbredo finds the deployment by the job's repository and workflow file.

## What it does

It asks Agbredo for a token with the job's GitHub identity, masks it in the logs, and writes these to `GITHUB_ENV`: `CLAUDE_CODE_ENABLE_TELEMETRY`, the `OTEL_*` exporter settings for `http/json` and the token header, `OTEL_METRICS_INCLUDE_VERSION=true` (so the Claude Code version is reported), `OTEL_METRICS_INCLUDE_ACCOUNT_UUID=true` (so the Claude account is recorded), traces (`OTEL_TRACES_EXPORTER=otlp` and the beta switch `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`), `TRACEPARENT` (so every Claude Code step of the run is in one trace), and the `OTEL_LOG_*` settings that keep prompts, answers and tool details out of the export.

If anything fails, it prints a warning (with Agbredo's HTTP status and reason when Agbredo refuses), turns telemetry off for the next step, and succeeds. The agent is never stopped by reporting. Agbredo refuses a token when the GitHub App is not installed on the repository, or when the deployment is disabled on the dashboard.

It needs `curl` and `jq`, which GitHub's Linux and macOS runners have. It does not install or change Claude Code.

## Limits

- Checked with `anthropics/claude-code-action` (pinned v1) and Claude Code 2.1.299: tokens, cache tokens, cost and the Claude Code version all arrived, including the final export when Claude Code exits. The action lists only some `OTEL_*` variables itself, but the ones set in `GITHUB_ENV` reach it anyway.
- Settings in Claude Code's managed settings can override the environment. Then no telemetry arrives, and the dashboard shows the step's numbers as unavailable.
- Run status and duration come from GitHub, not from this action.
- The token lasts 6 hours. A longer step stops reporting after that.
