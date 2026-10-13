#!/usr/bin/env python3
"""Collect around an existing uses: step without replacing or invoking it."""
import argparse
import hmac
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import threading
import time
import urllib.request

from report import Client, Handler, Measurements, NoRedirect, Receiver, probe, save_report, telemetry_env


def github_number(name):
    value = os.environ.get(name, "")
    return int(value) if re.fullmatch(r"[0-9]{1,19}", value) else 0


def telemetry_settings(port):
    configured = telemetry_env(port)
    # Only telemetry configuration can be exported or restored. Never snapshot
    # the runner's general environment or its Claude/GitHub credentials.
    keys = {k for k in configured if k.startswith("OTEL_")}
    keys.add("CLAUDE_CODE_ENABLE_TELEMETRY")
    keys.update(k for k in os.environ if k.startswith("OTEL_EXPORTER_OTLP_"))
    keys.add("BETA_TRACING_ENDPOINT")
    keys.add("OTEL_RESOURCE_ATTRIBUTES")
    return {key: configured.get(key, "") for key in keys}


def export_environment(values):
    target = os.environ.get("GITHUB_ENV")
    if not target:
        return
    with open(target, "a") as f:
        for key, value in values.items():
            delimiter = "AGBREDO_" + secrets.token_hex(16)
            f.write("%s<<%s\n%s\n%s\n" % (key, delimiter, value or "", delimiter))


class SidecarHandler(Handler):
    def do_POST(self):
        if self.path != "/finish":
            return super().do_POST()
        if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + self.server.control_token):
            self.send_error(403)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 4096:
                raise ValueError("invalid control request")
            request = json.loads(self.rfile.read(length))
            if request["outcome"] not in ("success", "failure", "cancelled", "skipped"):
                raise ValueError("invalid outcome")
            if not re.fullmatch(r"unknown|[0-9]+\.[0-9]+\.[0-9]+", request["version"]):
                raise ValueError("invalid version")
            result = dict(self.server.initial_report)
            result.update(self.server.measurements.report(), phase="finished",
                          action_outcome=request["outcome"], version=request["version"],
                          duration_ms=int((time.monotonic() - self.server.started) * 1000),
                          telemetry_supported=request.get("supported") is True or self.server.measurements.received)
            body = json.dumps(result).encode()
        except (ValueError, KeyError, TypeError):
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        threading.Thread(target=self.server.shutdown, daemon=True).start()


def serve(args):
    receiver = Receiver(Measurements())
    receiver.RequestHandlerClass = SidecarHandler
    receiver.control_token = secrets.token_hex(32)
    receiver.started = time.monotonic()
    receiver.initial_report = {"github_run_id": github_number("GITHUB_RUN_ID"),
        "github_run_attempt": github_number("GITHUB_RUN_ATTEMPT"), "source": args.source,
        "phase": "started", "version": "unknown", "duration_ms": 0,
        "telemetry_supported": False, **Measurements().report()}
    # The existing action installs Claude internally. Probe it only after that
    # step; setup must not install a different CLI or change authentication.
    settings = telemetry_settings(receiver.server_port)
    state = {"endpoint": "http://127.0.0.1:%d" % receiver.server_port,
             "control_token": receiver.control_token, "started": receiver.started,
             "initial_report": receiver.initial_report, "settings": settings,
             "restore": {key: os.environ.get(key, "") for key in settings}}
    save_report(args.state, state)
    if args.url and args.agent_id:
        def started():
            try:
                Client(args.url, args.agent_id).send(receiver.initial_report)
            except (ValueError, OSError):
                pass
        threading.Thread(target=started, daemon=True).start()
    try:
        receiver.serve_forever()
    finally:
        receiver.server_close()


def start(args):
    path = Path(args.state)
    if path.exists():
        raise ValueError("collector state already exists; use a unique source per invocation")
    path.parent.mkdir(parents=True, exist_ok=True)
    command = [sys.executable, str(Path(__file__).resolve()), "serve", "--state", str(path),
               "--source", args.source]
    if args.url and args.agent_id:
        command += ["--url", args.url, "--agent-id", args.agent_id]
    env = os.environ.copy()
    # The collector needs GitHub OIDC for reporting, never a model credential.
    for key in list(env):
        if key.startswith("ANTHROPIC_") or key == "CLAUDE_CODE_OAUTH_TOKEN":
            env.pop(key)
    # Keep RUNNER_TRACKING_ID so job teardown cleans up an abandoned collector.
    child = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if path.exists():
            state = json.loads(path.read_text())
            export_environment(state["settings"])
            return
        if child.poll() is not None:
            raise ValueError("local collector did not start")
        time.sleep(0.05)
    child.terminate()
    raise ValueError("local collector startup timed out")


def finish(args):
    state = json.loads(Path(args.state).read_text())
    version, supported = probe()
    result = dict(state["initial_report"])
    result.update(phase="finished", action_outcome=args.action_outcome, version=version,
                  duration_ms=max(0, int((time.monotonic() - state["started"]) * 1000)),
                  telemetry_supported=supported)
    try:
        request = urllib.request.Request(state["endpoint"] + "/finish", data=json.dumps({
            "outcome": args.action_outcome, "version": version, "supported": supported}).encode(),
            headers={"Authorization": "Bearer " + state["control_token"], "Content-Type": "application/json"})
        opener = urllib.request.build_opener(NoRedirect())
        with opener.open(request, timeout=10) as response:
            result = json.load(response)
    except (OSError, ValueError):
        print("::warning::Collector unavailable; reporting action status with missing telemetry.", file=sys.stderr)
    finally:
        # Only restore telemetry values we changed; do not touch authentication.
        export_environment(state["restore"])
    save_report(args.output, result)
    if args.url and args.agent_id:
        Client(args.url, args.agent_id).send(result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("start", "finish", "serve"))
    parser.add_argument("--state", required=True)
    parser.add_argument("--output", default="agbredo-report.json")
    parser.add_argument("--source", default="claude")
    parser.add_argument("--url", default=os.environ.get("AGBREDO_URL"))
    parser.add_argument("--agent-id", default=os.environ.get("AGBREDO_AGENT_ID"))
    parser.add_argument("--action-outcome", choices=("success", "failure", "cancelled", "skipped"), default="failure")
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", args.source):
        parser.error("invalid reporting source")
    try:
        {"start": start, "finish": finish, "serve": serve}[args.mode](args)
    except (OSError, ValueError, KeyError):
        print("::warning::Agbredo reporting unavailable; the existing agent action is unaffected.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
