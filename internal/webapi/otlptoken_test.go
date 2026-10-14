package webapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agbredo/internal/core"
	"agbredo/internal/forward"
	"agbredo/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gitHub is a stand-in for the GitHub App: one run, and one installation.
type gitHub struct{ run core.WorkflowRun }

func (g gitHub) GetRun(context.Context, core.Deployment, int64) (core.WorkflowRun, error) {
	return g.run, nil
}
func (g gitHub) GetRunAttempt(context.Context, core.Deployment, int64, int64) (core.WorkflowRun, error) {
	return g.run, nil
}
func (g gitHub) Installation(_ context.Context, owner, repository string) (int64, error) {
	if owner+"/"+repository != "acme/agent" {
		return 0, core.ErrNotInstalled
	}
	return 42, nil
}

// databaseService gives a service on a private schema, dropped afterwards.
func databaseService(t *testing.T, backend core.Backend) *core.Service {
	t.Helper()
	url := os.Getenv("AGBREDO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AGBREDO_TEST_DATABASE_URL to run real PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "agbredo_test_" + core.ID()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, e := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return &core.Service{DB: pool, Backend: backend, DefaultThresholdMicros: 5_000_000}
}

type tokenReply struct {
	Token       string `json:"token"`
	TraceParent string `json:"traceparent"`
}

func postToken(t *testing.T, a *API, path string) (int, tokenReply) {
	t.Helper()
	w := requestAPI(a, http.MethodPost, path, `{"source":"review"}`, map[string]string{"Authorization": "Bearer signed-token", "Content-Type": "application/json"})
	var reply tokenReply
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, reply
}

// POST /api/otlp-token creates the deployment and the run, and answers like
// the old route: its traceparent carries the run's trace ID, which the old
// route gives for the same run attempt too.
func TestDatabaseTokenWithoutDeploymentID(t *testing.T) {
	ctx := context.Background()
	service := databaseService(t, gitHub{run: core.WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "in_progress"}})
	v := &fakeVerifier{identity: core.Identity{Repository: "acme/agent", Workflow: "agent.yml", Ref: "refs/heads/main", RunID: 777, RunAttempt: 1, EventName: "push"}}
	a := testAPI(v)
	a.Service = service
	code, reply := postToken(t, a, "/api/otlp-token")
	if code != http.StatusOK || len(reply.Token) < 40 {
		t.Fatalf("status %d, reply %+v", code, reply)
	}
	fleet, err := service.Fleet(ctx)
	if err != nil || len(fleet.Deployments) != 1 || len(fleet.Runs) != 1 {
		t.Fatalf("%d deployments, %d runs, %v", len(fleet.Deployments), len(fleet.Runs), err)
	}
	run := fleet.Runs[0]
	if len(reply.TraceParent) != 55 || reply.TraceParent[3:35] != run.ID[:32] {
		t.Fatalf("traceparent %q does not follow run %s", reply.TraceParent, run.ID)
	}
	code, old := postToken(t, a, "/api/agents/"+fleet.Deployments[0].ID+"/otlp-token")
	if code != http.StatusOK || old.TraceParent[3:35] != reply.TraceParent[3:35] {
		t.Fatalf("old route: status %d, traceparent %q", code, old.TraceParent)
	}

	// A repository without the App is refused, and nothing is stored.
	v.identity.Repository = "other/agent"
	if code, _ = postToken(t, a, "/api/otlp-token"); code != http.StatusForbidden {
		t.Fatalf("repository without the App: status %d", code)
	}
	if fleet, _ = service.Fleet(ctx); len(fleet.Deployments) != 1 {
		t.Fatalf("%d deployments", len(fleet.Deployments))
	}
}

// A trace that is not valid JSON is refused with 400 and never forwarded.
// A valid one sent after it is the first thing the store receives.
func TestDatabaseMalformedTraceIsRefusedAndNotForwarded(t *testing.T) {
	service := databaseService(t, gitHub{run: core.WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "in_progress"}})
	received := make(chan string, 10)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- r.URL.Path + " " + string(body)
	}))
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := testAPI(&fakeVerifier{identity: core.Identity{Repository: "acme/agent", Workflow: "agent.yml", Ref: "refs/heads/main", RunID: 777, RunAttempt: 1, EventName: "push"}})
	a.Service = service
	a.Forward = forward.New(store.URL, "dXNlcjpwYXNz", a.Logger)
	go a.Forward.Run(ctx)
	code, reply := postToken(t, a, "/api/otlp-token")
	if code != http.StatusOK {
		t.Fatalf("token: status %d", code)
	}
	headers := map[string]string{"Authorization": "Bearer " + reply.Token, "Content-Type": "application/json"}
	if w := requestAPI(a, http.MethodPost, "/v1/traces", `{"resourceSpans":[`, headers); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed trace: status %d", w.Code)
	}
	if w := requestAPI(a, http.MethodPost, "/v1/traces", `{"resourceSpans":[]}`, headers); w.Code != http.StatusOK {
		t.Fatalf("valid trace: status %d", w.Code)
	}
	select {
	case got := <-received:
		// The malformed body is not valid JSON; the first thing forwarded must be.
		if !strings.HasPrefix(got, "/v1/traces ") || !json.Valid([]byte(strings.TrimPrefix(got, "/v1/traces "))) {
			t.Fatalf("the store first received %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the valid trace was not forwarded")
	}
	select {
	case got := <-received:
		t.Fatalf("forwarded more than the valid trace: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}
