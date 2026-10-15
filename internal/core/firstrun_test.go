package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// deploymentCount counts the live deployments.
func deploymentCount(t *testing.T, s *Service) int {
	t.Helper()
	f, err := s.Fleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(f.Deployments)
}

// A job of a new repository + workflow file, in GitHub's spelling of the names.
func newWorkflowRun(f *fakeBackend, runID int64) Identity {
	f.put(WorkflowRun{ID: runID, Attempt: 1, Workflow: "triage.yml", Ref: "main", Status: "in_progress"})
	return Identity{Repository: "Acme/Agent", Workflow: "triage.yml", Ref: "refs/heads/main", RunID: runID, RunAttempt: 1, EventName: "schedule"}
}

// The first valid requests of a new repository + workflow file create one
// deployment between them, even when they race. A later run reuses it.
func TestDatabaseFirstRunCreatesTheDeployment(t *testing.T) {
	s, f := testService(t)
	s.DefaultThresholdMicros = 2_500_000
	f.installations["Acme/Agent"] = 99
	ctx := context.Background()
	i := newWorkflowRun(f, 777)
	grants := make([]Grant, 8)
	var wg sync.WaitGroup
	for n := range grants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := s.MintTokenForWorkflow(ctx, fmt.Sprintf("step-%d", n), i)
			if err != nil {
				t.Error(err)
			}
			grants[n] = g
		}()
	}
	wg.Wait()
	fleet, err := s.Fleet(ctx)
	if err != nil || len(fleet.Deployments) != 1 || len(fleet.Runs) != 1 {
		t.Fatalf("first requests raced: %d deployments, %d runs, %v", len(fleet.Deployments), len(fleet.Runs), err)
	}
	a := fleet.Deployments[0]
	want := Deployment{ID: a.ID, Name: "Acme/Agent · triage.yml", Owner: "Acme", Repository: "Agent", Workflow: "triage.yml", InstallationID: 99, ThresholdMicros: 2_500_000, Enabled: true, Revision: 1}
	if a != want {
		t.Fatalf("created %#v, want %#v", a, want)
	}
	for n, g := range grants {
		if g.DeploymentID != a.ID || !strings.HasPrefix(g.Token, "agb_") {
			t.Errorf("grant %d: %#v", n, g)
		}
	}
	r := fleet.Runs[0]
	if r.Deployment != a || r.ThresholdMicros != 2_500_000 || len(r.AgentRuns) != 0 || r.GitHubURL != "https://github.com/Acme/Agent/actions/runs/777" {
		t.Fatalf("run %#v", r)
	}

	lookups := f.lookups
	second := newWorkflowRun(f, 778)
	g, err := s.MintTokenForWorkflow(ctx, "review", second)
	if err != nil || g.DeploymentID != a.ID {
		t.Fatalf("second run: %#v %v", g, err)
	}
	if deploymentCount(t, s) != 1 || f.lookups != lookups {
		t.Fatalf("second run made a deployment or asked GitHub for the installation again: %d lookups", f.lookups-lookups)
	}
	// The dashboard edits it like any other deployment.
	a.Name, a.Description = "Triage", "Sorts new issues"
	if _, err = s.SaveDeployment(ctx, a); err != nil {
		t.Fatal(err)
	}
}

// Without the App on the repository, or when the run does not check out with
// GitHub, nothing is created.
func TestDatabaseFirstRunNeedsTheAppAndAValidRun(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	i := newWorkflowRun(f, 777)
	if _, err := s.MintTokenForWorkflow(ctx, "review", i); !errors.Is(err, ErrNotInstalled) || !errors.Is(err, ErrUnauthorized) {
		t.Fatal("token given without the App", err)
	}
	f.installations["Acme/Agent"] = 99
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: "other.yml", Ref: "main", Status: "in_progress"})
	if _, err := s.MintTokenForWorkflow(ctx, "review", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("token given for a run of another workflow", err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: "triage.yml", Ref: "main", Status: "completed", Conclusion: "success"})
	if _, err := s.MintTokenForWorkflow(ctx, "review", i); !errors.Is(err, ErrConflict) {
		t.Fatal("token given for a finished run", err)
	}
	bad := i
	bad.RunAttempt = 0
	if _, err := s.MintTokenForWorkflow(ctx, "review", bad); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := s.MintTokenForWorkflow(ctx, "bad agent run!", i); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	var stored int
	if err := s.DB.QueryRow(ctx, "SELECT (SELECT count(*) FROM agents)+(SELECT count(*) FROM runs)").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("refused requests stored %d rows: %v", stored, err)
	}
}

