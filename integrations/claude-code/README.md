# Reporting existing Claude Code workflows

This integration runs beside Claude Code on the GitHub runner. Claude Code continues using the existing `CLAUDE_CODE_OAUTH_TOKEN` GitHub Actions secret and existing command, permissions, installation and settings. Agbredo receives numeric observations authenticated with GitHub OIDC. The Go process has no Claude client, model execution path, Claude credential setting or subscription access.

```text
GitHub runner: existing secret → existing Claude Code command
                                     ↓ local OpenTelemetry
                              loopback Python receiver
                                     ↓ typed report + GitHub OIDC
                              Go dashboard + PostgreSQL
```

## Minimal workflow change

1. Copy this directory into the existing agent repository as `integrations/claude-code`. It is a local composite action, not a reusable workflow. Keep the existing workflow file, triggers, run name, checkout, Claude installation and authentication steps.
2. Register the existing repository, workflow **filename**, Git ref and GitHub App installation ID in Agbredo. Enable reporting.
3. Add `id-token: write` alongside the job's **existing** permission entries. Keep its other permissions intact. Replace only the existing Claude execution step with the local action and pass its original shell command as `command`:

```yaml
# Keep the existing checkout and Claude installation/authentication steps above.
- name: Existing agent
  id: claude_agent
  uses: ./integrations/claude-code
  env:
    CLAUDE_CODE_OAUTH_TOKEN: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
    # Keep the existing step's other environment variables.
  with:
    url: ${{ vars.AGBREDO_URL }}
    agent-id: ${{ vars.AGBREDO_AGENT_ID }}
    source: agent-main
    command: ./scripts/existing-agent.sh
```

`command` may also contain the unchanged `claude -p ...` invocation. It runs under `bash -e -o pipefail`, with inherited stdin, stdout and stderr. Preserve any flags, prompt handling and working directory requirements in the command. Linux/macOS runners with Python 3 are supported; the action does not install or upgrade Claude Code. Do not add `--bare`: the verified version does not read the subscription OAuth environment variable in bare mode. No API key or alternative provider is introduced.

Use a distinct stable `source` for every execution step, job and matrix cell in a workflow attempt; for example `review-linux` and `review-macos`. Only letters, digits, dots, underscores and hyphens are accepted. Each source sends one `started` and one `finished` observation. Repeating the same source within an attempt will be deduplicated; use separate source names for separate invocations.

For an existing `run:` step where retaining its shell configuration is useful, copy the script and wrap the command directly instead:

```yaml
env:
  CLAUDE_CODE_OAUTH_TOKEN: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
  AGBREDO_URL: ${{ vars.AGBREDO_URL }}
  AGBREDO_AGENT_ID: ${{ vars.AGBREDO_AGENT_ID }}
run: |
  python3 integrations/claude-code/report.py --source agent-main \
    --output "$RUNNER_TEMP/agbredo-report.json" -- ./scripts/existing-agent.sh
```

## What is reported

### Keep an existing `uses: anthropics/claude-code-action@v1` step

Use `mode: start` before the existing action and `mode: finish` immediately after it. This keeps the original Claude step, OAuth input, prompt, arguments, installation and outputs intact:

```yaml
- id: agbredo_start
  if: vars.AGBREDO_URL != '' && vars.AGBREDO_AGENT_ID != ''
  continue-on-error: true
  uses: ./integrations/claude-code
  with:
    mode: start
    url: ${{ vars.AGBREDO_URL }}
    agent-id: ${{ vars.AGBREDO_AGENT_ID }}
    source: existing-agent

# Keep your existing step and its inputs unchanged.
- id: claude
  uses: anthropics/claude-code-action@v1
  with:
    claude_code_oauth_token: ${{ secrets.CLAUDE_CODE_OAUTH_TOKEN }}
    prompt: Your existing prompt

- if: always() && steps.agbredo_start.outcome == 'success'
  continue-on-error: true
  uses: ./integrations/claude-code
  with:
    mode: finish
    url: ${{ vars.AGBREDO_URL }}
    agent-id: ${{ vars.AGBREDO_AGENT_ID }}
    source: existing-agent
    action-outcome: ${{ steps.claude.outcome }}
```

Retain any existing gate in the start condition as well as the Claude step. Both reporting steps use the same source and report path. The collector detaches across steps with its streams redirected, keeps GitHub's runner tracking for job teardown, and writes state only under `runner.temp`. Finish restores the telemetry environment values and saves only the allowlisted observation. Private state can contain prior OTLP collector header settings and a local control token; do not upload the `.state` file. Claude credentials are excluded from the collector process environment.

The adapter probes the installed CLI after the Claude action finishes because that action installs it internally. It reports the actual **GitHub action outcome**, without inventing a Claude process exit code. No action stdout, transcript, or undocumented SDK result field is parsed. Reporting failures do not change the original action's failure/cancellation behavior. If the job is forcibly killed before finish, runner teardown cleans up the collector and usage can be missing.

