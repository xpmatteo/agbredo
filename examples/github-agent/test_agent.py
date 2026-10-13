import os
import urllib.error
import unittest
from unittest.mock import MagicMock, patch

import agent


ENV = {
    "AGBREDO_URL": "https://agbredo.example.test",
    "AGBREDO_RUN_ID": "run-1",
    "AGBREDO_CORRELATION_ID": "correlation-1",
    "GITHUB_REPOSITORY": "owner/repo",
    "GITHUB_RUN_ID": "42",
    "GITHUB_RUN_ATTEMPT": "1",
    "AGBREDO_APPROVAL_ID": "approval-1",
    "AGBREDO_PAYLOAD_HASH": "immutable-hash",
}


class ExampleTests(unittest.TestCase):
    def setUp(self):
        self.environment = patch.dict(os.environ, ENV, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def test_initial_only_proposes_and_reports_real_zero_cost(self):
        client = agent.Client()
        with patch.object(client, "call", return_value={}) as call:
            agent.propose(client)
        self.assertEqual(call.call_count, 2)
        proposal = call.call_args_list[-1].args[2]
        self.assertEqual(proposal["status"], "waiting_approval")
        self.assertEqual(proposal["proposal"]["resource"], "owner/repo")
        self.assertEqual(proposal["cost_micros"], 0)
        self.assertEqual(proposal["input_tokens"], 0)
        self.assertTrue(all("/events" in item.args[1] for item in call.call_args_list))

    def test_continuation_rejects_modified_authorization(self):
        client = agent.Client()
        approval = {"id": "approval-1", "run_id": "run-1", "payload_hash": "changed"}
        with patch.object(client, "call", return_value=approval) as call:
            with self.assertRaises(agent.AgentError):
                agent.continue_approved(client)
        self.assertEqual(call.call_count, 1)

    def test_continuation_rejects_other_repository(self):
        client = agent.Client()
        approval = {"id": "approval-1", "run_id": "run-1", "payload_hash": "immutable-hash",
                    "proposal": {"action": "create_issue", "resource": "other/repo"}}
        with patch.object(client, "call", return_value=approval) as call:
            with self.assertRaises(agent.AgentError):
                agent.continue_approved(client)
        self.assertEqual(call.call_count, 1)

    def test_continuation_executes_exact_authorization_and_reports_result(self):
        client = agent.Client()
        approval = {"id": "approval-1", "run_id": "run-1", "payload_hash": "immutable-hash",
                    "proposal": {"action": "create_issue", "resource": "owner/repo"}}
        with patch.object(client, "call", side_effect=[approval,
                {"status": "executed", "result_url": "https://github.com/owner/repo/issues/1"}, {}]) as call:
            agent.continue_approved(client)
        execution = call.call_args_list[1]
        self.assertEqual(execution.args[0], "POST")
        self.assertEqual(execution.args[2], {"run_id": "run-1", "correlation_id": "correlation-1",
                                          "payload_hash": "immutable-hash"})
        self.assertEqual(call.call_count, 3)
        self.assertEqual(call.call_args_list[2].args[2]["approval_id"], "approval-1")
        self.assertEqual(call.call_args_list[2].args[2]["status"], "completed")

    def test_public_https_required(self):
        for url in ("http://localhost:8080", "https://user:password@example.test", "https://example.test?q=1"):
            with self.subTest(url=url), patch.dict(os.environ, {"AGBREDO_URL": url}):
                with self.assertRaises(agent.AgentError):
                    agent.Client()

    def test_transient_event_retry_preserves_exact_payload_and_refreshes_token(self):
        client = agent.Client()
        response = MagicMock()
        response.__enter__.return_value.read.return_value = b'{"accepted":true}'
        failure = urllib.error.HTTPError(client.base_url, 503, "Unavailable", {}, None)
        with patch.object(client.opener, "open", side_effect=[failure, response]) as opening, \
                patch.object(client, "token", side_effect=["short-lived-1", "short-lived-2"]) as token, \
                patch.object(agent.time, "sleep"):
            result = client.call("POST", "/api/runs/run-1/events", {"id": "stable", "cost_micros": 0})
        self.assertTrue(result["accepted"])
        self.assertEqual(token.call_count, 2)
        first, second = [item.args[0] for item in opening.call_args_list]
        self.assertEqual(first.data, second.data)
        self.assertNotEqual(first.headers["Authorization"], second.headers["Authorization"])

    def test_api_redirect_is_not_followed_with_bearer_credentials(self):
        handler = agent.NoRedirect()
        self.assertIsNone(handler.redirect_request(None, None, 302, "Found", {}, "https://other.test"))


if __name__ == "__main__":
    unittest.main()
