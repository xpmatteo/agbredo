package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Work periodically reconciles open runs with GitHub, covering lost
// webhooks, and fetches the jobs of finished runs. A webhook that finishes a
// run wakes it to fetch that run's jobs at once.
func (s *Service) Work(ctx context.Context, logger *slog.Logger) {
	poll := time.NewTicker(30 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := s.Reconcile(workCtx); err != nil {
				logger.Error("reconcile", "error", Redact(err.Error()))
			}
			cancel()
		case <-s.jobsSignal():
			workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := s.FetchJobs(workCtx); err != nil {
				logger.Error("fetch jobs", "error", Redact(err.Error()))
			}
			cancel()
		}
	}
}

// ApplyLifecycle records a GitHub workflow state, read by the poll, on a run
// Agbredo knows.
func (s *Service) ApplyLifecycle(ctx context.Context, runID string, gh WorkflowRun) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", runID)
	if err != nil {
		return err
	}
	if gh.ID != r.GitHubRunID || gh.Attempt != r.RunAttempt || gh.Workflow != r.Deployment.Workflow {
		return ErrUnauthorized
	}
	if err = applyWorkflowRun(ctx, tx, r, gh); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// applyWorkflowRun records what GitHub says of a run attempt on its run: the
// status, which never leaves a finished state, and one event per status. It
// also fills in what the run is missing; it never changes what it has.
func applyWorkflowRun(ctx context.Context, tx pgx.Tx, r Run, gh WorkflowRun) error {
	r.Status = statusFromGitHub(r.Status, gh)
	fill := func(field *string, value string) {
		if *field == "" {
			*field = value
		}
	}
	fill(&r.HeadBranch, gh.HeadBranch)
	if commitSHA.MatchString(gh.HeadSHA) {
		fill(&r.HeadSHA, gh.HeadSHA)
	}
	fill(&r.TriggerEvent, gh.Event)
	fill(&r.Actor, gh.Actor)
	fill(&r.TriggeringActor, gh.TriggeringActor)
	if r.GitHubStartedAt.IsZero() {
		r.GitHubStartedAt = gh.StartedAt
	}
	// Set again by each GitHub state, so an approval, or any other news of the run, clears it.
	r.AwaitingApproval = r.Status == "queued" && awaitsApproval(gh)
	if r.GitHubCompletedAt.IsZero() && finished(gh) {
		r.GitHubCompletedAt = gh.UpdatedAt
	}
	_, err := insertEvent(ctx, tx, Event{ID: fmt.Sprintf("system:github:%d:%d:%s:%s", gh.ID, gh.Attempt, gh.Status, gh.Conclusion), RunID: r.ID, CorrelationID: r.CorrelationID, DeploymentID: r.Deployment.ID, DeploymentRevision: r.Deployment.Revision, Timestamp: now(), Type: "github.workflow_run", Status: r.Status, Message: "GitHub workflow " + gh.Status + " " + gh.Conclusion})
	if err != nil {
		return err
	}
	return saveRun(ctx, tx, r)
}

// Delivery is one GitHub webhook delivery that passed the signature check.
// Its raw payload goes to the telemetry store; webhook_deliveries keeps these fields.
type Delivery struct {
	ID         string // X-GitHub-Delivery
	Event      string // X-GitHub-Event
	Action     string
	Repository string // owner/repository, as the payload spells it
	// InstallationID is the GitHub App installation; 0 if the payload names none.
	InstallationID int64
}

// Outcomes of a webhook delivery, in webhook_deliveries.
const (
	DeliveryHandled = "handled" // applied to a run Agbredo knows
	DeliveryIgnored = "ignored" // nothing to do: another event, or a run Agbredo does not know
	DeliveryFailed  = "failed"  // processing failed; a redelivery is processed again
)

