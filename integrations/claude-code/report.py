#!/usr/bin/env python3
"""Run an unchanged command; collect Claude OTLP locally; send an allowlisted report.

Only Python's standard library is required. No Claude credentials or output are
sent to Agbredo. See README.md for verification evidence and schema limitations.
"""
import argparse
import decimal
import hashlib
import http.server
import json
import mmap
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def attributes(items):
    # OTLP JSON AnyValue is typed; unknown types are ignored.
    result = {}
    for item in items:
        value = item.get("value", {})
        for kind in ("stringValue", "intValue", "doubleValue", "boolValue"):
            if kind in value:
                result[item.get("key")] = value[kind]
                break
    return result


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def number(value):
    if isinstance(value, bool):
        raise ValueError("boolean measurement")
    n = decimal.Decimal(str(value))
    if not n.is_finite() or n < 0:
        raise ValueError("invalid measurement")
    return n


class Measurements:
    """Discard all content and identities; retain numeric counters and hashes only."""
    TYPES = {"input": "input_tokens", "output": "output_tokens",
             "cacheRead": "cache_read_tokens", "cacheCreation": "cache_creation_tokens"}

    def __init__(self):
        self.usage = {}
        self.cost = None
        self.received = False
        self.seen = set()
        self.cumulative = {}
        self.api_errors = self.rate_limits = 0
        self.last_error = self.last_success = 0

    def metrics(self, payload):
        for resource in payload.get("resourceMetrics", []):
            for scope in resource.get("scopeMetrics", []):
                for metric in scope.get("metrics", []):
                    name = metric.get("name")
                    if name not in ("claude_code.token.usage", "claude_code.cost.usage", "claude_code.session.count"):
                        continue
                    counter = metric.get("sum", {})
                    temporality = counter.get("aggregationTemporality")
                    if temporality not in (1, 2) or counter.get("isMonotonic") is not True:
                        continue
                    for point in counter.get("dataPoints", []):
                        attrs = attributes(point.get("attributes", []))
                        field = self.TYPES.get(attrs.get("type"))
                        if name == "claude_code.token.usage" and field is None:
                            continue
                        try:
                            amount = number(point.get("asInt", point.get("asDouble")))
                            if name == "claude_code.token.usage" and (amount != amount.to_integral_value() or amount > 1_000_000_000):
                                continue
                            if name == "claude_code.cost.usage" and amount > 1_000_000:
                                continue
                            start, end = int(point["startTimeUnixNano"]), int(point["timeUnixNano"])
                        except (ValueError, TypeError, KeyError, decimal.InvalidOperation):
                            continue
                        # Resource + scope + all dimensions distinguish process/model/
                        # subagent counters. These values are hashed, never retained.
                        series = fingerprint([resource.get("resource"), scope.get("scope"), name, attrs, start])
                        sample = fingerprint([series, end, str(amount)])
                        if sample in self.seen:
                            continue
                        self.seen.add(sample)
                        self.received = True
                        if temporality == 2:
                            previous_time, previous_value = self.cumulative.get(series, (0, decimal.Decimal(0)))
                            if end <= previous_time or amount < previous_value:
                                continue
                            self.cumulative[series] = (end, amount)
                            amount -= previous_value
                        if name == "claude_code.cost.usage":
                            self.cost = (self.cost or decimal.Decimal(0)) + amount
                        elif name == "claude_code.token.usage":
                            self.usage[field] = self.usage.get(field, 0) + int(amount)

    def logs(self, payload):
        for resource in payload.get("resourceLogs", []):
            for scope in resource.get("scopeLogs", []):
                for record in scope.get("logRecords", []):
                    attrs = attributes(record.get("attributes", []))
                    name = attrs.get("event.name")
                    if name not in ("api_error", "api_request"):
                        continue
                    key = fingerprint([resource.get("resource"), scope.get("scope"), record])
                    if key in self.seen:
                        continue
                    try:
                        timestamp = int(record["timeUnixNano"])
                    except (KeyError, TypeError, ValueError):
                        continue
                    self.seen.add(key)
                    self.received = True
                    if name == "api_request":
                        self.last_success = max(self.last_success, timestamp)
                        continue
                    self.last_error = max(self.last_error, timestamp)
                    self.api_errors += 1
                    # 429 is definitive. Subscription-limit messages can lack an
                    # HTTP code; these patterns are a documented classification.
                    error = str(attrs.get("error", "")).lower()
                    if str(attrs.get("status_code")) == "429" or re.search(
                            r"rate[ _-]?limit|usage limit|hit your limit|limit reached", error):
                        self.rate_limits += 1
                    # Do not keep or forward the error string, which can contain
                    # prompts, request bodies or credentials.

    def report(self):
        result = {"telemetry_received": self.received,
                  "api_errors": self.api_errors, "rate_limit_failures": self.rate_limits,
                  "api_failed": self.last_error > self.last_success}
        if self.usage:
            result["usage"] = dict(self.usage)
        if self.cost is not None:
            result["estimated_cost_micros"] = int((self.cost * 1_000_000).quantize(
                decimal.Decimal(1), rounding=decimal.ROUND_HALF_UP))
        return result


