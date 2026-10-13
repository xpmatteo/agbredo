package core

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func ptr[T any](n T) *T { return &n }

func observationAgent(t *testing.T, s *Service, f *fakeBackend) (Agent, Identity) {
	t.Helper()
	a := validAgent()
	a.Mode, a.ContinuationWorkflow = "observe", ""
	a, err := s.SaveAgent(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Title: "Existing agent title", Workflow: a.Workflow, Ref: "main", Status: "in_progress"})
	return a, Identity{Repository: "acme/agent", Workflow: a.Workflow, Ref: "refs/heads/main", RunID: 777, RunAttempt: 1, EventName: "schedule"}
}
func finishedObservation() Observation {
	return Observation{GitHubRunID: 777, GitHubRunAttempt: 1, Source: "agent", Phase: "finished", Version: "2.1.296", ExitCode: ptr(0), TelemetrySupported: true, TelemetryReceived: true,
		Usage:               &ClaudeUsage{InputTokens: ptr(int64(100)), OutputTokens: ptr(int64(10)), CacheReadTokens: ptr(int64(200)), CacheCreationTokens: ptr(int64(20))},
		EstimatedCostMicros: ptr(int64(1_000_001)), APIErrors: 1, RateLimitFailures: 1, APIFailed: true}
}

func TestObservationPreservesIndependentExecutionAndDeduplicatesConsumption(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := observationAgent(t, s, f)
	if _, err := s.Launch(ctx, a.ID, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("observe dispatched: %v", err)
	}
	o := finishedObservation()
	r, inserted, err := s.Observe(ctx, a.ID, o, i)
	if err != nil || !inserted {
		t.Fatal(err)
	}
	if r.Status != "running" || !r.OverBudget || r.CostMicros != 1_000_001 || !r.EstimatedCostAvailable || r.ClaudeUsage == nil || *r.ClaudeUsage.CacheReadTokens != 200 {
		t.Fatalf("wrong totals/lifecycle: %#v", r)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, accepted, e := s.Observe(ctx, a.ID, o, i)
			if e != nil || accepted {
				t.Errorf("retry was counted: %v, %v", accepted, e)
			}
		}()
	}
	wg.Wait()
	r, err = s.Run(ctx, r.ID)
	if err != nil || r.CostMicros != 1_000_001 || r.RateLimitFailures != 1 || r.InputTokens != 100 {
		t.Fatalf("duplicate consumption: %#v %v", r, err)
	}
	events, err := s.Events(ctx, r.ID)
	if err != nil || len(events) != 1 || events[0].Status != "failed" {
		t.Fatalf("process failure was lost: %#v %v", events, err)
	}
	if _, err = s.Ingest(ctx, r.ID, Event{ID: "proposal", CorrelationID: r.CorrelationID, Type: "proposal", Proposal: &Proposal{}}, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("observation acquired managed capability", err)
	}
	if err = s.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	fleet, err := s.Fleet(ctx)
	if err != nil || len(fleet.Approvals) != 0 || f.dispatches != 0 || f.creates != 0 {
		t.Fatal("observation had side effects", err)
	}
	// Webhooks use the independent workflow title and cannot regress a terminal run.
	f.put(WorkflowRun{ID: 777, Attempt: 1, Title: "Existing agent title", Workflow: a.Workflow, Ref: "main", Status: "completed", Conclusion: "failure"})
	if err = s.Webhook(ctx, "observation-delivery", "acme/agent", 777, "Existing agent title"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(ctx, r.ID)
	if r.Status != "failed" {
		t.Fatal("workflow lifecycle not reconciled", r.Status)
	}
}

