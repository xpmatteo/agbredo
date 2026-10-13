#!/usr/bin/env python3
"""Deterministic GitHub Actions agent; Python standard library only."""

import datetime
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


class AgentError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        # Bearer credentials must never follow an API redirect to another host.
        return None


def required(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise AgentError("Missing environment variable: " + name)
    return value


class Client:
    def __init__(self):
        self.base_url = required("RILIAN_URL").rstrip("/")
        parsed = urllib.parse.urlsplit(self.base_url)
        if (parsed.scheme != "https" or not parsed.netloc or parsed.query or parsed.fragment
                or parsed.username or parsed.password):
            raise AgentError("RILIAN_URL must be a public HTTPS URL without a query or fragment")
        self.run_id = required("RILIAN_RUN_ID")
        self.correlation_id = required("RILIAN_CORRELATION_ID")
        self.opener = urllib.request.build_opener(NoRedirect())

    def token(self):
        # GitHub issues a short-lived OIDC token bound to this workflow execution.
        url = required("ACTIONS_ID_TOKEN_REQUEST_URL")
        separator = "&" if "?" in url else "?"
        url += separator + urllib.parse.urlencode({"audience": self.base_url})
        request = urllib.request.Request(url, headers={
            "Authorization": "Bearer " + required("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
        })
        with urllib.request.urlopen(request, timeout=20) as response:
            token = json.load(response).get("value")
        if not token:
            raise AgentError("GitHub did not return an OIDC token")
        return token

    def call(self, method, path, payload=None):
        # Reuse the exact payload across retries. Rilian deduplicates event IDs
        # and serializes execution of each immutable approved proposal.
        body = None if payload is None else json.dumps(payload, separators=(",", ":")).encode()
        for attempt in range(4):
            try:
                request = urllib.request.Request(self.base_url + path, data=body, method=method,
                    headers={"Authorization": "Bearer " + self.token(),
                             "Content-Type": "application/json", "Accept": "application/json"})
                with self.opener.open(request, timeout=60) as response:
                    raw = response.read()
                    return json.loads(raw) if raw else {}
            except urllib.error.HTTPError as error:
                # Do not print response bodies, credentials or headers into runner logs.
                if error.code not in (429, 500, 502, 503, 504) or attempt == 3:
                    raise AgentError("Rilian API rejected request (HTTP %d)" % error.code) from None
            except (urllib.error.URLError, TimeoutError):
                if attempt == 3:
                    raise AgentError("Rilian API could not be reached after four attempts") from None
            time.sleep(2 ** attempt)
        raise AgentError("Rilian API request failed")

    def event(self, phase, event_type, status="", message="", proposal=None, approval_id=None):
        event = {
            "id": "github:%s:%s:%s" % (
                required("GITHUB_RUN_ID"), required("GITHUB_RUN_ATTEMPT"), phase),
            "run_id": self.run_id,
            "correlation_id": self.correlation_id,
            "timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "type": event_type, "status": status, "message": message,
            "input_tokens": 0, "output_tokens": 0, "cost_micros": 0,
        }
        if proposal is not None:
            event.update(proposal=proposal, action=proposal["action"], resource=proposal["resource"])
        if approval_id is not None:
            event["approval_id"] = approval_id
        return self.call("POST", "/api/runs/" + urllib.parse.quote(self.run_id, safe="") + "/events", event)


def propose(client):
    client.event("started", "execution.started", "running", "Deterministic example agent started")
    proposal = {
        "action": "create_issue",
        "resource": required("GITHUB_REPOSITORY"),
        "title": "Rilian approved agent action " + client.correlation_id,
        "body": "This issue was proposed by the Rilian example agent.\n\n"
                "Run: " + client.run_id + "\nCorrelation: " + client.correlation_id + "\n\n"
                "No model was called. Reported tokens and model cost are zero.",
    }
    client.event("proposal", "approval.proposed", "waiting_approval",
                 "Proposed creation of one GitHub issue; workflow exits for human review", proposal)
    print("Proposal recorded. The workflow ends here; no issue has been created.")


def continue_approved(client):
    approval_id = required("RILIAN_APPROVAL_ID")
    expected_hash = required("RILIAN_PAYLOAD_HASH")
    path = "/api/approvals/" + urllib.parse.quote(approval_id, safe="")
    approval = client.call("GET", path)
    if (approval.get("id") != approval_id or approval.get("run_id") != client.run_id
            or approval.get("payload_hash") != expected_hash):
        raise AgentError("Approval does not match the dispatched authorization")
    proposal = approval.get("proposal", {})
    if proposal.get("action") != "create_issue" or proposal.get("resource") != required("GITHUB_REPOSITORY"):
        raise AgentError("Approved action or repository does not match this agent")
    # The server rechecks expiry, status, payload hash, workflow identity and
    # budget in a transaction, then creates exactly this proposal via the App.
    result = client.call("POST", path + "/execute", {
        "run_id": client.run_id, "correlation_id": client.correlation_id,
        "payload_hash": expected_hash,
    })
    if result.get("status") != "executed" or not result.get("result_url"):
        raise AgentError("Approved action has not completed; inspect its audit trail in Rilian")
    # /execute persists the authoritative result. This event adds runner logs;
    # continuation events cannot independently change the action's final state.
    client.event("completed:" + approval_id, "execution.completed", "completed",
                 "Approved issue creation completed", approval_id=approval_id)
    print("Approved issue creation completed. Inspect the result in Rilian.")


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("propose", "continue"):
        raise AgentError("Usage: python3 agent.py propose|continue")
    client = Client()
    try:
        if sys.argv[1] == "propose":
            propose(client)
        else:
            continue_approved(client)
    except Exception:
        try:
            if sys.argv[1] == "propose":
                client.event("error:propose", "execution.error", "failed",
                             "Example workflow failed; inspect workflow logs and the approval audit trail")
            else:
                # An external result may be ambiguous; server reconciliation
                # retains control of the execution state while this records logs.
                client.event("error:continue", "execution.error", "failed",
                             "Continuation failed; inspect the approval audit trail",
                             approval_id=required("RILIAN_APPROVAL_ID"))
        except Exception:
            pass
        raise


if __name__ == "__main__":
    try:
        main()
    except AgentError as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
    except Exception:
        print("Example agent failed unexpectedly; no credentials were logged", file=sys.stderr)
        sys.exit(1)
