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
	// installations maps "owner/repository", spelled as asked, to the App's installation.
	installations map[string]int64
	lookups       int
	// installErr, if set, is what every installation lookup answers.
	installErr error
}

func (f *fakeBackend) Installation(_ context.Context, owner, repository string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.installErr != nil {
		return 0, f.installErr
	}
	id, ok := f.installations[owner+"/"+repository]
	if !ok {
		return 0, ErrNotInstalled
	}
	return id, nil
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
	return Deployment{Name: "Example", Owner: "acme", Repository: "agent", Workflow: "agent.yml", InstallationID: 123, ThresholdMicros: 1_000_000, Enabled: true}
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
		{"installation", func(a *Deployment) { a.InstallationID = 0 }}, {"negative threshold", func(a *Deployment) { a.ThresholdMicros = -1 }},
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
	fake := &fakeBackend{workflows: map[int64]WorkflowRun{}, installations: map[string]int64{}}
	return &Service{DB: pool, Backend: fake}, fake
}

// registeredDeployment saves a deployment and a GitHub run attempt of it, and
// returns the identity of a job in that attempt.
func registeredDeployment(t *testing.T, s *Service, f *fakeBackend) (Deployment, Identity) {
	t.Helper()
	a := validDeployment()
	a, err := s.SaveDeployment(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Title: "Existing agent title", Workflow: a.Workflow, HeadBranch: "main", Status: "in_progress"})
	return a, Identity{Repository: "acme/agent", Workflow: a.Workflow, Ref: "refs/heads/main", RunID: 777, RunAttempt: 1, EventName: "schedule"}
}

// startRun registers the run of identity i the way a job does: by asking for a telemetry token.
func startRun(t *testing.T, s *Service, deploymentID, agentRun string, i Identity) Run {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.MintToken(ctx, deploymentID, agentRun, i); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, attemptRunID(deploymentID, i.RunID, i.RunAttempt))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// webhook delivers a workflow_run webhook of repository whose payload
// describes the run attempt gh.
func webhook(s *Service, delivery, repository string, gh WorkflowRun) error {
	gh.Repository = repository
	action := "in_progress"
	switch gh.Status {
	case "completed":
		action = "completed"
	case "queued":
		action = "requested"
	}
	return s.Webhook(context.Background(), Delivery{ID: delivery, Event: "workflow_run", Action: action, Repository: repository, InstallationID: 123}, gh)
}

