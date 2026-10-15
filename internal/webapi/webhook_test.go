package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agbredo/internal/core"
	"agbredo/internal/forward"
)

// changingGitHub is a GitHub whose run, and whether it answers, can change.
type changingGitHub struct {
	mu   sync.Mutex
	run  core.WorkflowRun
	down bool
}

func (g *changingGitHub) GetRun(context.Context, core.Deployment, int64) (core.WorkflowRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return core.WorkflowRun{}, errors.New("GitHub is down")
	}
	return g.run, nil
}
func (g *changingGitHub) GetRunAttempt(ctx context.Context, d core.Deployment, id, _ int64) (core.WorkflowRun, error) {
	return g.GetRun(ctx, d, id)
}
func (g *changingGitHub) Installation(ctx context.Context, owner, repository string) (int64, error) {
	return gitHub{}.Installation(ctx, owner, repository)
}
func (g *changingGitHub) set(run core.WorkflowRun, down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.run, g.down = run, down
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
	github := &changingGitHub{run: core.WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "in_progress"}}
	service := databaseService(t, github)
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
	pr := `{"action":"opened","number":13,"repository":{"full_name":"acme/agent"},"installation":{"id":42},"sender":{"login":"agbredo-bot[bot]"}}`
	if code := deliver("pull_request", "pr-1", pr); code != http.StatusNoContent {
		t.Fatalf("pull_request: status %d", code)
	}
	expectOutcome("pr-1", core.DeliveryIgnored, "")
	var installation int64
	if r, _ := index("pr-1"); text(r.event) != "pull_request" || text(r.action) != "opened" || text(r.repository) != "acme/agent" ||
		service.DB.QueryRow(ctx, "SELECT installation_id FROM webhook_deliveries WHERE id='pr-1'").Scan(&installation) != nil || installation != 42 {
		t.Fatalf("pull_request row: %q %q %q %d", text(r.event), text(r.action), text(r.repository), installation)
	}

	// A run Agbredo does not know: ignored. A bad workflow_run payload: failed.
	if code := deliver("workflow_run", "wr-unknown", `{"action":"completed","repository":{"full_name":"acme/agent"},"workflow_run":{"id":999}}`); code != http.StatusNoContent {
		t.Fatalf("unknown run: status %d", code)
	}
	expectOutcome("wr-unknown", core.DeliveryIgnored, "")
	for _, body := range []string{`not-json`, `{}`, `{"workflow_run":{"id":0}}`} {
		if code := deliver("workflow_run", "wr-bad", body); code != http.StatusBadRequest {
			t.Fatalf("invalid workflow payload %s: status %d", body, code)
		}
	}
	expectOutcome("wr-bad", core.DeliveryFailed, "invalid workflow_run payload")

	// A known run, while GitHub is down: failed. GitHub sends it again: handled.
	if code, _ := postToken(t, a, "/api/otlp-token"); code != http.StatusOK {
		t.Fatalf("token: status %d", code)
	}
	completed := core.WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "completed", Conclusion: "success"}
	github.set(completed, true)
	run := `{"action":"completed","repository":{"full_name":"acme/agent"},"installation":{"id":42},"workflow_run":{"id":777}}`
	if code := deliver("workflow_run", "wr-1", run); code != http.StatusInternalServerError {
		t.Fatalf("GitHub down: status %d", code)
	}
	expectOutcome("wr-1", core.DeliveryFailed, "GitHub is down")
	github.set(completed, false)
	if code := deliver("workflow_run", "wr-1", run); code != http.StatusNoContent {
		t.Fatalf("redelivery: status %d", code)
	}
	expectOutcome("wr-1", core.DeliveryHandled, "")
	fleet, err := service.Fleet(ctx)
	if err != nil || len(fleet.Runs) != 1 || fleet.Runs[0].Status != "completed" {
		t.Fatalf("run not completed: %+v %v", fleet.Runs, err)
	}

	// Every verified delivery reached the store, in its own stream, unchanged.
	want := []string{"ping-1", "pr-1", "wr-unknown", "wr-bad", "wr-bad", "wr-bad", "wr-1", "wr-1"}
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
