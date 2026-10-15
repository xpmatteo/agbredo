package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

const headSHA = "4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a"

// auditRun is GitHub's description of an attempt of the deployment's
// workflow, as a workflow_run webhook gives it. It calls a pipeline at the tag v1.
func auditRun(id int64, status, conclusion string) WorkflowRun {
	started := time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)
	return WorkflowRun{ID: id, Attempt: 1, Workflow: "agent.yml", Status: status, Conclusion: conclusion,
		URL: fmt.Sprintf("https://github.com/acme/agent/actions/runs/%d", id), HeadBranch: "v1.2", HeadSHA: headSHA, Event: "schedule",
		StartedAt: started, UpdatedAt: started.Add(17 * time.Second), Actor: "matt", TriggeringActor: "matt",
		Referenced: []ReferencedWorkflow{{Path: "org/pipelines/.github/workflows/audit.yml@v1", SHA: pipelineSHA}}}
}

func eventCount(t *testing.T, s *Service, runID string) int {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

// A run of a deployment that asks for no telemetry token is recorded from its
// webhooks alone, with what the payload says. Measurements stay missing.
func TestDatabaseWebhookRecordsARunWithoutTelemetry(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "requested", "acme/agent", auditRun(777, "queued", "")); err != nil {
		t.Fatal(err)
	}
	id := attemptRunID(a.ID, 777, 1)
	r, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "requested"); outcome != DeliveryHandled {
		t.Fatal(outcome)
	}
	want := &Pipeline{Path: "org/pipelines/.github/workflows/audit.yml", Ref: "v1", SHA: pipelineSHA}
	if r.Status != "queued" || r.GitHubRunID != 777 || r.RunAttempt != 1 || r.GitHubURL != "https://github.com/acme/agent/actions/runs/777" ||
		r.TriggerEvent != "schedule" || r.Actor != "matt" || r.TriggeringActor != "matt" || r.HeadBranch != "v1.2" || r.HeadSHA != headSHA ||
		!r.GitHubStartedAt.Equal(time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)) || !r.GitHubCompletedAt.IsZero() ||
		r.Deployment.ID != a.ID || r.Deployment.Revision != 1 || r.ThresholdMicros != a.ThresholdMicros || !reflect.DeepEqual(r.Pipeline, want) {
		t.Fatalf("%#v", r)
	}
	// Only the OIDC identity gives a ref. Telemetry measurements stay missing.
	if r.Ref != "" || r.EstimatedCostAvailable || r.ClaudeUsage != nil || r.AgentRuns != nil {
		t.Fatalf("a run without telemetry has %q %v %v %v", r.Ref, r.EstimatedCostAvailable, r.ClaudeUsage, r.AgentRuns)
	}

	if err = webhook(s, "in-progress", "acme/agent", auditRun(777, "in_progress", "")); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, id); r.Status != "running" {
		t.Fatal(r.Status)
	}
	if err = webhook(s, "completed", "acme/agent", auditRun(777, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, id); r.Status != "completed" || !r.GitHubCompletedAt.Equal(time.Date(2026, 10, 15, 7, 8, 17, 0, time.UTC)) {
		t.Fatal(r.Status, r.GitHubCompletedAt)
	}
	// A late delivery does not reopen it, and does not move its completion.
	late := auditRun(777, "in_progress", "")
	late.UpdatedAt = late.UpdatedAt.Add(time.Hour)
	if err = webhook(s, "late", "acme/agent", late); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, id); r.Status != "completed" || !r.GitHubCompletedAt.Equal(time.Date(2026, 10, 15, 7, 8, 17, 0, time.UTC)) {
		t.Fatal("reopened", r.Status, r.GitHubCompletedAt)
	}
	// One event per status: the late one repeats one already seen.
	if n := eventCount(t, s, id); n != 3 {
		t.Fatalf("%d events", n)
	}
	// Runs triggered by pull requests are recorded too; they still get no
	// token. Their pipeline is not recorded: a fork's author controls the
	// workflow file of a pull_request run.
	pr := auditRun(778, "completed", "success")
	pr.Event = "pull_request"
	if err = webhook(s, "pr", "acme/agent", pr); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, attemptRunID(a.ID, 778, 1)); r.TriggerEvent != "pull_request" || r.Status != "completed" || r.Pipeline != nil {
		t.Fatal(r.TriggerEvent, r.Status, r.Pipeline)
	}
}

