package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agbredo/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeBackend struct {
	mu            sync.Mutex
	workflows     map[int64]WorkflowRun
	creates       int
	dispatches    int
	dispatchError error
	createError   error
	issues        map[string]string
}

func (f *fakeBackend) Dispatch(context.Context, Agent, string, map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatches++
	return f.dispatchError
}
func (f *fakeBackend) GetRun(_ context.Context, _ Agent, id int64) (WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.workflows[id]
	if !ok {
		return r, errors.New("no workflow")
	}
	return r, nil
}
func (f *fakeBackend) FindRuns(_ context.Context, _ Agent, title string, _ time.Time) ([]WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []WorkflowRun
	for _, r := range f.workflows {
		if r.Title == title {
			result = append(result, r)
		}
	}
	return result, nil
}
func (f *fakeBackend) CreateIssue(_ context.Context, _ Agent, _ Proposal, marker string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	url := "https://github.com/acme/agent/issues/1"
	f.issues[marker] = url
	return url, f.createError
}
func (f *fakeBackend) FindIssue(_ context.Context, _ Agent, marker string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[marker], nil
}
func (f *fakeBackend) put(r WorkflowRun) { f.mu.Lock(); defer f.mu.Unlock(); f.workflows[r.ID] = r }

func validAgent() Agent {
	return Agent{Name: "Example", Owner: "acme", Repository: "agent", Workflow: "agent.yml", ContinuationWorkflow: "continue.yml", Ref: "main", InstallationID: 123, BudgetMicros: 1_000_000, Enabled: true}
}
func TestAgentValidation(t *testing.T) {
	if err := ValidateAgent(validAgent()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*Agent)
	}{
		{"empty name", func(a *Agent) { a.Name = "" }}, {"owner path", func(a *Agent) { a.Owner = "../acme" }}, {"workflow path", func(a *Agent) { a.Workflow = "../x.yml" }},
		{"identical workflows", func(a *Agent) { a.ContinuationWorkflow = a.Workflow }}, {"invalid reference", func(a *Agent) { a.Ref = "main;\n" }}, {"installation", func(a *Agent) { a.InstallationID = 0 }}, {"negative budget", func(a *Agent) { a.BudgetMicros = -1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := validAgent()
			c.change(&a)
			if ValidateAgent(a) == nil {
				t.Fatal("accepted invalid agent")
			}
		})
	}
}
func TestTransitions(t *testing.T) {
	cases := [][3]string{{"queued", "running", "running"}, {"running", "queued", "running"}, {"running", "completed", "completed"}, {"completed", "running", "completed"}, {"failed", "completed", "failed"}, {"cancelled", "running", "cancelled"}, {"waiting_approval", "completed", "waiting_approval"}, {"waiting_approval", "failed", "waiting_approval"}}
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
	got := string(redactJSON([]byte(`{"secret":"hidden","nested":{"message":"Bearer hidden"}}`)))
	if strings.Contains(got, "hidden") {
		t.Fatal(got)
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
	fake := &fakeBackend{workflows: map[int64]WorkflowRun{}, issues: map[string]string{}}
	return &Service{DB: pool, Backend: fake, BaseURL: "https://agbredo.example", ApprovalTTL: time.Hour, Admins: []string{"admin"}}, fake
}
func launched(t *testing.T, s *Service, f *fakeBackend) (Run, Identity) {
	t.Helper()
	a, err := s.SaveAgent(context.Background(), validAgent())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Launch(context.Background(), a.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	f.put(WorkflowRun{ID: 100, Title: "agbredo:" + r.CorrelationID + ":initial", Workflow: a.Workflow, Ref: "main", Status: "in_progress", URL: "https://github.com/acme/agent/actions/runs/100"})
	return r, Identity{Repository: "acme/agent", Workflow: a.Workflow, Ref: "refs/heads/main", RunID: 100, RunAttempt: 1}
}
func proposed(t *testing.T, s *Service, f *fakeBackend) (Run, Approval, Identity) {
	t.Helper()
	r, i := launched(t, s, f)
	e := Event{ID: "proposal", CorrelationID: r.CorrelationID, Status: "waiting_approval", Type: "approval.proposed", Proposal: &Proposal{Action: "create_issue", Resource: "acme/agent", Title: "Approved test", Body: "Test body"}}
	if _, err := s.Ingest(context.Background(), r.ID, e, i); err != nil {
		t.Fatal(err)
	}
	fleet, err := s.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := fleet.Approvals[0]
	f.put(WorkflowRun{ID: 200, Title: "agbredo:" + r.CorrelationID + ":approval:" + a.ID, Workflow: r.Agent.ContinuationWorkflow, Ref: "main", Status: "in_progress"})
	ci := Identity{Repository: i.Repository, Workflow: r.Agent.ContinuationWorkflow, Ref: i.Ref, RunID: 200, RunAttempt: 1}
	return r, a, ci
}

func TestDatabaseAgentCRUDSnapshots(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, _ := launched(t, s, f)
	a := r.Agent
	a.Name = "Updated"
	a, err := s.SaveAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 2 {
		t.Fatal(a)
	}
	read, err := s.Run(ctx, r.ID)
	if err != nil || read.Agent.Name != "Example" || read.Agent.Version != 1 {
		t.Fatal(read, err)
	}
	a.Enabled = false
	if _, err = s.SaveAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Launch(ctx, a.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Agent(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = s.Run(ctx, r.ID); err != nil {
		t.Fatal("historical run lost", err)
	}
}
func TestDatabaseEventAuthenticationIdempotencySpending(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, i := launched(t, s, f)
	e := Event{ID: "usage", CorrelationID: r.CorrelationID, Type: "usage", Status: "running", CostMicros: 750_000, InputTokens: 10, OutputTokens: 5, Message: "token=private"}
	bad := i
	bad.Repository = "attacker/repo"
	if _, err := s.Ingest(ctx, r.ID, e, bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	bad = i
	bad.Workflow = "other.yml"
	if _, err := s.Ingest(ctx, r.ID, e, bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	wrong := e
	wrong.CorrelationID = "other"
	if _, err := s.Ingest(ctx, r.ID, wrong, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	negative := e
	negative.CostMicros = -1
	if _, err := s.Ingest(ctx, r.ID, negative, i); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid cost accepted", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Ingest(ctx, r.ID, e, i); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	read, err := s.Run(ctx, r.ID)
	if err != nil || read.CostMicros != 750_000 || read.InputTokens != 10 || read.OutputTokens != 5 {
		t.Fatal(read, err)
	}
	e.ID = "usage2"
	e.CostMicros = 300_000
	if _, err = s.Ingest(ctx, r.ID, e, i); err != nil {
		t.Fatal(err)
	}
	read, _ = s.Run(ctx, r.ID)
	if read.CostMicros != 1_050_000 || !read.OverBudget {
		t.Fatal(read)
	}
	events, _ := s.Events(ctx, r.ID)
	for _, event := range events {
		if strings.Contains(event.Message, "private") {
			t.Fatal("secret persisted")
		}
	}
}
func TestDatabaseOutOfOrderEventsAndCorrelation(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, i := launched(t, s, f)
	gh, _ := f.GetRun(ctx, r.Agent, 100)
	gh.Status = "completed"
	gh.Conclusion = "success"
	if err := s.ApplyLifecycle(ctx, r.ID, gh, "delivery1"); err != nil {
		t.Fatal(err)
	}
	// A completion webhook can arrive before the HTTP proposal; it must not close the run.
	e := Event{ID: "proposal", CorrelationID: r.CorrelationID, Type: "approval", Proposal: &Proposal{Action: "create_issue", Resource: "acme/agent", Title: "Test"}}
	if _, err := s.Ingest(ctx, r.ID, e, i); err != nil {
		t.Fatal(err)
	}
	gh.Status = "in_progress"
	gh.Conclusion = ""
	if err := s.ApplyLifecycle(ctx, r.ID, gh, "delivery2"); err != nil {
		t.Fatal(err)
	}
	read, _ := s.Run(ctx, r.ID)
	if read.Status != "waiting_approval" || read.GitHubRunID != 100 {
		t.Fatal(read)
	}
	before, _ := s.Events(ctx, r.ID)
	if err := s.ApplyLifecycle(ctx, r.ID, gh, "delivery2"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Events(ctx, r.ID)
	if len(after) != len(before) {
		t.Fatal("webhook replay inserted events")
	}
	wrong := gh
	wrong.ID = 999
	wrong.Title = "agbredo:other:initial"
	if err := s.ApplyLifecycle(ctx, r.ID, wrong, ""); !errors.Is(err, ErrUnauthorized) && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestDatabaseApprovalExecutesOnce(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if err := s.Decide(ctx, a.ID, "runner", "approve", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, a.ID, "admin", "approve", "Reviewed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, a.ID, "admin", "approve", "Again"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i)
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.creates != 1 {
		t.Fatalf("created %d issues", f.creates)
	}
	read, _ := s.Approval(ctx, a.ID)
	run, _ := s.Run(ctx, r.ID)
	if read.Status != "executed" || read.DecidedBy != "admin" || read.ResultURL == "" || run.Status != "completed" {
		t.Fatal(read, run)
	}
	e := Event{ID: "continuation.completed", ApprovalID: a.ID, CorrelationID: r.CorrelationID, Type: "execution.completed", Status: "completed"}
	if _, err := s.Ingest(ctx, r.ID, e, i); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Events(ctx, r.ID)
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, kind := range []string{"approval.requested", "approval.approved", "action.started", "action.executed", "execution.completed"} {
		if !seen[kind] {
			t.Errorf("missing audit %s", kind)
		}
	}
}
func TestDatabaseConcurrentDecisions(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	_, a, _ := proposed(t, s, f)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			decision := "approve"
			if n%2 == 0 {
				decision = "reject"
			}
			err := s.Decide(ctx, a.ID, "admin", decision, "")
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("%d accepted decisions", success)
	}
}
func TestDatabaseRejectionAndRunnerCannotSelfAuthorize(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := s.Decide(ctx, a.ID, "admin", "reject", "Unsafe proposal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	run, _ := s.Run(ctx, r.ID)
	if run.Status != "cancelled" || f.creates != 0 {
		t.Fatal(run)
	}
	var jobs int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM dispatch_jobs WHERE approval_id=$1", a.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatal(jobs, err)
	}
}
func TestDatabaseExpiredAndStaleApproval(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		s, f := testService(t)
		s.ApprovalTTL = 30 * time.Millisecond
		_, a, _ := proposed(t, s, f)
		time.Sleep(time.Until(a.ExpiresAt) + time.Millisecond)
		if err := s.Decide(context.Background(), a.ID, "admin", "approve", ""); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	})
	t.Run("stale config", func(t *testing.T) {
		s, f := testService(t)
		ctx := context.Background()
		r, a, i := proposed(t, s, f)
		if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
			t.Fatal(err)
		}
		changed := r.Agent
		changed.Description = "new version"
		if _, err := s.SaveAgent(ctx, changed); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if f.creates != 0 {
			t.Fatal("stale executed")
		}
	})
	t.Run("expires before execution", func(t *testing.T) {
		s, f := testService(t)
		s.ApprovalTTL = 300 * time.Millisecond
		ctx := context.Background()
		r, a, i := proposed(t, s, f)
		if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Until(a.ExpiresAt) + time.Millisecond)
		if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
		if err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		read, _ := s.Approval(ctx, a.ID)
		if read.Status != "expired" || f.creates != 0 {
			t.Fatal(read)
		}
	})
}
func TestDatabaseBudgetAndContinuationSafety(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, "tampered", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	wrong := i
	wrong.Workflow = r.Agent.Workflow
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, wrong); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	initial := Identity{Repository: "acme/agent", Workflow: r.Agent.Workflow, Ref: "refs/heads/main", RunID: 100}
	if _, err := s.Ingest(ctx, r.ID, Event{ID: "usage", CorrelationID: r.CorrelationID, Type: "usage", CostMicros: r.BudgetMicros + 1}, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if f.creates != 0 {
		t.Fatal("over budget executed")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE approvals SET document=jsonb_set(document,'{proposal,title}','"changed"') WHERE id=$1`, a.ID); err == nil {
		t.Fatal("database allowed immutable payload mutation")
	}
}
func TestDatabaseUncertainExecutionRecovery(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
		t.Fatal(err)
	}
	f.createError = errors.New("lost response after creation")
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); err == nil {
		t.Fatal("claimed uncertain success")
	}
	read, _ := s.Approval(ctx, a.ID)
	if read.Status != "execution_unknown" {
		t.Fatal(read)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, a.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	read, _ = s.Approval(ctx, a.ID)
	if read.Status != "executed" || f.creates != 1 {
		t.Fatal(read, f.creates)
	}
}

func TestDatabaseContinuationRetryUsesNewWorkflow(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, "UPDATE dispatch_jobs SET finished=true,attempts=1"); err != nil {
		t.Fatal(err)
	}
	old, _ := f.GetRun(ctx, r.Agent, i.RunID)
	old.Status = "completed"
	old.Conclusion = "failure"
	f.put(old)
	if err := s.ApplyLifecycle(ctx, r.ID, old, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, a.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	read, _ := s.Approval(ctx, a.ID)
	if read.GitHubRunID != 0 || !ignoredRun(read, old.ID) {
		t.Fatal("rebound failed continuation", read)
	}
	if err := s.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	if f.dispatches != 1 {
		t.Fatal("retry did not dispatch a new continuation", f.dispatches)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old runner retained authorization", err)
	}
	newWorkflow := old
	newWorkflow.ID = 201
	newWorkflow.Status = "in_progress"
	newWorkflow.Conclusion = ""
	f.put(newWorkflow)
	i.RunID = 201
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatal(f.creates)
	}
}

func TestDatabaseUncertainResultNeverBlindlyRetries(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, a, i := proposed(t, s, f)
	if err := s.Decide(ctx, a.ID, "admin", "approve", ""); err != nil {
		t.Fatal(err)
	}
	f.createError = errors.New("network uncertain")
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); err == nil {
		t.Fatal("unexpected success")
	}
	f.mu.Lock()
	delete(f.issues, a.ID)
	f.mu.Unlock()
	if err := s.Retry(ctx, a.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.Execute(ctx, a.ID, r.ID, r.CorrelationID, a.PayloadHash, i); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	read, _ := s.Approval(ctx, a.ID)
	if read.Status != "execution_unknown" || f.creates != 1 {
		t.Fatal(read, f.creates)
	}
}
func TestDatabaseDurableDispatchAndReconciliation(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	r, _ := launched(t, s, f)
	f.dispatchError = errors.New("temporary failure")
	if err := s.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	var finished bool
	if err := s.DB.QueryRow(ctx, "SELECT finished FROM dispatch_jobs WHERE run_id=$1", r.ID).Scan(&finished); err != nil || finished {
		t.Fatal(finished, err)
	}
	f.dispatchError = nil
	if _, err := s.DB.Exec(ctx, "UPDATE dispatch_jobs SET available_at=now()"); err != nil {
		t.Fatal(err)
	}
	if err := s.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	if f.dispatches != 1 {
		t.Fatal("retried a correlated dispatch", f.dispatches)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	read, _ := s.Run(ctx, r.ID)
	if read.GitHubRunID != 100 || read.Status != "running" {
		t.Fatal(read)
	}
	gh, _ := f.GetRun(ctx, r.Agent, 100)
	gh.Status = "completed"
	gh.Conclusion = "failure"
	f.put(gh)
	if err := s.Webhook(ctx, "delivery", r.Agent.Owner+"/"+r.Agent.Repository, gh.ID, gh.Title); err != nil {
		t.Fatal(err)
	}
	read, _ = s.Run(ctx, r.ID)
	if read.Status != "failed" {
		t.Fatal(read)
	}
	// A delayed running event cannot reopen a terminal run.
	gh.Status = "in_progress"
	gh.Conclusion = ""
	if err := s.ApplyLifecycle(ctx, r.ID, gh, ""); err != nil {
		t.Fatal(err)
	}
	read, _ = s.Run(ctx, r.ID)
	if read.Status != "failed" {
		t.Fatal(read)
	}
}

func ExampleEvent() {
	e := Event{ID: "stable-event-id", CostMicros: 125_000, InputTokens: 100}
	fmt.Printf("%s: $%.6f, %d reported tokens", e.ID, float64(e.CostMicros)/1_000_000, e.InputTokens)
	// Output: stable-event-id: $0.125000, 100 reported tokens
}
