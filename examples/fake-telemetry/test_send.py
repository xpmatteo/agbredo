import io
import json
import unittest

import send


class SendTests(unittest.TestCase):
    def test_without_an_endpoint_it_prints_instead_of_sending(self):
        out = io.StringIO()
        send.send(env={}, out=out)
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

        send.send(env={"OTEL_EXPORTER_OTLP_ENDPOINT": "https://x.example/",
                       "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer agb_1"}, out=io.StringIO(), post=post)
        self.assertEqual([u for u, _, _ in sent], ["https://x.example/v1/metrics"] * 2 + ["https://x.example/v1/logs"])
        self.assertTrue(all(a == "Bearer agb_1" and c == "application/json" for _, a, c in sent))

    def test_the_payload_has_no_prompt_or_identity(self):
        text = json.dumps(send.otlp_payloads())
        for word in ("prompt", "email", "account"):
            self.assertNotIn(word, text)


if __name__ == "__main__":
    unittest.main()