class Receiver(http.server.HTTPServer):
    def __init__(self, measurements):
        self.measurements = measurements
        super().__init__(("127.0.0.1", 0), Handler)


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        if self.path not in ("/v1/metrics", "/v1/logs"):
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 4 * 1024 * 1024:
                raise ValueError("invalid payload size")
            payload = json.loads(self.rfile.read(length))
            if self.path == "/v1/metrics":
                self.server.measurements.metrics(payload)
            else:
                self.server.measurements.logs(payload)
        except (ValueError, TypeError, AttributeError):
            self.send_error(400)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args):
        return None


class Client:
    def __init__(self, base_url, agent):
        self.base_url = base_url.rstrip("/")
        parsed = urllib.parse.urlsplit(self.base_url)
        if (parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password
                or parsed.query or parsed.fragment or parsed.path):
            raise ValueError("Agbredo URL must be an HTTPS origin")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", agent):
            raise ValueError("invalid agent ID")
        self.path = "/api/agents/" + agent + "/observations"
        self.opener = urllib.request.build_opener(NoRedirect())

    def token(self):
        url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"]
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme != "https" or parsed.username or parsed.password:
            raise ValueError("invalid GitHub OIDC URL")
        url += ("&" if "?" in url else "?") + urllib.parse.urlencode({"audience": self.base_url})
        req = urllib.request.Request(url, headers={"Authorization": "Bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]})
        with self.opener.open(req, timeout=10) as response:
            return json.load(response)["value"]

    def send(self, report):
        # Exact same body/event identity on every retry. Only GitHub's short-lived
        # OIDC token authenticates this request. Never read a Claude secret here.
        body = json.dumps(sanitized(report), separators=(",", ":")).encode()
        for attempt in range(3):
            try:
                req = urllib.request.Request(self.base_url + self.path, data=body, headers={
                    "Authorization": "Bearer " + self.token(), "Content-Type": "application/json"})
                with self.opener.open(req, timeout=10) as response:
                    response.read(65536)
                return True
            except urllib.error.HTTPError as error:
                if error.code not in (429, 500, 502, 503, 504):
                    break
            except (OSError, KeyError, ValueError):
                pass
            if attempt < 2:
                time.sleep(2 ** attempt)
        print("::warning::Agbredo report unavailable; agent execution is unaffected. Retry the saved report.", file=sys.stderr)
        return False


def sanitized(value):
    """Validate the allowlist again at the network boundary, including replays."""
    if not isinstance(value, dict):
        raise ValueError("invalid report")
    required = {"github_run_id", "github_run_attempt", "source", "phase", "version", "duration_ms", "telemetry_supported",
                "telemetry_received", "api_errors", "api_failed", "rate_limit_failures"}
    optional = {"exit_code", "action_outcome", "usage", "estimated_cost_micros"}
    if not required <= value.keys() or value.keys() - required - optional:
        raise ValueError("unknown report fields")
    if (not isinstance(value["source"], str) or not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", value["source"])
            or not isinstance(value["version"], str) or not re.fullmatch(r"unknown|\d+\.\d+\.\d+", value["version"])
            or len(value["version"]) > 40 or value["phase"] not in ("started", "finished")):
        raise ValueError("invalid report identifiers")
    for key in ("telemetry_supported", "telemetry_received", "api_failed"):
        if type(value[key]) is not bool:
            raise ValueError("invalid report boolean")
    limits = {"github_run_id": 2**63 - 1, "github_run_attempt": 2**63 - 1,
              "duration_ms": 2**63 - 1, "api_errors": 1_000_000,
              "rate_limit_failures": 1_000_000, "exit_code": 255, "estimated_cost_micros": 1_000_000_000_000}
    for key, limit in limits.items():
        if key in value and (type(value[key]) is not int or not 0 <= value[key] <= limit):
            raise ValueError("invalid numeric report field")
    if value["github_run_id"] <= 0 or value["github_run_attempt"] <= 0:
        raise ValueError("missing GitHub workflow identity")
    if "action_outcome" in value and value["action_outcome"] not in ("success", "failure", "cancelled", "skipped"):
        raise ValueError("invalid action outcome")
    if "usage" in value:
        usage = value["usage"]
        if not isinstance(usage, dict) or usage.keys() - set(Measurements.TYPES.values()):
            raise ValueError("unknown usage fields")
        if any(type(n) is not int or not 0 <= n <= 1_000_000_000 for n in usage.values()):
            raise ValueError("invalid token measurement")
    return value


def probe(executable="claude"):
    """Verify installed CLI and native telemetry markers without a model call.

    npm launchers may not contain the markers; actual recognized OTLP samples
    then provide runtime evidence instead. Never assume a JSON stdout schema.
    """
    version, supported = "unknown", False
    try:
        result = subprocess.run([executable, "--version"], capture_output=True, text=True, timeout=10)
        match = re.fullmatch(r"(\d+\.\d+\.\d+) \(Claude Code\)\s*", result.stdout)
        if result.returncode == 0 and match:
            version = match[1]
        binary = shutil.which(executable)
        if binary:
            with open(Path(binary).resolve(), "rb") as f:
                with mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as b:
                    supported = all(b.find(term) >= 0 for term in (
                        b"CLAUDE_CODE_OAUTH_TOKEN", b"CLAUDE_CODE_ENABLE_TELEMETRY",
                        b"http/json", b"claude_code.token.usage", b"claude_code.cost.usage"))
    except (OSError, ValueError, subprocess.SubprocessError):
        pass
    return version, supported


def telemetry_env(port):
    # Keep existing authentication and command args. Override only telemetry
    # routing in this child process. Raw telemetry stays on loopback.
    env = os.environ.copy()
    for key in list(env):
        if key.startswith("OTEL_EXPORTER_OTLP_") or key in ("BETA_TRACING_ENDPOINT", "OTEL_RESOURCE_ATTRIBUTES"):
            env.pop(key)
    env.update({"CLAUDE_CODE_ENABLE_TELEMETRY": "1", "OTEL_METRICS_EXPORTER": "otlp",
                "OTEL_LOGS_EXPORTER": "otlp", "OTEL_TRACES_EXPORTER": "none",
                "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:%d" % port,
                "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta",
                "OTEL_METRIC_EXPORT_INTERVAL": "1000", "OTEL_LOGS_EXPORT_INTERVAL": "1000",
                "OTEL_LOG_USER_PROMPTS": "0", "OTEL_LOG_ASSISTANT_RESPONSES": "0",
                "OTEL_LOG_TOOL_DETAILS": "0", "OTEL_LOG_TOOL_CONTENT": "0",
                "OTEL_LOG_RAW_API_BODIES": "0", "OTEL_LOG_MANAGED_SETTINGS": "0",
                "OTEL_METRICS_INCLUDE_ACCOUNT_UUID": "false"})
    return env


def save_report(path, report):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    with open(temporary, "w", opener=lambda p, flags: os.open(p, flags, 0o600)) as f:
        json.dump(report, f)
    temporary.replace(path)


def run_command(command, source, output, client=None):
    measurements = Measurements()
    version, supported = probe()
    def github_number(name):
        value = os.environ.get(name, "")
        return int(value) if re.fullmatch(r"[0-9]{1,19}", value) else 0
    report = {"github_run_id": github_number("GITHUB_RUN_ID"),
              "github_run_attempt": github_number("GITHUB_RUN_ATTEMPT"),
              "source": source, "phase": "started", "version": version,
              "duration_ms": 0, "telemetry_supported": supported,
              "telemetry_received": False, "api_errors": 0, "rate_limit_failures": 0, "api_failed": False}
    # Begin reporting asynchronously so an unavailable control plane cannot
    # delay the existing agent's startup.
    if client:
        initial_report = dict(report)
        def started():
            try:
                client.send(initial_report)
            except ValueError:
                print("::warning::Missing GitHub reporting identity; agent execution is unaffected.", file=sys.stderr)
        threading.Thread(target=started, daemon=True).start()
    receiver = None
    try:
        receiver = Receiver(measurements)
        thread = threading.Thread(target=receiver.serve_forever, daemon=True)
        thread.start()
        env = telemetry_env(receiver.server_port)
    except OSError:
        env = os.environ.copy()
        supported = False
        print("::warning::Local telemetry receiver unavailable; running agent normally.", file=sys.stderr)
    start = time.monotonic()
    exit_code = 127
    previous_handlers = {}
    try:
        child = subprocess.Popen(command, env=env, start_new_session=True)
        def forward(signum, _frame):
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass
        for signum in (signal.SIGINT, signal.SIGTERM):
            previous_handlers[signum] = signal.signal(signum, forward)
        exit_code = child.wait()
        if exit_code < 0:
            exit_code = 128 - exit_code
    except OSError:
        print("::error::Agent command could not start.", file=sys.stderr)
    finally:
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)
        if receiver:
            # Claude flushes on normal exit; shutdown drains the active request.
            receiver.shutdown()
            receiver.server_close()
    report.update(measurements.report(), phase="finished", exit_code=exit_code,
                  duration_ms=int((time.monotonic() - start) * 1000),
                  telemetry_supported=supported or measurements.received)
    try:
        save_report(output, report)
    except OSError:
        print("::warning::Could not save Agbredo report; agent execution is unaffected.", file=sys.stderr)
    if client:
        try:
            client.send(report)
        except ValueError:
            print("::warning::Invalid telemetry measurements; agent execution is unaffected.", file=sys.stderr)
    return exit_code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=os.environ.get("AGBREDO_URL"))
    parser.add_argument("--agent-id", default=os.environ.get("AGBREDO_AGENT_ID"))
    parser.add_argument("--source", default="claude")
    parser.add_argument("--output", default="agbredo-report.json")
    parser.add_argument("--send", help="retry a previously saved sanitized report")
    parser.add_argument("--probe", action="store_true")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.probe:
        version, supported = probe()
        print(json.dumps({"version": version, "telemetry_supported": supported}))
        return 0
    client = None
    try:
        if args.url and args.agent_id:
            client = Client(args.url, args.agent_id)
    except ValueError:
        print("::warning::Invalid Agbredo reporting configuration; agent execution is unaffected.", file=sys.stderr)
    if args.send:
        if client is None:
            parser.error("--send requires --url and --agent-id")
        # Replaying requires the same GitHub run/attempt identity, while its OIDC
        # endpoint is still available; the Go service enforces that binding.
        with open(args.send) as f:
            report = json.load(f)
        try:
            return 0 if client.send(report) else 1
        except ValueError:
            print("::error::Saved report contains invalid or unknown fields; nothing was sent.", file=sys.stderr)
            return 1
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command or not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", args.source):
        parser.error("provide a stable source and a command after --")
    return run_command(command, args.source, args.output, client)


if __name__ == "__main__":
    sys.exit(main())