// GitHub's conclusions: skipped and neutral runs are completed, not failed. A
// run that waits for a maintainer's approval (action_required) is not
// finished: it stays queued, and the approval moves it on.
func TestDatabaseWebhookStatusFromConclusions(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	for n, c := range []struct{ conclusion, want string }{
		{"success", "completed"}, {"skipped", "completed"}, {"neutral", "completed"}, {"cancelled", "cancelled"},
		{"failure", "failed"}, {"timed_out", "failed"}, {"startup_failure", "failed"}, {"action_required", "queued"},
	} {
		id := int64(800 + n)
		if err = webhook(s, c.conclusion, "acme/agent", auditRun(id, "completed", c.conclusion)); err != nil {
			t.Fatal(err)
		}
		r, _ := s.Run(ctx, attemptRunID(a.ID, id, 1))
		if r.Status != c.want || r.GitHubCompletedAt.IsZero() != (c.want == "queued") {
			t.Fatalf("%s: status %s, completed at %v", c.conclusion, r.Status, r.GitHubCompletedAt)
		}
	}
	approved := auditRun(807, "in_progress", "")
	if err = webhook(s, "approved", "acme/agent", approved); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, attemptRunID(a.ID, 807, 1)); r.Status != "running" {
		t.Fatal("approval did not move the run on", r.Status)
	}
}

// The webhook and the token request meet on the same run. The first token
// then records the OIDC identity, which is more exact; later ones change
// nothing.
func TestDatabaseTokenAfterWebhookFindsTheRunAndSetsOIDC(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, HeadBranch: "main", Status: "in_progress", Actor: "matt", TriggeringActor: "bot"})
	gh := auditRun(777, "in_progress", "")
	gh.Event, gh.Actor, gh.TriggeringActor = "push", "", ""
	if err := webhook(s, "first", "acme/agent", gh); err != nil {
		t.Fatal(err)
	}
	i.JobWorkflowRef, i.JobWorkflowSHA = "org/pipelines/.github/workflows/audit.yml@refs/tags/v1", pipelineSHA
	r := startRun(t, s, a.ID, "review", i)
	want := &Pipeline{Path: "org/pipelines/.github/workflows/audit.yml", Ref: "refs/tags/v1", SHA: pipelineSHA}
	if r.ID != attemptRunID(a.ID, 777, 1) || r.Ref != "refs/heads/main" || r.TriggerEvent != "schedule" || !reflect.DeepEqual(r.Pipeline, want) ||
		r.Actor != "matt" || r.TriggeringActor != "bot" || r.HeadBranch != "v1.2" || r.Status != "running" {
		t.Fatalf("%#v", r)
	}
	later := i
	later.Ref, later.EventName, later.JobWorkflowRef = "refs/heads/other", "push", "org/pipelines/.github/workflows/audit.yml@refs/tags/v2"
	if r = startRun(t, s, a.ID, "fix", later); r.Ref != "refs/heads/main" || r.TriggerEvent != "schedule" || !reflect.DeepEqual(r.Pipeline, want) {
		t.Fatalf("a later token changed the run: %#v", r)
	}
	// A webhook after the token keeps the OIDC facts.
	if err := webhook(s, "second", "acme/agent", auditRun(777, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Run(ctx, r.ID); r.Status != "completed" || r.TriggerEvent != "schedule" || !reflect.DeepEqual(r.Pipeline, want) || r.Actor != "matt" {
		t.Fatalf("%#v", r)
	}
	if fleet, _ := s.Fleet(ctx); len(fleet.Runs) != 1 {
		t.Fatalf("%d runs", len(fleet.Runs))
	}
}

// A run token-requested first also gets the webhook's facts it has no other
// source for, and keeps its own.
func TestDatabaseWebhookFillsATokenRun(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "review", i)
	if err := webhook(s, "after", "acme/agent", auditRun(777, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(ctx, r.ID)
	if r.HeadBranch != "v1.2" || r.HeadSHA != headSHA || r.GitHubStartedAt.IsZero() || r.GitHubCompletedAt.IsZero() ||
		r.Ref != "refs/heads/main" || r.TriggerEvent != "schedule" || r.Pipeline != nil {
		t.Fatalf("%#v", r)
	}
}

// Webhooks and token requests for the same new run attempt, all at once, end
// with one run, which has the OIDC identity.
func TestDatabaseWebhookAndTokenRaceKeepOneRun(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n%2 == 0 {
				if err := webhook(s, fmt.Sprintf("d-%d", n), "acme/agent", auditRun(777, "in_progress", "")); err != nil {
					t.Error(err)
				}
				return
			}
			if _, _, err := s.MintToken(ctx, a.ID, fmt.Sprintf("step-%d", n), i); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	fleet, err := s.Fleet(ctx)
	if err != nil || len(fleet.Runs) != 1 {
		t.Fatalf("%d runs, %v", len(fleet.Runs), err)
	}
	if r := fleet.Runs[0]; r.Ref != "refs/heads/main" || r.TriggerEvent != "schedule" || r.HeadBranch != "v1.2" || r.Status != "running" {
		t.Fatalf("%#v", r)
	}
	for n := 0; n < 8; n += 2 {
		if outcome, _ := deliveryOutcome(t, s, fmt.Sprintf("d-%d", n)); outcome != DeliveryHandled {
			t.Fatal(n, outcome)
		}
	}
}

// A disabled deployment's runs are recorded, but recording grants nothing:
// such a run gets no telemetry token on either route. A run that got a token
// before the deployment was disabled may get more, as before.
func TestDatabaseRecordingGrantsNothing(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	f.installations["acme/agent"] = 123
	earlier := Identity{Repository: "acme/agent", Workflow: a.Workflow, Ref: "refs/heads/main", RunID: 776, RunAttempt: 1, EventName: "push"}
	f.put(WorkflowRun{ID: 776, Attempt: 1, Workflow: a.Workflow, Status: "in_progress"})
	startRun(t, s, a.ID, "review", earlier)
	a.Enabled = false
	a, err := s.SaveDeployment(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "disabled", "acme/agent", auditRun(777, "in_progress", "")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, attemptRunID(a.ID, 777, 1))
	if err != nil || r.Status != "running" || r.Deployment.Enabled {
		t.Fatalf("disabled deployment's run not recorded: %#v %v", r, err)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "review", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old route gave a token to a recorded run of a disabled deployment", err)
	}
	if _, err = s.MintTokenForWorkflow(ctx, "review", i); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("new route gave a token to a recorded run of a disabled deployment", err)
	}
	var tokens int
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM run_tokens WHERE run_id=$1", r.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatal(tokens, err)
	}
	if r, _ = s.Run(ctx, r.ID); r.Ref != "" {
		t.Fatal("a refused request recorded its identity", r.Ref)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "fix", earlier); err != nil {
		t.Fatal("a run with a token lost it when the deployment was disabled", err)
	}
	// Enabled again, the recorded run gets its token.
	a.Enabled = true
	if _, err = s.SaveDeployment(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "review", i); err != nil {
		t.Fatal(err)
	}
}

