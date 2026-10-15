package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// ApplyLifecycle records a GitHub workflow state for a run Agbredo knows. With a
// delivery ID, the delivery is indexed as handled in the same transaction, so a
// redelivery of a handled delivery changes nothing. The poll passes no delivery.
func (s *Service) ApplyLifecycle(ctx context.Context, runID string, gh WorkflowRun, d Delivery) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", runID)
	if err != nil {
		return err
	}
	if d.ID != "" {
		// A failed or ignored delivery may be handled when GitHub sends it again.
		// Rows from before outcomes were kept have none, and were all handled.
		tag, e := tx.Exec(ctx, `INSERT INTO webhook_deliveries(id, event, action, repository, installation_id, outcome)
			VALUES($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,0), 'handled')
			ON CONFLICT (id) DO UPDATE SET event=EXCLUDED.event, action=EXCLUDED.action, repository=EXCLUDED.repository,
				installation_id=EXCLUDED.installation_id, outcome='handled', error=NULL
			WHERE webhook_deliveries.outcome IN ('failed','ignored')`, d.ID, d.Event, d.Action, d.Repository, d.InstallationID)
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
	}
	if gh.ID != r.GitHubRunID || gh.Attempt != r.RunAttempt || gh.Workflow != r.Deployment.Workflow {
		return ErrUnauthorized
	}
	r.Status = statusFromGitHub(r.Status, gh)
	_, err = insertEvent(ctx, tx, Event{ID: fmt.Sprintf("system:github:%d:%d:%s:%s", gh.ID, gh.Attempt, gh.Status, gh.Conclusion), RunID: r.ID, CorrelationID: r.CorrelationID, DeploymentID: r.Deployment.ID, DeploymentRevision: r.Deployment.Revision, Timestamp: now(), Type: "github.workflow_run", Status: r.Status, Message: "GitHub workflow " + gh.Status + " " + gh.Conclusion})
	if err != nil {
		return err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// Webhook updates only runs that Agbredo already knows: their workflow asked for a telemetry token.
// Existing run names carry no Agbredo correlation. The delivery is indexed as
// handled, ignored (no known run) or failed.
func (s *Service) Webhook(ctx context.Context, d Delivery, ghID int64) error {
	handled, err := s.webhook(ctx, d, ghID)
	outcome := DeliveryIgnored
	if err != nil {
		outcome = DeliveryFailed
	} else if handled {
		return nil // indexed by ApplyLifecycle
	}
	// The request may have timed out; the index is still written.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.RecordDelivery(recordCtx, d, outcome, err); e != nil {
		return errors.Join(err, e)
	}
	return err
}

func (s *Service) webhook(ctx context.Context, d Delivery, ghID int64) (bool, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE document->>'github_run_id'=$1", fmt.Sprint(ghID))
	if err != nil {
		return false, err
	}
	known, err := collect[Run](rows)
	if err != nil {
		return false, err
	}
	handled := false
	for _, r := range known {
		if !sameRepository(d.Repository, r.Deployment.Owner+"/"+r.Deployment.Repository) {
			continue
		}
		gh, e := s.Backend.GetRun(ctx, r.Deployment, ghID)
		if e != nil {
			return false, e
		}
		if gh.Attempt != r.RunAttempt {
			continue
		}
		if e = s.ApplyLifecycle(ctx, r.ID, gh, d); e != nil {
			return false, e
		}
		handled = true
	}
	return handled, nil
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
		if e = s.ApplyLifecycle(ctx, r.ID, gh, Delivery{}); e != nil {
			return e
		}
	}
	return nil
}
