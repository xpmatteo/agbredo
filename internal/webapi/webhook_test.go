package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agbredo/internal/core"
	"agbredo/internal/forward"
)

// workflowRunBody is a workflow_run webhook payload for one run attempt.
func workflowRunBody(repository, workflow string, id int64, status, conclusion string) string {
	return fmt.Sprintf(`{"action":"completed","workflow_run":{"id":%d,"run_attempt":1,"path":".github/workflows/%s","status":%q,"conclusion":%q,
		"head_branch":"main","event":"schedule","run_started_at":"2026-10-15T07:08:00Z","updated_at":"2026-10-15T07:08:17Z","repository":{"full_name":%q}},"repository":{"full_name":%q},"installation":{"id":42}}`,
		id, workflow, status, conclusion, repository, repository)
}

// pullRequestBody is a pull_request webhook payload of a pull request that
// github-actions opened in repository (ID 7) at 2026-10-15 07:08:10 UTC.
func pullRequestBody(action, repository string, number int, body string) string {
	return fmt.Sprintf(`{"action":%q,"number":%d,"pull_request":{"number":%d,"title":"Audit fixes","body":%q,"created_at":"2026-10-15T07:08:10Z",
		"html_url":"https://github.com/%s/pull/%d","user":{"login":"github-actions[bot]","type":"Bot"},
		"head":{"ref":"audit","repo":{"id":7,"full_name":%q}},"base":{"ref":"main","repo":{"id":7,"full_name":%q}}},
		"repository":{"id":7,"full_name":%q},"installation":{"id":42}}`, action, number, number, body, repository, number, repository, repository, repository)
}

// archived is one webhook delivery as the telemetry store received it.
type archived struct {
	path, stream, delivery, body string
}