func TestObservationMissingUsageAndSignedBinding(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := observationAgent(t, s, f)
	o := Observation{GitHubRunID: 777, GitHubRunAttempt: 1, Source: "job", Phase: "finished", Version: "unknown", ExitCode: ptr(0)}
	for _, change := range []func(*Identity){
		func(i *Identity) { i.Repository = "evil/repo" }, func(i *Identity) { i.Workflow = "other.yml" },
		func(i *Identity) { i.Ref = "refs/heads/other" }, func(i *Identity) { i.RunAttempt = 2 },
	} {
		wrong := i
		change(&wrong)
		if _, _, err := s.Observe(ctx, a.ID, o, wrong); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("unbound observation accepted: %v", err)
		}
	}
	r, _, err := s.Observe(ctx, a.ID, o, i)
	if err != nil {
		t.Fatal(err)
	}
	if r.ClaudeUsage != nil || r.EstimatedCostAvailable {
		t.Fatal("missing consumption fabricated as measured zero")
	}
	// A measured zero is available and distinct from missing usage.
	o.Source = "second-step"
	o.Usage = &ClaudeUsage{InputTokens: ptr(int64(0))}
	o.EstimatedCostMicros = ptr(int64(0))
	o.TelemetryReceived = true
	r, _, err = s.Observe(ctx, a.ID, o, i)
	if err != nil || r.ClaudeUsage.InputTokens == nil || r.ClaudeUsage.OutputTokens != nil || !r.EstimatedCostAvailable {
		t.Fatal("zero measurement lost", err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, Ref: "main", Status: "completed", Conclusion: "success"})
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(ctx, r.ID)
	if r.Status != "completed" {
		t.Fatal("successful observed workflow requires an invented managed final event")
	}
	// A rerun has a new immutable run and fresh accounting.
	i.RunAttempt = 2
	o.GitHubRunAttempt = 2
	f.put(WorkflowRun{ID: 777, Attempt: 2, Workflow: a.Workflow, Ref: "main", Status: "in_progress"})
	rerun, _, err := s.Observe(ctx, a.ID, o, i)
	if err != nil || rerun.ID == r.ID || rerun.RunAttempt != 2 {
		t.Fatal("rerun merged into prior attempt", err)
	}
}

func TestObservationValidation(t *testing.T) {
	for _, change := range []func(*Observation){
		func(o *Observation) { o.Source = "a/b" }, func(o *Observation) { o.Version = "secret=value" },
		func(o *Observation) { o.ExitCode = nil }, func(o *Observation) { o.ExitCode = ptr(-1) },
		func(o *Observation) { o.EstimatedCostMicros = ptr(int64(-1)) },
		func(o *Observation) { o.Usage.CacheReadTokens = ptr(int64(-1)) },
		func(o *Observation) { o.RateLimitFailures = 2 }, func(o *Observation) { o.Phase = "started" },
	} {
		o := finishedObservation()
		change(&o)
		if o.validate() == nil {
			t.Fatalf("invalid report accepted: %#v", o)
		}
	}
}

func TestObservationConcurrentFirstReportsKeepOneSnapshot(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := observationAgent(t, s, f)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Observe(ctx, a.ID, finishedObservation(), i); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	fleet, err := s.Fleet(ctx)
	if err != nil || len(fleet.Runs) != 1 || fleet.Runs[0].CostMicros != 1_000_001 {
		t.Fatalf("first reports raced: %#v %v", fleet, err)
	}
	// The existing attempt retains its config when the registry is edited.
	a.Description = "Updated registration"
	a, err = s.SaveAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	o := finishedObservation()
	o.Source = "next-step"
	r, _, err := s.Observe(ctx, a.ID, o, i)
	if err != nil || r.Agent.Version != 1 || r.CostMicros != 2_000_002 {
		t.Fatalf("historical snapshot changed: %#v %v", r, err)
	}
}

func TestNewObservationTriggersCannotUseManagedCapabilities(t *testing.T) {
	s, f := testService(t)
	r, i := launched(t, s, f)
	i.EventName = "schedule"
	_, err := s.Ingest(context.Background(), r.ID, Event{ID: "forbidden", CorrelationID: r.CorrelationID, Type: "status"}, i)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatal("non-dispatch trigger gained managed capability", err)
	}
}

func TestObservationSavedReportCannotMoveToAnotherAttempt(t *testing.T) {
	s, f := testService(t)
	a, i := observationAgent(t, s, f)
	o := finishedObservation()
	i.RunAttempt = 2
	f.put(WorkflowRun{ID: 777, Attempt: 2, Workflow: a.Workflow, Ref: "main", Status: "in_progress"})
	if _, _, err := s.Observe(context.Background(), a.ID, o, i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("saved report moved to a different attempt", err)
	}
	fleet, err := s.Fleet(context.Background())
	if err != nil || len(fleet.Runs) != 0 {
		t.Fatal("invalid replay registered a run", err)
	}
}

func TestObservationExistingActionOutcomeDoesNotFabricateExitCode(t *testing.T) {
	s, f := testService(t)
	a, i := observationAgent(t, s, f)
	o := finishedObservation()
	o.ExitCode, o.ActionOutcome = nil, "failure"
	r, _, err := s.Observe(context.Background(), a.ID, o, i)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(context.Background(), r.ID)
	if err != nil || events[0].Status != "failed" || events[0].Claude.ExitCode != nil || events[0].Claude.ActionOutcome != "failure" {
		t.Fatalf("lost action outcome: %#v %v", events, err)
	}
	o.ActionOutcome = "success"
	if o.validate() != nil {
		t.Fatal("valid action outcome rejected")
	}
	o.ExitCode = ptr(0)
	if o.validate() == nil {
		t.Fatal("ambiguous execution status accepted")
	}
}