Checked against the current [Anthropic action configuration](https://raw.githubusercontent.com/anthropics/claude-code-action/v1/action.yml) and [installation implementation](https://raw.githubusercontent.com/anthropics/claude-code-action/v1/src/entrypoints/run.ts): telemetry variables are explicitly forwarded, and the installed CLI is currently 2.1.296. No CLI installation or authentication input is changed by this adapter.

| Measurement | Source / meaning |
| --- | --- |
| Installed CLI version | `claude --version`; unknown if the CLI probe fails |
| Execution status and duration | Actual child exit code for command wrapping, or the original action outcome for start/finish mode; unmatched terminal API errors also flag the execution as failed |
| Input / output tokens | Recognized `claude_code.token.usage` counters, when exported |
| Cache read / creation tokens | Separate token counter types; excluded from the ordinary input count |
| Estimated API-equivalent consumption | Claude Code's `claude_code.cost.usage` estimate, aggregated as decimal USD and rounded once to integer microdollars |
| API / rate-limit failures | Deduplicated `api_error` events; HTTP 429 is definitive, and recognized subscription-limit messages are classified as rate-limit failures |
| Actual subscription charges | **Unknown**; never derived from token counts or consumption estimates |
| Remaining Pro quota / reset time | **Unknown**; these observations are not a subscription quota API |

The dashboard displays **Unavailable** for missing token or cost measurements. A measured zero stays distinct from a missing value. Totals include only received samples and can be partial. A threshold applies to estimated consumption, not money charged to the subscription; crossing it only displays an advisory flag in observation mode.

Claude process status is separate from the workflow status. The workflow may continue with other steps, recover from errors or intentionally tolerate a failed command. GitHub webhooks and polling supply its authoritative conclusion. Signed OIDC identity is bound to the registered repository, workflow and ref, and GitHub's specific run attempt. A GitHub rerun gets a separate immutable run and accounting record. No correlation inputs or custom run names are required.

Agbredo creates no dispatches or issues and has no endpoint that acts on GitHub. It discovers runs through reports, not by scanning every historical execution. If no report reaches Agbredo, that execution is not registered. Direct jobs are supported; externally called reusable workflows and pull-request OIDC identities remain excluded by the existing verifier. Scheduled, push, issue/comment and manual workflows on the registered ref can report.

## Telemetry and failure handling

The reporter starts an HTTP/JSON OTLP receiver on a random loopback port, enables Claude telemetry only for the wrapped process, and directs metrics and logs to that receiver. Existing external OTLP routing is overridden for that command. It does not write global Claude settings. Existing managed Claude settings can override the environment; if telemetry does not arrive, execution still runs and the report marks it unavailable.

Prompt, assistant response, tool content, raw body, account UUID and tracing exports are disabled. The receiver discards content, account identities and arbitrary attributes. Only typed counters, failure counts, a version string, a source identifier, GitHub run/attempt identifiers, exit code and duration leave the runner. Error text is inspected locally for rate-limit classification and immediately discarded. The sender validates the same allowlist again before every HTTP request, including saved-report retries. Raw stdout, stderr, session transcripts, OTLP payloads and environment variables are never uploaded to Agbredo.

Metrics accept monotonic delta or cumulative sums, distinguish models/processes/counter dimensions, handle resets, and deduplicate repeated exports. Input/output/cache counters and CLI cost counters are counted once; log events are used for failures, not a second cost total. Rate-limit message matching is a classification heuristic and cannot identify every version's subscription error. A failed API request followed by a later successful request stays in failure counts but does not by itself flag terminal API failure.

Reporting retries use the same event identity and refresh GitHub OIDC; they never retry the Claude command. An unavailable control plane cannot delay command startup or change the command's exit code. Final reporting can wait up to about a minute for bounded delivery attempts. The final sanitized report is saved under the runner's temporary directory and exposed through the action's `report-file` output. Retry delivery within the **same workflow attempt**, while OIDC is available:

```sh
python3 integrations/claude-code/report.py --send "$REPORT_FILE"
```

SIGINT/SIGTERM are forwarded to the existing command's process group and the wrapper attempts to save/report its final state. A runner kill, SIGKILL, a crash, or a missing final telemetry flush can leave usage partial or unavailable; GitHub lifecycle reconciliation still records workflow failure/cancellation if a start report registered the run. The sanitized report may be retained as an artifact if desired; this integration does not upload raw agent logs or telemetry artifacts.

## Installed-version verification

Verified locally on **2026-10-13**, using installed **Claude Code 2.1.296**:

- `claude --help` supports print mode and text/JSON/stream-JSON output. The integration does not parse any of those output fields.
- In an isolated temporary configuration, `claude auth status --json` recognized a **dummy** `CLAUDE_CODE_OAUTH_TOKEN` as `oauth_token`. This verifies credential selection, not validity of the user's subscription or token.
- The installed native executable contains the OAuth/telemetry environment names, HTTP/JSON exporter support, and token/cost counter names.
- An empty-input print-mode process emitted a real `claude_code.session.count` OTLP JSON delta sum: temporality `1`, `asDouble`, `attributes`, `startTimeUnixNano`, and `timeUnixNano`. It exited successfully without a model call. See [saved verification evidence](verified-2.1.296.json).
- Token/cache/cost dimensions and `api_error` fields were checked against [Anthropic monitoring documentation](https://code.claude.com/docs/en/monitoring-usage). Their values and rate-limit recovery behavior are tested with synthetic fixtures; no paid model call or deliberate subscription-limit exhaustion was performed. [Anthropic authentication documentation](https://code.claude.com/docs/en/authentication) confirms subscription token support and the bare-mode restriction.

The action probes the installed version and native capability markers on each execution. npm launchers can hide native markers; recognized runtime OTLP samples then establish support. Unknown schemas are ignored rather than fabricated into usage. Version changes should be verified on the target runner with:

```sh
python3 integrations/claude-code/report.py --probe
python3 integrations/claude-code/verify_installed.py
python3 -m unittest discover -s integrations/claude-code -v
```

`verify_installed.py` uses a dummy credential, isolated config and empty input. It emits capability/schema evidence only. A target workflow smoke run is still necessary to confirm usage exports from its actual installation and settings. Agbredo does not attempt to validate the real Claude OAuth secret.
