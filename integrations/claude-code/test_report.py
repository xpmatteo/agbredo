import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
import urllib.error

import report


def attrs(**values):
    return [{"key": key, "value": {"stringValue": str(value)}} for key, value in values.items()]


def metric(name, value, kind="input", end=2, start=1, temporality=1, model="claude-test"):
    return {"resourceMetrics": [{"resource": {"attributes": attrs(**{"session.id": "session"})},
        "scopeMetrics": [{"scope": {"name": "claude-code"}, "metrics": [{"name": name,
            "sum": {"isMonotonic": True, "aggregationTemporality": temporality, "dataPoints": [{
                "asDouble": value, "startTimeUnixNano": str(start), "timeUnixNano": str(end),
                "attributes": attrs(type=kind, model=model)}]}}]}]}]}


def log(name="api_error", end=3, code="429", error="private content"):
    return {"resourceLogs": [{"scopeLogs": [{"logRecords": [{"timeUnixNano": str(end),
        "body": {"stringValue": "claude_code." + name}, "attributes": attrs(**{
            "event.name": name, "status_code": code, "error": error, "user.email": "private@example.com"})}]}]}]}


class MeasurementsTests(unittest.TestCase):
    def test_missing_is_not_zero_and_delta_retries_do_not_double_count(self):
        m = report.Measurements()
        self.assertNotIn("usage", m.report())
        self.assertNotIn("estimated_cost_micros", m.report())
        p = metric("claude_code.token.usage", 100)
        m.metrics(p); m.metrics(p)
        m.metrics(metric("claude_code.token.usage", 25, end=4))
        m.metrics(metric("claude_code.token.usage", 30, kind="cacheRead"))
        m.metrics(metric("claude_code.token.usage", 10, kind="cacheCreation"))
        m.metrics(metric("claude_code.token.usage", 0, kind="output"))
        # Aggregate multiple models/sources and round only after summing cost.
        m.metrics(metric("claude_code.cost.usage", 0.0000004))
        m.metrics(metric("claude_code.cost.usage", 0.0000004, model="second"))
        result = m.report()
        self.assertEqual(result["usage"], {"input_tokens": 125, "output_tokens": 0,
                                         "cache_read_tokens": 30, "cache_creation_tokens": 10})
        self.assertEqual(result["estimated_cost_micros"], 1)

    def test_cumulative_counters_resets_and_out_of_order_exports(self):
        m = report.Measurements()
        for amount, end in [(10, 2), (15, 3), (15, 3), (10, 2)]:
            m.metrics(metric("claude_code.token.usage", amount, end=end, temporality=2))
        m.metrics(metric("claude_code.token.usage", 2, start=4, end=5, temporality=2))
        self.assertEqual(m.report()["usage"]["input_tokens"], 17)

    def test_unknown_and_invalid_fields_cannot_fabricate_usage(self):
        m = report.Measurements()
        m.metrics(metric("some.other.counter", 100))
        m.metrics(metric("claude_code.token.usage", 100, kind="unknown"))
        for bad in (-1, float("nan"), float("inf"), 0.25, True, 1_000_000_001):
            m.metrics(metric("claude_code.token.usage", bad))
        m.metrics(metric("claude_code.cost.usage", -1))
        self.assertNotIn("usage", m.report())
        self.assertNotIn("estimated_cost_micros", m.report())

    def test_rate_limit_failure_deduplication_and_recovery_without_raw_content(self):
        m = report.Measurements()
        p = log(error="CLAUDE_CODE_OAUTH_TOKEN=never-forward")
        m.logs(p); m.logs(p)
        self.assertEqual(m.report()["rate_limit_failures"], 1)
        self.assertTrue(m.report()["api_failed"])
        m.logs(log("api_request", end=4))
        self.assertFalse(m.report()["api_failed"])
        m.logs(log(end=5, code="none", error="You've hit your limit"))
        self.assertEqual(m.report()["rate_limit_failures"], 2)
        serialized = json.dumps(m.report()) + repr(m.__dict__)
        for private in ("never-forward", "private@example.com", "hit your limit"):
            self.assertNotIn(private, serialized)

    def test_transport_retries_exact_report_and_refreshes_only_github_oidc(self):
        client = report.Client("https://agbredo.example", "agent-id")
        response = unittest.mock.MagicMock()
        response.__enter__.return_value.read.return_value = b"{}"
        transient = urllib.error.HTTPError("https://agbredo.example", 503, "unavailable", {}, None)
        payload = {"github_run_id": 123, "github_run_attempt": 1,
                   "source": "agent", "phase": "finished", "version": "2.1.296", "exit_code": 0,
                   "duration_ms": 10, "telemetry_supported": True, **report.Measurements().report()}
        with patch.object(client, "token", side_effect=["oidc-1", "oidc-2"]) as token, \
                patch.object(client.opener, "open", side_effect=[transient, response]) as send, \
                patch("report.time.sleep"), patch.dict(os.environ, {"CLAUDE_CODE_OAUTH_TOKEN": "never-forward"}):
            self.assertTrue(client.send(payload))
            self.assertEqual(token.call_count, 2)
            first, second = [call.args[0] for call in send.call_args_list]
            self.assertEqual(first.data, second.data)
            self.assertNotIn(b"never-forward", first.data)
            self.assertEqual(second.get_header("Authorization"), "Bearer oidc-2")

    def test_replayed_opaque_data_or_credentials_never_reach_transport(self):
        client = report.Client("https://agbredo.example", "agent-id")
        with patch.object(client, "token") as token, patch.object(client.opener, "open") as send:
            with self.assertRaises(ValueError):
                client.send({"CLAUDE_CODE_OAUTH_TOKEN": "secret"})
            token.assert_not_called()
            send.assert_not_called()