// breakRuns makes every write to the runs table fail, as when the database
// fails, until the returned function is called.
func breakRuns(t *testing.T, s *Service) func() {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION broken_runs() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'runs are broken'; END; $$;
		CREATE TRIGGER broken_runs BEFORE INSERT OR UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION broken_runs()`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := s.DB.Exec(ctx, "DROP TRIGGER broken_runs ON runs; DROP FUNCTION broken_runs()"); err != nil {
			t.Fatal(err)
		}
	}
}

// deliveryOutcome reads the index row of a delivery: its outcome and error, "" when NULL.
func deliveryOutcome(t *testing.T, s *Service, id string) (string, string) {
	t.Helper()
	var outcome, problem *string
	if err := s.DB.QueryRow(context.Background(), "SELECT outcome, error FROM webhook_deliveries WHERE id=$1", id).Scan(&outcome, &problem); err != nil {
		t.Fatal(id, err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return deref(outcome), deref(problem)
}

func TestDatabaseDeploymentCRUDSnapshots(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "agent", i)
	a.Name = "Updated"
	a, err := s.SaveDeployment(ctx, a)
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

// A repository + workflow file is registered once among live deployments.
// Owner and repository ignore case, as on GitHub.
func TestDatabaseDeploymentIsUniquePerRepositoryAndWorkflow(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	first, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Deployment){
		"same":       func(*Deployment) {},
		"other case": func(a *Deployment) { a.Owner, a.Repository = "ACME", "Agent" },
	} {
		dup := validDeployment()
		change(&dup)
		dup.Name = "Duplicate"
		if _, err = s.SaveDeployment(ctx, dup); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("%s: duplicate accepted or wrong error: %v", name, err)
		}
	}
	other := validDeployment()
	other.Workflow = "other.yml"
	if other, err = s.SaveDeployment(ctx, other); err != nil {
		t.Fatal("another workflow in the same repository refused", err)
	}
	other.Workflow = first.Workflow
	if _, err = s.SaveDeployment(ctx, other); !errors.Is(err, ErrInvalid) {
		t.Fatal("edit into a duplicate accepted", err)
	}
	if err = s.DeleteDeployment(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveDeployment(ctx, validDeployment()); err != nil {
		t.Fatal("a deleted deployment cannot be registered again", err)
	}
}

// Deployments stored when they were tied to one ref still load and still get
// runs, from any ref. Their runs keep that copy unchanged and record their own ref.
func TestDatabaseLegacyDeploymentWithRef(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	legacy := `{"id":"legacy","name":"Legacy","description":"","owner":"acme","repository":"agent","workflow":"agent.yml","ref":"main","installation_id":123,"budget_micros":1000000,"enabled":true,"version":1}`
	if _, err := s.DB.Exec(ctx, "INSERT INTO agents(id,document) VALUES('legacy',$1)", legacy); err != nil {
		t.Fatal(err)
	}
	a, err := s.Deployment(ctx, "legacy")
	if err != nil || a.Ref != "main" || a.Workflow != "agent.yml" {
		t.Fatalf("legacy deployment did not load: %#v %v", a, err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", HeadBranch: "feature", Status: "in_progress"})
	i := Identity{Repository: "acme/agent", Workflow: "agent.yml", Ref: "refs/heads/feature", RunID: 777, RunAttempt: 1, EventName: "push"}
	r := startRun(t, s, "legacy", "agent", i)
	if r.Ref != "refs/heads/feature" || r.Deployment.Ref != "main" {
		t.Fatalf("run ref %q, copy ref %q", r.Ref, r.Deployment.Ref)
	}
	if err = webhook(s, "legacy-delivery", "acme/agent", WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", HeadBranch: "feature", Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatal("saving a run with a legacy copy failed", err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "completed" || read.Deployment.Ref != "main" {
		t.Fatalf("%#v", read)
	}
}

func TestDatabaseWebhookAndReconciliation(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "job", i)
	var err error
	gh, _ := f.GetRun(ctx, a, 777)
	gh.Status = "completed"
	gh.Conclusion = "failure"
	if err = webhook(s, "delivery", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "delivery", "acme/agent", gh); err != nil {
		t.Fatal("redelivery failed", err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "failed" {
		t.Fatal(read)
	}
	// A webhook from another repository, or a delayed running state, changes nothing.
	if err = webhook(s, "other-repo", "evil/repo", gh); err != nil {
		t.Fatal(err)
	}
	gh.Status, gh.Conclusion = "in_progress", ""
	if err = webhook(s, "late", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyLifecycle(ctx, r.ID, gh); err != nil {
		t.Fatal(err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "failed" {
		t.Fatal("terminal run reopened", read.Status)
	}
	// Webhooks for a workflow file with no deployment are ignored.
	if err = webhook(s, "unknown-workflow", "acme/agent", WorkflowRun{ID: 999, Attempt: 1, Workflow: "other.yml", Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	if fleet, _ := s.Fleet(ctx); len(fleet.Runs) != 1 {
		t.Fatalf("%d runs", len(fleet.Runs))
	}
}

// Every workflow_run delivery is indexed. A failed one is processed again when
// GitHub sends it again; a handled one, including one indexed before outcomes
// were kept, is not.
func TestDatabaseWebhookDeliveryIndex(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "job", i)
	gh, _ := f.GetRun(ctx, a, 777)
	gh.Status, gh.Conclusion = "completed", "success"

	// A row from before migration 007 has no outcome. It was handled.
	if _, err := s.DB.Exec(ctx, "INSERT INTO webhook_deliveries(id) VALUES('old')"); err != nil {
		t.Fatal(err)
	}
	if err := webhook(s, "old", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "running" {
		t.Fatal("an old handled delivery was applied again", read.Status)
	}
	if outcome, _ := deliveryOutcome(t, s, "old"); outcome != "" {
		t.Fatal("old row changed", outcome)
	}

	// The database fails: failed, with the reason.
	restore := breakRuns(t, s)
	if err := webhook(s, "flaky", "acme/agent", gh); err == nil {
		t.Fatal("failure not reported")
	}
	if outcome, problem := deliveryOutcome(t, s, "flaky"); outcome != DeliveryFailed || !strings.Contains(problem, "runs are broken") {
		t.Fatalf("outcome %q, error %q", outcome, problem)
	}
	// The redelivery is processed, and the row says handled.
	restore()
	if err := webhook(s, "flaky", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if outcome, problem := deliveryOutcome(t, s, "flaky"); outcome != DeliveryHandled || problem != "" {
		t.Fatalf("outcome %q, error %q", outcome, problem)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "completed" {
		t.Fatal(read.Status)
	}
	var event, action, repository string
	var installation int64
	if err := s.DB.QueryRow(ctx, "SELECT event, action, repository, installation_id FROM webhook_deliveries WHERE id='flaky'").Scan(&event, &action, &repository, &installation); err != nil ||
		event != "workflow_run" || action != "completed" || repository != "acme/agent" || installation != 123 {
		t.Fatalf("%q %q %q %d %v", event, action, repository, installation, err)
	}
	// Once handled, a redelivery is not applied again, and one that would fail leaves it handled.
	if err := webhook(s, "flaky", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	restore = breakRuns(t, s)
	if err := webhook(s, "flaky", "acme/agent", gh); err != nil {
		t.Fatal("a handled delivery was processed again", err)
	}
	restore()
	if outcome, problem := deliveryOutcome(t, s, "flaky"); outcome != DeliveryHandled || problem != "" {
		t.Fatal(outcome, problem)
	}

	// A workflow file with no deployment, or another repository: ignored.
	for _, d := range []struct{ id, repository, workflow string }{{"unknown-workflow", "acme/agent", "other.yml"}, {"other-repo", "evil/repo", a.Workflow}} {
		if err := webhook(s, d.id, d.repository, WorkflowRun{ID: 999, Attempt: 1, Workflow: d.workflow, Status: "in_progress"}); err != nil {
			t.Fatal(err)
		}
		if outcome, _ := deliveryOutcome(t, s, d.id); outcome != DeliveryIgnored {
			t.Fatal(d.id, outcome)
		}
	}
	// An ignored delivery that fails on redelivery becomes failed, then ignored again.
	if err := s.RecordDelivery(ctx, Delivery{ID: "unknown-workflow"}, DeliveryFailed, errors.New("timeout")); err != nil {
		t.Fatal(err)
	}
	if outcome, problem := deliveryOutcome(t, s, "unknown-workflow"); outcome != DeliveryFailed || problem != "timeout" {
		t.Fatal(outcome, problem)
	}
	if err := webhook(s, "unknown-workflow", "acme/agent", WorkflowRun{ID: 999, Attempt: 1, Workflow: "other.yml", Status: "in_progress"}); err != nil {
		t.Fatal(err)
	}
	if outcome, problem := deliveryOutcome(t, s, "unknown-workflow"); outcome != DeliveryIgnored || problem != "" {
		t.Fatal(outcome, problem)
	}
}

// A delivery for a workflow with no deployment yet is ignored. Once the
// deployment exists, GitHub's redelivery of it records the run.
func TestDatabaseIgnoredDeliveryIsHandledOnRedelivery(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	gh := WorkflowRun{ID: 777, Attempt: 1, Workflow: "agent.yml", Status: "completed", Conclusion: "success"}
	if err := webhook(s, "early", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "early"); outcome != DeliveryIgnored {
		t.Fatal(outcome)
	}
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if err := webhook(s, "early", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "early"); outcome != DeliveryHandled {
		t.Fatal(outcome)
	}
	if read, err := s.Run(ctx, attemptRunID(a.ID, 777, 1)); err != nil || read.Status != "completed" {
		t.Fatal("redelivery not applied", read.Status, err)
	}
}

// An error cut to 500 bytes stays valid UTF-8, or PostgreSQL refuses it.
func TestDatabaseDeliveryErrorIsCutOnACharacter(t *testing.T) {
	s, _ := testService(t)
	long := strings.Repeat("a", 499) + "é and more"
	if err := s.RecordDelivery(context.Background(), Delivery{ID: "long"}, DeliveryFailed, errors.New(long)); err != nil {
		t.Fatal(err)
	}
	if _, problem := deliveryOutcome(t, s, "long"); problem != strings.Repeat("a", 499) {
		t.Fatalf("error %q", problem)
	}
}

// The first valid request for a run attempt creates its run, once, with a copy
// of the deployment as it was. A rerun attempt is a run of its own.
func TestDatabaseFirstRequestsKeepOneRunAndSnapshot(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.MintToken(ctx, a.ID, fmt.Sprintf("step-%d", n), i); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	fleet, err := s.Fleet(ctx)
	if err != nil || len(fleet.Runs) != 1 {
		t.Fatalf("first requests raced: %#v %v", fleet, err)
	}
	a.Description = "Updated registration"
	if a, err = s.SaveDeployment(ctx, a); err != nil {
		t.Fatal(err)
	}
	r := startRun(t, s, a.ID, "next-step", i)
	if r.Deployment.Revision != 1 || r.Deployment.Description != "" {
		t.Fatalf("historical snapshot changed: %#v", r.Deployment)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, HeadBranch: "main", Status: "completed", Conclusion: "success"})
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "completed" {
		t.Fatal("poll did not finish the run", read.Status)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 2, Workflow: a.Workflow, HeadBranch: "main", Status: "in_progress"})
	rerun := i
	rerun.RunAttempt = 2
	second := startRun(t, s, a.ID, "agent", rerun)
	if second.ID == r.ID || second.RunAttempt != 2 || second.Deployment.Revision != 2 || second.Status != "running" {
		t.Fatalf("rerun merged into prior attempt: %#v", second)
	}
}

// Events stored by the former report route carry extra keys. They still load.
func TestDatabaseOldReportEventsStillLoad(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "agent", i)
	old := `{"claude":{"github_run_id":777,"github_run_attempt":1,"source":"agent","phase":"finished","version":"2.1.296","exit_code":0,"duration_ms":5,"telemetry_supported":true,"telemetry_received":true,"api_errors":0,"api_failed":false,"rate_limit_failures":0},` +
		`"id":"github:777:1:agent:finished","run_id":"` + r.ID + `","correlation_id":"` + r.ID + `","agent_id":"` + a.ID + `","agent_version":1,"timestamp":"2026-01-02T03:04:05Z",` +
		`"type":"claude.execution","action":"","resource":"","status":"completed","duration_ms":5,"input_tokens":100,"output_tokens":10,"cost_micros":3,"message":"Claude Code execution report","error":""}`
	if _, err := s.DB.Exec(ctx, "INSERT INTO events(run_id,id,document) VALUES($1,$2,$3)", r.ID, "github:777:1:agent:finished", old); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, r.ID)
	if err != nil || len(events) != 1 || events[0].Type != "claude.execution" || events[0].Status != "completed" {
		t.Fatalf("old report event did not load: %#v %v", events, err)
	}
}

func ExampleEvent() {
	e := Event{ID: "system:github:777:1:completed:success", Type: "github.workflow_run", Status: "completed"}
	fmt.Printf("%s %s: %s", e.Type, e.Status, e.ID)
	// Output: github.workflow_run completed: system:github:777:1:completed:success
}