// RecordDelivery indexes a delivery as ignored or failed, with the reason it failed.
// It never overwrites a handled delivery. A failed one becomes ignored when a
// redelivery has nothing to do; an ignored one becomes failed when a redelivery fails.
func (s *Service) RecordDelivery(ctx context.Context, d Delivery, outcome string, problem error) error {
	var text *string
	if problem != nil {
		t := Redact(problem.Error())
		if len(t) > 500 {
			t = strings.ToValidUTF8(t[:500], "") // a cut may split a character
		}
		text = &t
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO webhook_deliveries(id, event, action, repository, installation_id, outcome, error)
		VALUES($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,0), $6, $7)
		ON CONFLICT (id) DO UPDATE SET event=EXCLUDED.event, action=EXCLUDED.action, repository=EXCLUDED.repository,
			installation_id=EXCLUDED.installation_id, outcome=EXCLUDED.outcome, error=EXCLUDED.error
		WHERE webhook_deliveries.outcome = 'failed' OR (EXCLUDED.outcome = 'failed' AND webhook_deliveries.outcome = 'ignored')`,
		d.ID, d.Event, d.Action, d.Repository, d.InstallationID, outcome, text)
	return err
}

// Webhook records a workflow_run delivery. GitHub signed the payload, so its
// facts are used as they are; nothing is asked of GitHub. The run is recorded
// under the live deployment of its repository and workflow file, disabled or
// not, and created if it is new. Runs of deleted deployments that Agbredo
// already has are updated too. With no such deployment and no such run, the
// delivery is ignored. Webhooks never create deployments, and recording a run
// grants it nothing: see lockRun.
//
// The delivery is indexed as handled, ignored or failed. A handled delivery is
// not applied again.
func (s *Service) Webhook(ctx context.Context, d Delivery, gh WorkflowRun) error {
	handled, err := s.webhook(ctx, d, gh)
	outcome := DeliveryIgnored
	if err != nil {
		outcome = DeliveryFailed
	} else if handled {
		if finished(gh) {
			s.jobsMayBeDue() // the worker asks GitHub; the delivery does not wait
		}
		return nil // indexed with the run, in its transaction
	}
	// The request may have timed out; the index is still written.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.RecordDelivery(recordCtx, d, outcome, err); e != nil {
		return errors.Join(err, e)
	}
	return err
}

func (s *Service) webhook(ctx context.Context, d Delivery, gh WorkflowRun) (bool, error) {
	owner, repository, ok := strings.Cut(d.Repository, "/")
	if !ok || gh.ID <= 0 || gh.Attempt <= 0 || gh.Workflow == "" {
		return false, invalid("invalid workflow_run payload")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if d.ID != "" {
		// A failed or ignored delivery may be handled when GitHub sends it again.
		// Rows from before outcomes were kept have none, and were all handled.
		tag, e := tx.Exec(ctx, `INSERT INTO webhook_deliveries(id, event, action, repository, installation_id, outcome)
			VALUES($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,0), 'handled')
			ON CONFLICT (id) DO UPDATE SET event=EXCLUDED.event, action=EXCLUDED.action, repository=EXCLUDED.repository,
				installation_id=EXCLUDED.installation_id, outcome='handled', error=NULL
			WHERE webhook_deliveries.outcome IN ('failed','ignored')`, d.ID, d.Event, d.Action, d.Repository, d.InstallationID)
		if e != nil {
			return false, e
		}
		if tag.RowsAffected() == 0 {
			return true, nil // handled before
		}
	}
	a, err := findDeployment(ctx, tx, owner, repository, gh.Workflow)
	switch {
	case err == nil:
		// The same row as the token path's (lockRun): a token request and a
		// webhook at the same time end with one run. The loser of the insert
		// waits for the winner, then finds its row below.
		r := newRunFromWebhook(a, gh)
		if _, err = tx.Exec(ctx, "INSERT INTO runs(id,agent_id,correlation_id,status,document) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", r.ID, a.ID, r.ID, r.Status, mustJSON(r)); err != nil {
			return false, err
		}
	case !errors.Is(err, ErrNotFound):
		return false, err
	}
	// Locked in ID order, so that two deliveries for the same run attempt wait for each other.
	rows, err := tx.Query(ctx, "SELECT document FROM runs WHERE document->>'github_run_id'=$1 AND document->>'run_attempt'=$2 ORDER BY id FOR UPDATE", fmt.Sprint(gh.ID), fmt.Sprint(gh.Attempt))
	if err != nil {
		return false, err
	}
	runs, err := collect[Run](rows)
	if err != nil {
		return false, err
	}
	handled := false
	for _, r := range runs {
		if !sameRepository(d.Repository, r.Deployment.Owner+"/"+r.Deployment.Repository) || gh.Workflow != r.Deployment.Workflow {
			continue
		}
		if err = applyWorkflowRun(ctx, tx, r, gh); err != nil {
			return false, err
		}
		handled = true
	}
	if !handled {
		return false, nil
	}
	return true, tx.Commit(ctx)
}

// newRunFromWebhook is a run first seen in a workflow_run webhook, before any
// of its jobs asked for a telemetry token. It has no ref, which only the OIDC
// identity gives; the first token request adds it (see lockRun).
//
// A run of a pull request event (pull_request, pull_request_review,
// pull_request_review_comment) runs the workflow file of the pull request,
// which the author of a fork controls, so its referenced workflows say
// nothing reliable: it records no pipeline. pull_request_target runs the base
// repository's file, and keeps it. The token path refuses pull request events
// and refs for the same reason.
func newRunFromWebhook(a Deployment, gh WorkflowRun) Run {
	id := attemptRunID(a.ID, gh.ID, gh.Attempt)
	r := Run{ID: id, CorrelationID: id, Deployment: a, GitHubRunID: gh.ID, RunAttempt: gh.Attempt, Status: "queued", ThresholdMicros: a.ThresholdMicros,
		TriggerEvent: gh.Event, Actor: gh.Actor, TriggeringActor: gh.TriggeringActor, CreatedAt: now(), UpdatedAt: now()}
	if !runsPullRequestWorkflow(gh.Event) {
		r.Pipeline = PipelineOfRun(gh)
	}
	r.GitHubURL = fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", a.Owner, a.Repository, gh.ID)
	if strings.HasPrefix(gh.URL, "https://github.com/") {
		r.GitHubURL = gh.URL
	}
	r.Status = statusFromGitHub(r.Status, gh)
	return r
}

// runsPullRequestWorkflow says whether a run of this event runs the workflow
// file of a pull request rather than the base repository's.
func runsPullRequestWorkflow(event string) bool {
	return strings.HasPrefix(event, "pull_request") && event != "pull_request_target"
}

// Reconcile polls GitHub for the runs that have not finished. A run waiting
// for approval is left out: it may wait for days, and the approval's webhook
// moves it on. Then it fetches the jobs of finished runs (FetchJobs).
func (s *Service) Reconcile(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE status IN ('queued','running') AND document->>'awaiting_approval' IS DISTINCT FROM 'true' ORDER BY created_at")
	if err != nil {
		return err
	}
	runs, err := collect[Run](rows)
	if err != nil {
		return err
	}
	for _, r := range runs {
		gh, e := s.Backend.GetRunAttempt(ctx, r.Deployment, r.GitHubRunID, r.RunAttempt)
		if e != nil {
			continue
		}
		if e = s.ApplyLifecycle(ctx, r.ID, gh); e != nil {
			return e
		}
	}
	return s.FetchJobs(ctx)
}
