# Claude Code telemetry to Agbredo

A small action which lets Claude Code send its OpenTelemetry straight to Agbredo. It replaces the wrapper and sidecar in `integrations/claude-code/`. That older integration still works.

## Use

1. Register the workflow in Agbredo. Set the repository variables `AGBREDO_URL` and `AGBREDO_AGENT_ID`.
2. Give the job `id-token: write`, next to its other permissions.
3. Copy this directory into the repository. Put the action before each Claude Code step. Give each one its own `source` name. Letters, digits, dots, underscores and hyphens are allowed.

```yaml
- uses: ./integrations/claude-code-otlp
  with:
    url: ${{ vars.AGBREDO_URL }}
    agent-id: ${{ vars.AGBREDO_AGENT_ID }}
    source: review
- uses: anthropics/claude-code-action@v1
  with:
    claude_code_oauth_token: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
    prompt: Your existing prompt
```

The Claude step stays as it was. The action only sets environment variables for the steps after it. Each step shows up on the run page under its `source`.

## What it does

It asks Agbredo for a token with the job's GitHub identity, masks it in the logs, and writes these to `GITHUB_ENV`: `CLAUDE_CODE_ENABLE_TELEMETRY`, the `OTEL_*` exporter settings for `http/json` and the token header, `OTEL_METRICS_INCLUDE_VERSION=true` (so the Claude Code version is reported), and the `OTEL_LOG_*` settings that keep prompts, answers and tool details out of the export.

If anything fails, it prints a warning, turns telemetry off for the next step, and succeeds. The agent is never stopped by reporting.

It needs `curl` and `jq`, which GitHub's Linux and macOS runners have. It does not install or change Claude Code.

## Limits

- Settings in Claude Code's managed settings can override the environment. Then no telemetry arrives, and the dashboard shows the step's numbers as unavailable.
- Run status and duration come from GitHub, not from this action.
- The token lasts 6 hours. A longer step stops reporting after that.
