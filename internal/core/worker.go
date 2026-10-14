package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Work periodically reconciles open runs with GitHub, covering lost webhooks.
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
		}
	}
}

// ApplyLifecycle records a GitHub workflow state for a reported run. A nonempty
// delivery ID makes webhook redelivery harmless.
func (s *Service) ApplyLifecycle(ctx context.Context, runID string, gh WorkflowRun, delivery string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", runID)
	if err != nil {
		return err
	}
	if delivery != "" {
		tag, e := tx.Exec(ctx, "INSERT INTO webhook_deliveries(id) VALUES($1) ON CONFLICT DO NOTHING", delivery)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
	}
	if gh.ID != r.GitHubRunID || gh.Attempt != r.RunAttempt || gh.Workflow != r.Deployment.Workflow || !refMatches(r.Deployment.Ref, gh.Ref) {
		return ErrUnauthorized
	}
	r.Status = reportedStatus(r.Status, gh)
	_, err = insertEvent(ctx, tx, Event{ID: fmt.Sprintf("system:github:%d:%d:%s:%s", gh.ID, gh.Attempt, gh.Status, gh.Conclusion), RunID: r.ID, CorrelationID: r.CorrelationID, DeploymentID: r.Deployment.ID, DeploymentRevision: r.Deployment.Revision, Timestamp: now(), Type: "github.workflow_run", Status: r.Status, Message: "GitHub workflow " + gh.Status + " " + gh.Conclusion})
	if err != nil {
		return err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Webhook updates only runs that were previously reported by their workflow.
// Existing run names carry no Agbredo correlation.
func (s *Service) Webhook(ctx context.Context, delivery, repository string, ghID int64) error {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE document->>'github_run_id'=$1", fmt.Sprint(ghID))
	if err != nil {
		return err
	}
	reported, err := collect[Run](rows)
	if err != nil {
		return err
	}
	for _, r := range reported {
		if repository != r.Deployment.Owner+"/"+r.Deployment.Repository {
			continue
		}
		gh, e := s.Backend.GetRun(ctx, r.Deployment, ghID)
		if e != nil {
			return e
		}
		if gh.Attempt != r.RunAttempt {
			continue
		}
		if e = s.ApplyLifecycle(ctx, r.ID, gh, delivery); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) Reconcile(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE status IN ('queued','running') ORDER BY created_at")
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
		if e = s.ApplyLifecycle(ctx, r.ID, gh, ""); e != nil {
			return e
		}
	}
	return nil
}
