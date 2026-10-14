import io
import json
import unittest

import agent


class FakeClient:
    def __init__(self):
        self.sent = []

    def send(self, report):
        self.sent.append(report)


class ScenarioTests(unittest.TestCase):
    def play(self, name):
        client, out = FakeClient(), io.StringIO()
        agent.play(name, client, 42, 1, pause=0, out=out)
        return client.sent, out.getvalue()

    def test_every_act_sends_started_then_finished_with_the_run_identity(self):
        for name, acts in agent.SCENARIOS.items():
            sent, _ = self.play(name)
            self.assertEqual([(r["source"], r["phase"]) for r in sent],
                             [(a["source"], p) for a in acts for p in ("started", "finished")])
            self.assertTrue(all(r["github_run_id"] == 42 and r["github_run_attempt"] == 1 for r in sent))

    def test_started_reports_carry_no_results(self):
        for name in agent.SCENARIOS:
            for r in self.play(name)[0]:
                if r["phase"] == "started":
                    self.assertNotIn("exit_code", r)
                    self.assertNotIn("usage", r)
                    self.assertEqual(r["duration_ms"], 0)

    def test_hello_says_hi_and_measures_a_true_zero(self):
        sent, out = self.play("hello")
        self.assertIn("hi", out)
        self.assertEqual(sent[1]["usage"]["input_tokens"], 0)
        self.assertEqual(sent[1]["estimated_cost_micros"], 0)

    def test_silent_agent_reports_no_measurements(self):
        finished = self.play("silent")[0][1]
        self.assertFalse(finished["telemetry_received"])
        self.assertNotIn("usage", finished)
        self.assertNotIn("estimated_cost_micros", finished)

    def test_rate_limited_agent_follows_the_report_rules(self):
        finished = self.play("rate-limited")[0][1]
        self.assertEqual(finished["exit_code"], 1)
        self.assertTrue(finished["api_failed"])
        self.assertLessEqual(finished["rate_limit_failures"], finished["api_errors"])

    def test_finished_reports_stay_within_the_server_limits(self):
        for name in agent.SCENARIOS:
            for r in self.play(name)[0]:
                if r["phase"] == "finished":
                    self.assertTrue(0 <= r["exit_code"] <= 255)
                    self.assertLessEqual(r.get("estimated_cost_micros", 0), 1_000_000_000_000)

    def test_without_a_url_reports_are_printed(self):
        out = io.StringIO()
        agent.play("hello", None, 1, 1, pause=0, out=out)
        self.assertEqual(out.getvalue().count('"phase"'), 2)

    def test_client_rejects_a_bad_agent_id(self):
        with self.assertRaises(ValueError):
            agent.Client("https://agbredo.example", "../x")


class OtlpTests(unittest.TestCase):
    def test_without_an_endpoint_it_prints_instead_of_sending(self):
        out = io.StringIO()
        agent.play_otlp(env={}, out=out)
        self.assertIn("/v1/metrics", out.getvalue())
        self.assertIn("/v1/logs", out.getvalue())

    def test_it_sends_json_with_the_token_and_repeats_one_export(self):
        sent = []

        class Response:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, n):
                return b"{}"

        def post(request, timeout):
            sent.append((request.full_url, request.get_header("Authorization"), request.get_header("Content-type")))
            return Response()

        agent.play_otlp(env={"OTEL_EXPORTER_OTLP_ENDPOINT": "https://x.example/",
                             "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer agb_1"}, out=io.StringIO(), post=post)
        self.assertEqual([u for u, _, _ in sent], ["https://x.example/v1/metrics"] * 2 + ["https://x.example/v1/logs"])
        self.assertTrue(all(a == "Bearer agb_1" and c == "application/json" for _, a, c in sent))

    def test_the_payload_has_no_prompt_or_identity(self):
        text = json.dumps(agent.otlp_payloads())
        for word in ("prompt", "email", "account"):
            self.assertNotIn(word, text)


if __name__ == "__main__":
    unittest.main()