// When GitHub fails to answer, the request fails as a server error, not as a
// refusal, and stores nothing. A workflow file name Agbredo cannot keep is
// refused before GitHub is asked.
func TestDatabaseFirstRunWhenGitHubFailsOrTheNameIsBad(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	i := newWorkflowRun(f, 777)
	f.installErr = errors.New("find GitHub App installation: 502 Bad Gateway")
	_, err := s.MintTokenForWorkflow(ctx, "review", i)
	if err == nil || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
		t.Fatalf("GitHub failure mapped to a client error: %v", err)
	}
	lookups := f.lookups
	bad := i
	bad.Workflow = "my agent.yml"
	if _, err = s.MintTokenForWorkflow(ctx, "review", bad); !errors.Is(err, ErrInvalid) || f.lookups != lookups {
		t.Fatalf("bad workflow name: %v, asked GitHub %d times", err, f.lookups-lookups)
	}
	var stored int
	if err = s.DB.QueryRow(ctx, "SELECT (SELECT count(*) FROM agents)+(SELECT count(*) FROM runs)").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("failed requests stored %d rows: %v", stored, err)
	}
}

// A disabled deployment gets no token on either route, and is not replaced.
func TestDatabaseDisabledDeploymentIsRefused(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	f.installations["acme/agent"] = 123
	a.Enabled = false
	if _, err := s.SaveDeployment(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintTokenForWorkflow(ctx, "review", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("new route gave a token to a disabled deployment", err)
	}
	if _, _, err := s.MintToken(ctx, a.ID, "review", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old route gave a token to a disabled deployment", err)
	}
	if n := deploymentCount(t, s); n != 1 {
		t.Fatalf("%d deployments", n)
	}
}

// GitHub ignores case in owner and repository names, and so does Agbredo:
// finding the deployment, checking the job against it, and webhooks.
func TestDatabaseRepositoryNamesIgnoreCase(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f) // registered as acme/agent
	i.Repository = "ACME/Agent"
	g, err := s.MintTokenForWorkflow(ctx, "review", i)
	if err != nil || g.DeploymentID != a.ID || deploymentCount(t, s) != 1 || f.lookups != 0 {
		t.Fatalf("new route: %#v %v, %d lookups", g, err, f.lookups)
	}
	i.Repository = "Acme/AGENT"
	if _, _, err = s.MintToken(ctx, a.ID, "fix", i); err != nil {
		t.Fatal("old route", err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, Ref: "main", Status: "completed", Conclusion: "success"})
	if err = webhook(s, "case", "ACME/AGENT", 777); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, attemptRunID(a.ID, 777, 1)); r.Status != "completed" {
		t.Fatal("webhook in other case ignored", r.Status)
	}
	// The workflow file name does not ignore case.
	f.put(WorkflowRun{ID: 779, Attempt: 1, Workflow: "Agent.yml", Ref: "main", Status: "in_progress"})
	other := Identity{Repository: "acme/agent", Workflow: "Agent.yml", Ref: "refs/heads/main", RunID: 779, RunAttempt: 1, EventName: "push"}
	if _, _, err = s.MintToken(ctx, a.ID, "review", other); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("workflow file matched in another case", err)
	}
}

// A hidden (deleted) deployment is not found: the next run of its workflow
// creates a new one. Disabling, not deleting, stops a deployment.
func TestDatabaseDeletedDeploymentIsCreatedAgain(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	f.installations["acme/agent"] = 123
	if err := s.DeleteDeployment(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	g, err := s.MintTokenForWorkflow(ctx, "review", i)
	if err != nil || g.DeploymentID == a.ID || deploymentCount(t, s) != 1 {
		t.Fatalf("%#v %v", g, err)
	}
}

func TestDefaultName(t *testing.T) {
	if got := defaultName("acme", "agent", "triage.yml"); got != "acme/agent · triage.yml" {
		t.Fatal(got)
	}
	long := defaultName(strings.Repeat("o", 39), strings.Repeat("r", 100), "triage.yml")
	if len(long) > 120 || !strings.HasPrefix(long, strings.Repeat("o", 39)+"/") {
		t.Fatalf("%d bytes: %s", len(long), long)
	}
	a := validDeployment()
	a.Name = long
	if err := ValidateDeployment(a); err != nil {
		t.Fatal(err)
	}
}
