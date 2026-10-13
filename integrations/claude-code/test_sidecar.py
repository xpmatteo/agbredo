import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import urllib.request
from unittest.mock import patch

import report
import sidecar
from test_report import metric


class SidecarTests(unittest.TestCase):
    def test_existing_action_can_export_between_steps_and_finalize_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            state, output, environment = (Path(directory, name) for name in ("state.json", "report.json", "github-env"))
            env = os.environ.copy()
            env.update(GITHUB_RUN_ID="987", GITHUB_RUN_ATTEMPT="2", GITHUB_ENV=str(environment),
                       CLAUDE_CODE_OAUTH_TOKEN="never-forward", OTEL_RESOURCE_ATTRIBUTES="old=value")
            env.pop("AGBREDO_URL", None)
            env.pop("AGBREDO_AGENT_ID", None)
            command = [sys.executable, str(Path(sidecar.__file__)), "start", "--state", str(state), "--source", "audit"]
            subprocess.run(command, env=env, check=True, timeout=10)
            saved = json.loads(state.read_text())
            self.assertNotIn("never-forward", state.read_text())
            self.assertIn("CLAUDE_CODE_ENABLE_TELEMETRY", environment.read_text())
            self.assertEqual(saved["settings"]["OTEL_RESOURCE_ATTRIBUTES"], "")
            self.assertEqual(saved["restore"]["OTEL_RESOURCE_ATTRIBUTES"], "old=value")
            token = saved["control_token"]
            try:
                request = urllib.request.Request(saved["endpoint"] + "/v1/metrics",
                    data=json.dumps(metric("claude_code.token.usage", 27)).encode(),
                    headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(request, timeout=5) as response:
                    self.assertEqual(response.status, 200)
                command[2] = "finish"
                command += ["--output", str(output), "--action-outcome", "failure"]
                subprocess.run(command, env=env, check=True, timeout=20)
                result = json.loads(output.read_text())
                self.assertEqual(result["action_outcome"], "failure")
                self.assertNotIn("exit_code", result)
                self.assertEqual(result["usage"]["input_tokens"], 27)
                self.assertEqual(result["github_run_id"], 987)
                self.assertEqual(result["github_run_attempt"], 2)
                for secret in ("never-forward", token, "old=value"):
                    self.assertNotIn(secret, output.read_text())
                report.sanitized(result)
                self.assertIn("old=value", environment.read_text())
            finally:
                # If an assertion fails before finalization, stop the daemon so
                # this test doesn't leave a background collector behind.
                try:
                    request = urllib.request.Request(saved["endpoint"] + "/finish",
                        data=json.dumps({"outcome": "cancelled", "version": "unknown", "supported": False}).encode(),
                        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
                    urllib.request.urlopen(request, timeout=2).close()
                except OSError:
                    pass

    def test_action_outcome_is_validated_without_inventing_exit_codes(self):
        base = {"github_run_id": 987, "github_run_attempt": 2, "source": "audit", "phase": "finished",
                "version": "unknown", "duration_ms": 1, "telemetry_supported": False, **report.Measurements().report()}
        for outcome in ("success", "failure", "cancelled", "skipped"):
            report.sanitized({**base, "action_outcome": outcome})
        with self.assertRaises(ValueError):
            report.sanitized({**base, "action_outcome": "secret=value"})


if __name__ == "__main__":
    unittest.main()
