#!/usr/bin/env python3
"""Pretend to be a Claude Code step that sends its OpenTelemetry to Agbredo.

It calls no model and makes up its numbers. Run the integrations/claude-code-otlp
action first, so OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS are
set. Without OTEL_EXPORTER_OTLP_ENDPOINT it prints the payloads instead.

    python3 send.py
"""
import json
import os
import sys
import time
import urllib.request


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


def send(env=os.environ, out=sys.stdout, post=None):
    endpoint = env.get("OTEL_EXPORTER_OTLP_ENDPOINT", "").rstrip("/")
    headers = dict(h.split("=", 1) for h in env.get("OTEL_EXPORTER_OTLP_HEADERS", "").split(",") if "=" in h)
    print("Pretending to be Claude Code, sending telemetry.", file=out)
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
            # Like the telemetry step, never fail the job because Agbredo is unavailable.
            print(f"::warning::Agbredo telemetry unavailable: {type(error).__name__}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(send())