// A deleted deployment's new runs are not recorded. Its runs that Agbredo
// has are still updated.
func TestDatabaseWebhookOfDeletedDeployment(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, i := registeredDeployment(t, s, f)
	r := startRun(t, s, a.ID, "review", i)
	if err := s.DeleteDeployment(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := webhook(s, "new", "acme/agent", auditRun(778, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "new"); outcome != DeliveryIgnored {
		t.Fatal(outcome)
	}
	if err := webhook(s, "known", "acme/agent", auditRun(777, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "known"); outcome != DeliveryHandled {
		t.Fatal(outcome)
	}
	if read, _ := s.Run(ctx, r.ID); read.Status != "completed" {
		t.Fatal(read.Status)
	}
	if fleet, _ := s.Fleet(ctx); len(fleet.Runs) != 1 {
		t.Fatalf("%d runs", len(fleet.Runs))
	}
}

// The poll covers lost deliveries of runs recorded from webhooks too.
func TestDatabasePollFinishesARunRecordedFromAWebhook(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "requested", "acme/agent", auditRun(777, "queued", "")); err != nil {
		t.Fatal(err)
	}
	f.put(auditRun(777, "completed", "cancelled"))
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, attemptRunID(a.ID, 777, 1)); r.Status != "cancelled" || r.GitHubCompletedAt.IsZero() {
		t.Fatal(r.Status, r.GitHubCompletedAt)
	}
}