// Every delivery with a good signature is archived and indexed, whatever its
// event. One with a bad signature is neither. A failed one is processed again
// when GitHub sends it again.
func TestDatabaseWebhookDeliveriesAreArchivedAndIndexed(t *testing.T) {
	ctx := context.Background()
	service := databaseService(t, gitHub{run: core.WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "in_progress"}})
	received := make(chan archived, 20)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []struct {
						Body       struct{ StringValue string } `json:"body"`
						Attributes []struct {
							Key   string            `json:"key"`
							Value map[string]string `json:"value"`
						} `json:"attributes"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.Unmarshal(body, &doc); err != nil || len(doc.ResourceLogs) != 1 {
			t.Errorf("store received %s", body)
			return
		}
		record := doc.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		got := archived{path: r.URL.Path, stream: r.Header.Get("stream-name"), body: record.Body.StringValue}
		for _, a := range record.Attributes {
			if a.Key == "github.delivery" {
				got.delivery = a.Value["stringValue"]
			}
		}
		received <- got
	}))
	defer store.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := testAPI(&fakeVerifier{identity: core.Identity{Repository: "acme/agent", Workflow: "agent.yml", Ref: "refs/heads/main", RunID: 777, RunAttempt: 1, EventName: "push"}})
	a.Service = service
	a.Forward = forward.New(store.URL+"/api/default", "dXNlcjpwYXNz", a.Logger)
	go a.Forward.Run(runCtx)

	deliver := func(event, delivery, body string) int {
		return requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signWebhook(a.WebhookSecret, body), "X-GitHub-Event": event, "X-GitHub-Delivery": delivery}).Code
	}
	type row struct{ event, action, repository, outcome, problem *string }
	index := func(id string) (row, bool) {
		var r row
		err := service.DB.QueryRow(ctx, "SELECT event, action, repository, outcome, error FROM webhook_deliveries WHERE id=$1", id).Scan(&r.event, &r.action, &r.repository, &r.outcome, &r.problem)
		return r, err == nil
	}
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	expectOutcome := func(id, outcome, problem string) {
		t.Helper()
		r, ok := index(id)
		if !ok || text(r.outcome) != outcome || text(r.problem) != problem {
			t.Fatalf("%s: indexed %v, outcome %q, error %q", id, ok, text(r.outcome), text(r.problem))
		}
	}

	// A bad signature: refused, not archived, not indexed.
	ping := `{"zen":"Keep it logically awesome.","hook_id":1}`
	if code := requestAPI(a, http.MethodPost, "/api/github/webhook", ping, map[string]string{"X-Hub-Signature-256": signWebhook("another-secret", ping), "X-GitHub-Event": "ping", "X-GitHub-Delivery": "forged"}).Code; code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d", code)
	}
	if _, ok := index("forged"); ok {
		t.Fatal("a delivery with a bad signature was indexed")
	}

	// Events Agbredo does not act on: ignored, with what the payload says.
	if code := deliver("ping", "ping-1", ping); code != http.StatusNoContent {
		t.Fatalf("ping: status %d", code)
	}
	expectOutcome("ping-1", core.DeliveryIgnored, "")
	pr := `{"action":"closed","number":13,"repository":{"full_name":"acme/agent"},"installation":{"id":42},"sender":{"login":"agbredo-bot[bot]"}}`
	if code := deliver("pull_request", "pr-1", pr); code != http.StatusNoContent {
		t.Fatalf("pull_request: status %d", code)
	}
	expectOutcome("pr-1", core.DeliveryIgnored, "")
	var installation int64
	if r, _ := index("pr-1"); text(r.event) != "pull_request" || text(r.action) != "closed" || text(r.repository) != "acme/agent" ||
		service.DB.QueryRow(ctx, "SELECT installation_id FROM webhook_deliveries WHERE id='pr-1'").Scan(&installation) != nil || installation != 42 {
		t.Fatalf("pull_request row: %q %q %q %d", text(r.event), text(r.action), text(r.repository), installation)
	}

	// A workflow with no deployment: ignored. A bad workflow_run payload: failed.
	if code := deliver("workflow_run", "wr-unknown", workflowRunBody("acme/agent", "other.yml", 999, "completed", "success")); code != http.StatusNoContent {
		t.Fatalf("unknown workflow: status %d", code)
	}
	expectOutcome("wr-unknown", core.DeliveryIgnored, "")
	for _, body := range []string{`not-json`, `{}`, `{"workflow_run":{"id":0}}`, pr} {
		if code := deliver("workflow_run", "wr-bad", body); code != http.StatusBadRequest {
			t.Fatalf("invalid workflow payload %s: status %d", body, code)
		}
	}
	expectOutcome("wr-bad", core.DeliveryFailed, "invalid workflow_run payload")

	// The deployment exists once a job asked for a token. While the database
	// fails: failed. GitHub sends it again: handled.
	if code, _ := postToken(t, a, "/api/otlp-token"); code != http.StatusOK {
		t.Fatalf("token: status %d", code)
	}
	run := workflowRunBody("acme/agent", "agent.yml", 777, "completed", "success")
	if _, err := service.DB.Exec(ctx, `CREATE FUNCTION broken_runs() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'runs are broken'; END; $$;
		CREATE TRIGGER broken_runs BEFORE INSERT OR UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION broken_runs()`); err != nil {
		t.Fatal(err)
	}
	if code := deliver("workflow_run", "wr-1", run); code != http.StatusInternalServerError {
		t.Fatalf("database fails: status %d", code)
	}
	if r, _ := index("wr-1"); text(r.outcome) != core.DeliveryFailed || !strings.Contains(text(r.problem), "runs are broken") {
		t.Fatalf("wr-1: outcome %q, error %q", text(r.outcome), text(r.problem))
	}
	if _, err := service.DB.Exec(ctx, "DROP TRIGGER broken_runs ON runs; DROP FUNCTION broken_runs()"); err != nil {
		t.Fatal(err)
	}
	if code := deliver("workflow_run", "wr-1", run); code != http.StatusNoContent {
		t.Fatalf("redelivery: status %d", code)
	}
	expectOutcome("wr-1", core.DeliveryHandled, "")
	// A run of the deployment that asked for no token is recorded too.
	if code := deliver("workflow_run", "wr-2", workflowRunBody("acme/agent", "agent.yml", 778, "completed", "skipped")); code != http.StatusNoContent {
		t.Fatalf("run without telemetry: status %d", code)
	}
	expectOutcome("wr-2", core.DeliveryHandled, "")
	fleet, err := service.Fleet(ctx)
	if err != nil || len(fleet.Runs) != 2 {
		t.Fatalf("runs: %+v %v", fleet.Runs, err)
	}
	for _, r := range fleet.Runs {
		want := map[int64]string{777: "completed", 778: "completed"}[r.GitHubRunID] // skipped is not a failure
		if r.Status != want || r.HeadBranch != "main" {
			t.Fatalf("run %d: status %s, head branch %q", r.GitHubRunID, r.Status, r.HeadBranch)
		}
	}

	// A pull request a bot opened in the repository: recorded and linked to
	// the run its trailer names. One without the shape of a pull_request
	// event: failed.
	opened := pullRequestBody("opened", "acme/agent", 14, "Fixes.\n\nAgbredo-Run: 778/1")
	if code := deliver("pull_request", "pr-2", opened); code != http.StatusNoContent {
		t.Fatalf("opened pull request: status %d", code)
	}
	expectOutcome("pr-2", core.DeliveryHandled, "")
	var link string
	if err := service.DB.QueryRow(ctx, "SELECT link FROM pull_requests WHERE id='7:14'").Scan(&link); err != nil || link != core.LinkDeclared {
		t.Fatalf("pull request: %q %v", link, err)
	}
	for _, body := range []string{`{"action":"opened"}`, strings.Replace(opened, `"created_at"`, `"updated_at"`, 1), workflowRunBody("acme/agent", "agent.yml", 778, "completed", "success")} {
		if code := deliver("pull_request", "pr-bad", strings.Replace(body, `"action":"completed"`, `"action":"opened"`, 1)); code != http.StatusBadRequest {
			t.Fatalf("invalid pull_request payload %s: status %d", body, code)
		}
	}
	expectOutcome("pr-bad", core.DeliveryFailed, "invalid pull_request payload")

	// Every verified delivery reached the store, in its own stream, unchanged.
	want := []string{"ping-1", "pr-1", "wr-unknown", "wr-bad", "wr-bad", "wr-bad", "wr-bad", "wr-1", "wr-1", "wr-2", "pr-2", "pr-bad", "pr-bad", "pr-bad"}
	for n, delivery := range want {
		select {
		case got := <-received:
			if got.delivery != delivery || got.path != "/api/default/v1/logs" || got.stream != forward.WebhookStream {
				t.Fatalf("archive %d: %+v, want delivery %s", n, got, delivery)
			}
			if delivery == "pr-1" && got.body != pr {
				t.Fatalf("body changed: %s", got.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("delivery %s not archived", delivery)
		}
	}
	select {
	case got := <-received:
		t.Fatalf("archived more than the verified deliveries: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}
