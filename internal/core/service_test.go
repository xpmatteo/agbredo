package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"agbredo/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeBackend struct {
	mu        sync.Mutex
	workflows map[int64]WorkflowRun
}

func (f *fakeBackend) GetRun(_ context.Context, _ Deployment, id int64) (WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.workflows[id]
	if !ok {
		return r, errors.New("no workflow")
	}
	return r, nil
}
func (f *fakeBackend) GetRunAttempt(ctx context.Context, a Deployment, id int64, attempt int64) (WorkflowRun, error) {
	return f.GetRun(ctx, a, id)
}
func (f *fakeBackend) put(r WorkflowRun) { f.mu.Lock(); defer f.mu.Unlock(); f.workflows[r.ID] = r }

func validDeployment() Deployment {
	return Deployment{Name: "Example", Owner: "acme", Repository: "agent", Workflow: "agent.yml", Ref: "main", InstallationID: 123, ThresholdMicros: 1_000_000, Enabled: true}
}
func TestDeploymentValidation(t *testing.T) {
	if err := ValidateDeployment(validDeployment()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Deployment)
	}{
		{"empty name", func(a *Deployment) { a.Name = "" }}, {"owner path", func(a *Deployment) { a.Owner = "../acme" }}, {"workflow path", func(a *Deployment) { a.Workflow = "../x.yml" }},
		{"invalid reference", func(a *Deployment) { a.Ref = "main;\n" }}, {"installation", func(a *Deployment) { a.InstallationID = 0 }}, {"negative threshold", func(a *Deployment) { a.ThresholdMicros = -1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := validDeployment()
			c.change(&a)
			if ValidateDeployment(a) == nil {
				t.Fatal("accepted invalid deployment")
			}
		})
	}
}
func TestTransitions(t *testing.T) {
	cases := [][3]string{{"queued", "running", "running"}, {"running", "queued", "running"}, {"running", "completed", "completed"}, {"completed", "running", "completed"}, {"failed", "completed", "failed"}, {"cancelled", "running", "cancelled"}}
	for _, c := range cases {
		if got := Transition(c[0], c[1]); got != c[2] {
			t.Errorf("%s -> %s = %s", c[0], c[1], got)
		}
	}
}
func TestSecretsRedaction(t *testing.T) {
	for _, s := range []string{"Bearer abcdef", "token=abcdef", "api_key:abcdef", "github_pat_12345", "ghp_12345"} {
		if strings.Contains(Redact(s), "abcdef") || Redact(s) == s {
			t.Errorf("not redacted: %s", s)
		}
	}
}

// Each test receives a private schema; an existing database's tables are never reset.
func testService(t *testing.T) (*Service, *fakeBackend) {
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
	schema := "agbredo_test_" + ID()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10
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
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal("repeat migration", err)
	}
	fake := &fakeBackend{workflows: map[int64]WorkflowRun{}}
	return &Service{DB: pool, Backend: fake}, fake
}
func TestDatabaseDeploymentCRUDSnapshots(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := reportDeployment(t, s, f)
	r, _, err := s.Report(ctx, a.ID, finishedReport(), i)
	if err != nil {
		t.Fatal(err)
	}
	a.Name = "Updated"
	a, err = s.SaveDeployment(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if a.Revision != 2 {
		t.Fatal(a)
	}
	read, err := s.Run(ctx, r.ID)
	if err != nil || read.Deployment.Name != "Example" || read.Deployment.Revision != 1 {
		t.Fatal(read, err)
	}
	stale := a
	stale.Revision = 1
	if _, err = s.SaveDeployment(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale update accepted", err)
	}
	if err = s.DeleteDeployment(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Deployment(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = s.Run(ctx, r.ID); err != nil {
		t.Fatal("historical run lost", err)
	}
}
func TestDatabaseWebhookAndReconciliation(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := reportDeployment(t, s, f)
	r, _, err := s.Report(ctx, a.ID, Report{GitHubRunID: 777, GitHubRunAttempt: 1, Source: "job", Phase: "started", Version: "unknown"}, i)
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := f.GetRun(ctx, a, 777)
	gh.Status = "completed"
	gh.Conclusion = "failure"
	f.put(gh)
	if err = s.Webhook(ctx, "delivery", "acme/agent", 777); err != nil {
		t.Fatal(err)
	}
	if err = s.Webhook(ctx, "delivery", "acme/agent", 777); err != nil {
		t.Fatal("redelivery failed", err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "failed" {
		t.Fatal(read)
	}
	// A webhook from another repository, or a delayed running state, changes nothing.
	if err = s.Webhook(ctx, "other-repo", "evil/repo", 777); err != nil {
		t.Fatal(err)
	}
	gh.Status, gh.Conclusion = "in_progress", ""
	f.put(gh)
	if err = s.ApplyLifecycle(ctx, r.ID, gh, ""); err != nil {
		t.Fatal(err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "failed" {
		t.Fatal("terminal run reopened", read.Status)
	}
	// Webhooks for workflow runs that never reported are ignored.
	if err = s.Webhook(ctx, "unknown-run", "acme/agent", 999); err != nil {
		t.Fatal(err)
	}
}

func ExampleEvent() {
	e := Event{ID: "stable-event-id", CostMicros: 125_000, InputTokens: 100}
	fmt.Printf("%s: $%.6f, %d reported tokens", e.ID, float64(e.CostMicros)/1_000_000, e.InputTokens)
	// Output: stable-event-id: $0.125000, 100 reported tokens
}