class ExecutionTests(unittest.TestCase):
    def test_existing_command_authentication_output_and_exit_are_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory, "existing.py")
            marker, output = Path(directory, "ran"), Path(directory, "report.json")
            # This existing workflow command uses only the provided secret. The
            # secret must reach it, and must not reach the saved/network report.
            script.write_text("import os, pathlib, sys\n"
                              "assert os.environ['CLAUDE_CODE_OAUTH_TOKEN'] == 'original-subscription-secret'\n"
                              "pathlib.Path(sys.argv[1]).write_text('existing command ran')\n"
                              "print('existing stdout')\n"
                              "sys.exit(7)\n")
            with patch("report.probe", return_value=("2.1.296", True)), \
                    patch.dict(os.environ, {"CLAUDE_CODE_OAUTH_TOKEN": "original-subscription-secret"}):
                self.assertEqual(report.run_command([sys.executable, str(script), str(marker)], "job", output), 7)
            self.assertEqual(marker.read_text(), "existing command ran")
            saved = json.loads(output.read_text())
            self.assertEqual(saved["exit_code"], 7)
            self.assertNotIn("usage", saved)
            self.assertNotIn("original-subscription-secret", output.read_text())

    def test_local_otlp_export_is_collected_during_real_process(self):
        with tempfile.TemporaryDirectory() as directory:
            script, output = Path(directory, "export.py"), Path(directory, "report.json")
            payload = metric("claude_code.token.usage", 19)
            script.write_text("import json, os, urllib.request\n"
                "payload = " + repr(payload) + "\n"
                "r = urllib.request.Request(os.environ['OTEL_EXPORTER_OTLP_ENDPOINT']+'/v1/metrics', "
                "data=json.dumps(payload).encode(), headers={'Content-Type':'application/json'})\n"
                "urllib.request.urlopen(r).read()\n")
            with patch("report.probe", return_value=("unknown", False)):
                self.assertEqual(report.run_command([sys.executable, str(script)], "job", output), 0)
            saved = json.loads(output.read_text())
            self.assertEqual(saved["usage"]["input_tokens"], 19)
            self.assertTrue(saved["telemetry_supported"])
            self.assertTrue(saved["telemetry_received"])

    def test_reporting_outage_does_not_change_process_result(self):
        client = unittest.mock.Mock()
        client.send.return_value = False
        with tempfile.TemporaryDirectory() as directory, patch("report.probe", return_value=("unknown", False)):
            output = Path(directory, "report.json")
            self.assertEqual(report.run_command([sys.executable, "-c", "pass"], "job", output, client), 0)
            self.assertTrue(output.exists())

    def test_cancellation_is_forwarded_and_final_report_is_saved(self):
        with tempfile.TemporaryDirectory() as directory:
            marker, output = Path(directory, "running"), Path(directory, "report.json")
            script = "import pathlib, time; pathlib.Path(" + repr(str(marker)) + ").touch(); time.sleep(20)"
            child = subprocess.Popen([sys.executable, str(Path(report.__file__)), "--output", str(output),
                                      "--", sys.executable, "-c", script])
            try:
                deadline = time.monotonic() + 10
                while not marker.exists() and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue(marker.exists())
                child.send_signal(signal.SIGTERM)
                self.assertEqual(child.wait(timeout=10), 143)
                self.assertEqual(json.loads(output.read_text())["exit_code"], 143)
            finally:
                if child.poll() is None:
                    child.kill()
                    child.wait()


if __name__ == "__main__":
    unittest.main()
