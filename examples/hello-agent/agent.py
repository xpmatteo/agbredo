#!/usr/bin/env python3
"""Example agents for Agbredo. They call no model and make up their numbers.

Each scenario plays one or more acts. An act sends a "started" report, does
its (pretend) work, then sends a "finished" report. Run it from a GitHub
Actions job that has `id-token: write`, with AGBREDO_URL and AGBREDO_AGENT_ID
set. Without AGBREDO_URL it prints the reports instead of sending them.

    python3 agent.py hello
    python3 agent.py --list

The "otlp" scenario is different. It plays a Claude Code step that sends its
OpenTelemetry straight to Agbredo, using the token from integrations/claude-code-otlp.
Run that action first, or set OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS.
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def act(source, says, exit_code=0, usage=None, cost_micros=None, api_errors=0, rate_limits=0):
    """One reporting source. `usage=None` means no measurements are reported."""
    return {"source": source, "says": says, "exit_code": exit_code, "usage": usage,
            "cost_micros": cost_micros, "api_errors": api_errors, "rate_limits": rate_limits}


def zero_usage():
    return {"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0}


SCENARIOS = {
    "hello": [act("greeter", "hi", usage=zero_usage(), cost_micros=0)],
    "play": [
        act("starting", "The curtain rises.", usage=zero_usage(), cost_micros=0),
        act("hello", "hi", usage={"input_tokens": 12, "output_tokens": 2, "cache_read_tokens": 0, "cache_creation_tokens": 0}, cost_micros=90),
        act("closing", "Goodbye, and thanks for the fish.", usage={"input_tokens": 30, "output_tokens": 9, "cache_read_tokens": 12, "cache_creation_tokens": 0}, cost_micros=210),
    ],
    "rate-limited": [
        act("grumpy", "I wanted to say hi, but the API said 429.", exit_code=1,
            usage={"input_tokens": 800, "output_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0},
            cost_micros=2400, api_errors=3, rate_limits=3),
    ],
    "spendthrift": [
        act("spendthrift", "Hi! I read the whole monorepo first.",
            usage={"input_tokens": 1_200_000, "output_tokens": 40_000, "cache_read_tokens": 3_000_000, "cache_creation_tokens": 250_000},
            cost_micros=12_500_000),
    ],
    "silent": [act("silent", "hi (no telemetry was collected)")],
    "cancelled": [act("sleepy", "hi... zzz", exit_code=143)],
}


def otlp_payloads(now_ns=None):
    """What a Claude Code step would export: one metrics body, the same body again
    (a retry, which Agbredo must count once), and one rate-limit error log."""
    now_ns = now_ns or time.time_ns()

    def point(value, **attrs):
        keys = [{"key": k, "value": {"stringValue": v}} for k, v in attrs.items()]
        return {"attributes": keys, "startTimeUnixNano": str(now_ns - 5_000_000_000), "timeUnixNano": str(now_ns), **value}

    def counter(name, points):
        return {"name": name, "sum": {"aggregationTemporality": 1, "isMonotonic": True, "dataPoints": points}}

    tokens = [point({"asInt": str(n)}, type=kind, **{"app.version": "0.0.0"})
              for kind, n in (("input", 1200), ("output", 80), ("cacheRead", 5000), ("cacheCreation", 300))]
    metrics = {"resourceMetrics": [{"resource": {}, "scopeMetrics": [{"scope": {"name": "pretend"}, "metrics": [
        counter("claude_code.token.usage", tokens),
        counter("claude_code.cost.usage", [point({"asDouble": 0.0123})])]}]}]}
    logs = {"resourceLogs": [{"resource": {}, "scopeLogs": [{"scope": {"name": "pretend"}, "logRecords": [{
        "timeUnixNano": str(now_ns), "attributes": [
            {"key": "event.name", "value": {"stringValue": "api_error"}},
            {"key": "status_code", "value": {"stringValue": "429"}},
            {"key": "error", "value": {"stringValue": "rate limit (pretend)"}}]}]}]}]}
    return [("/v1/metrics", metrics), ("/v1/metrics", metrics), ("/v1/logs", logs)]


def play_otlp(env=os.environ, out=sys.stdout, post=None):
    endpoint = env.get("OTEL_EXPORTER_OTLP_ENDPOINT", "").rstrip("/")
    headers = dict(h.split("=", 1) for h in env.get("OTEL_EXPORTER_OTLP_HEADERS", "").split(",") if "=" in h)
    print("[otlp] Pretending to be Claude Code, sending telemetry.", file=out)
    for path, body in otlp_payloads():
        if not endpoint:
            print(path, json.dumps(body), file=out)
            continue
        request = urllib.request.Request(endpoint + path, data=json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json", **headers})
        try:
            with (post or urllib.request.urlopen)(request, timeout=10) as response:
                response.read(1024)
        except (OSError, ValueError) as error:
            print(f"::warning::Agbredo telemetry unavailable: {type(error).__name__}", file=sys.stderr)
    return 0


def started_report(source, run_id, attempt):
    return {"github_run_id": run_id, "github_run_attempt": attempt, "source": source, "phase": "started",
            "version": "unknown", "duration_ms": 0, "telemetry_supported": False,
            "telemetry_received": False, "api_errors": 0, "rate_limit_failures": 0, "api_failed": False}


def finished_report(a, run_id, attempt, duration_ms):
    report = started_report(a["source"], run_id, attempt)
    report.update({"phase": "finished", "exit_code": a["exit_code"], "duration_ms": duration_ms})
    if a["usage"] is not None:
        report.update({"telemetry_supported": True, "telemetry_received": True, "usage": a["usage"],
                       "estimated_cost_micros": a["cost_micros"], "api_errors": a["api_errors"],
                       "rate_limit_failures": a["rate_limits"], "api_failed": a["api_errors"] > 0})
    return report


class Client:
    def __init__(self, base_url, agent_id):
        self.base_url = base_url.rstrip("/")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", agent_id):
            raise ValueError("invalid agent ID")
        self.url = self.base_url + "/api/agents/" + agent_id + "/observations"

    def token(self):
        # GitHub signs a short-lived token for this job; it is the only credential.
        url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"]
        url += ("&" if "?" in url else "?") + urllib.parse.urlencode({"audience": self.base_url})
        request = urllib.request.Request(url, headers={"Authorization": "Bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]})
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)["value"]

    def send(self, report):
        request = urllib.request.Request(self.url, data=json.dumps(report).encode(), headers={
            "Authorization": "Bearer " + self.token(), "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=10) as response:
            response.read(65536)


def play(name, client, run_id, attempt, pause=1.0, out=sys.stdout):
    for a in SCENARIOS[name]:
        deliver(client, started_report(a["source"], run_id, attempt), out)
        began = time.monotonic()
        print(f"[{a['source']}] {a['says']}", file=out)
        time.sleep(pause)
        deliver(client, finished_report(a, run_id, attempt, int((time.monotonic() - began) * 1000)), out)


def deliver(client, report, out):
    if client is None:
        print(json.dumps(report), file=out)
        return
    try:
        client.send(report)
    except (OSError, KeyError, ValueError) as error:
        # Like the real reporter, never fail the agent because reporting failed.
        print(f"::warning::Agbredo report unavailable: {type(error).__name__}", file=sys.stderr)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("scenario", nargs="?", default="hello", choices=sorted(SCENARIOS) + ["otlp"])
    parser.add_argument("--list", action="store_true", help="list the scenarios and exit")
    args = parser.parse_args(argv)
    if args.list:
        for name, acts in SCENARIOS.items():
            print(f"{name}: {', '.join(a['source'] for a in acts)}")
        print("otlp: sends Claude Code telemetry straight to Agbredo")
        return 0
    if args.scenario == "otlp":
        return play_otlp()
    url, agent = os.environ.get("AGBREDO_URL", ""), os.environ.get("AGBREDO_AGENT_ID", "")
    client = Client(url, agent) if url and agent else None
    run_id = int(os.environ.get("GITHUB_RUN_ID", "1"))
    attempt = int(os.environ.get("GITHUB_RUN_ATTEMPT", "1"))
    play(args.scenario, client, run_id, attempt)
    # Like a real agent, the job fails when the last act failed.
    return SCENARIOS[args.scenario][-1]["exit_code"]


if __name__ == "__main__":
    sys.exit(main())
