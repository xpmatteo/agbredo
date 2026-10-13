#!/usr/bin/env python3
"""Offline installation check. Uses a dummy OAuth token and no model prompt."""
import json
import os
import subprocess
import tempfile
import threading

from report import Measurements, Receiver, probe, telemetry_env


def main():
    version, native_support = probe()
    measurements = Measurements()
    receiver = Receiver(measurements)
    observed_metrics = []
    original = measurements.metrics
    def capture(payload):
        for resource in payload.get("resourceMetrics", []):
            for scope in resource.get("scopeMetrics", []):
                for metric in scope.get("metrics", []):
                    name = metric.get("name", "")
                    if name.startswith("claude_code."):
                        observed_metrics.append({"name": name, "unit": metric.get("unit"),
                            "temporality": metric.get("sum", {}).get("aggregationTemporality"),
                            "point_fields": sorted({key for point in metric.get("sum", {}).get("dataPoints", []) for key in point})})
        original(payload)
    measurements.metrics = capture
    thread = threading.Thread(target=receiver.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="agbredo-claude-probe-") as config:
            env = telemetry_env(receiver.server_port)
            for key in list(env):
                if key.startswith(("ANTHROPIC_", "CLAUDE_")):
                    env.pop(key)
            env.update(CLAUDE_CONFIG_DIR=config,
                       CLAUDE_CODE_OAUTH_TOKEN="agbredo-offline-probe-not-a-real-credential",
                       CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
                       CLAUDE_CODE_ENABLE_TELEMETRY="1")
            auth = subprocess.run(["claude", "auth", "status", "--json"], env=env,
                                  capture_output=True, text=True, timeout=20, cwd=config)
            status = json.loads(auth.stdout)
            execution = subprocess.run(["claude", "-p", "--input-format", "stream-json",
                                       "--output-format", "stream-json", "--verbose"], input="",
                                      env=env, capture_output=True, text=True, timeout=20, cwd=config)
            # Only capability evidence is printed; CLI output and auth identities
            # never appear in the report. An empty input performs no model call.
            evidence = {"version": version, "native_telemetry_markers": native_support,
                        "oauth_token_recognized": status.get("authMethod") == "oauth_token",
                        "empty_input_exit_code": execution.returncode}
    finally:
        receiver.shutdown()
        receiver.server_close()
    evidence["recognized_otlp_received"] = measurements.received
    evidence["observed_metric_schemas"] = observed_metrics
    print(json.dumps(evidence, indent=2))


if __name__ == "__main__":
    main()
