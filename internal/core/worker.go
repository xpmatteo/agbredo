package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type dispatchJob struct {
	ID, RunID  string
	ApprovalID *string
	Attempts   int
}

func (s *Service) Work(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	poll := time.NewTicker(30 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			workCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			if err := s.DispatchOne(workCtx); err != nil {
				logger.Error("dispatch worker", "error", Redact(err.Error()))
			}
			cancel()
		case <-poll.C:
			workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := s.Reconcile(workCtx); err != nil {
				logger.Error("reconcile", "error", Redact(err.Error()))
			}
			cancel()
		}
	}
}
func (s *Service) DispatchOne(ctx context.Context) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var j dispatchJob
	err = tx.QueryRow(ctx, `SELECT id,run_id,approval_id,attempts FROM dispatch_jobs WHERE NOT finished AND available_at<=now() AND (lease_until IS NULL OR lease_until<now()) ORDER BY available_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&j.ID, &j.RunID, &j.ApprovalID, &j.Attempts)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE dispatch_jobs SET lease_until=now()+interval '2 minutes',attempts=attempts+1 WHERE id=$1", j.ID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	r, err := s.Run(ctx, j.RunID)
	if err != nil {
		return err
	}
	inputs := map[string]interface{}{"rilian_run_id": r.ID, "rilian_correlation_id": r.CorrelationID, "rilian_url": s.BaseURL}
	workflow := r.Agent.Workflow
	current, currentErr := s.Agent(ctx, r.Agent.ID)
	valid := currentErr == nil && current.Enabled && current.Version == r.Agent.Version && !Terminal(r.Status)
	if j.ApprovalID != nil {
		a, e := s.Approval(ctx, *j.ApprovalID)
		if e != nil {
			return e
		}
		valid = valid && a.Status == "approved" && a.AttemptedAt == nil && a.ExpiresAt.After(now()) && !r.OverBudget
		workflow = r.Agent.ContinuationWorkflow
		inputs["rilian_approval_id"] = a.ID
		inputs["rilian_payload_hash"] = a.PayloadHash
	}
	if !valid {
		_, err = s.DB.Exec(ctx, "UPDATE dispatch_jobs SET finished=true,lease_until=NULL,last_error='run, agent, or approval is no longer dispatchable' WHERE id=$1", j.ID)
		return err
	}
	// Dispatch can time out after GitHub accepted it. Look for its correlation before retrying.
	if j.Attempts > 0 {
		title := "rilian:" + r.CorrelationID + ":initial"
		if j.ApprovalID != nil {
			title = "rilian:" + r.CorrelationID + ":approval:" + *j.ApprovalID
		}
		items, e := s.Backend.FindRuns(ctx, r.Agent, title, r.CreatedAt)
		if e != nil {
			return e
		}
		if j.ApprovalID != nil {
			a, e := s.Approval(ctx, *j.ApprovalID)
			if e != nil {
				return e
			}
			items = eligibleRuns(items, a)
		}
		if len(items) > 0 {
			if err = s.ApplyLifecycle(ctx, r.ID, items[0], ""); err != nil {
				return err
			}
			_, err = s.DB.Exec(ctx, "UPDATE dispatch_jobs SET finished=true,lease_until=NULL,last_error='' WHERE id=$1", j.ID)
			return err
		}
	}
	dispatchErr := s.Backend.Dispatch(ctx, r.Agent, workflow, inputs)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err = s.DB.Begin(finishCtx)
	if err != nil {
		return err
	}
	defer tx.Rollback(finishCtx)
	r, err = readJSON[Run](finishCtx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", r.ID)
	if err != nil {
		return err
	}
	if dispatchErr == nil {
		_, err = tx.Exec(finishCtx, "UPDATE dispatch_jobs SET finished=true,lease_until=NULL,last_error='' WHERE id=$1", j.ID)
		if err == nil {
			err = audit(finishCtx, tx, r, "workflow.dispatched", "control-plane", "GitHub accepted workflow dispatch; awaiting run correlation")
		}
	} else {
		delay := time.Duration(min(300, 5*(1<<min(j.Attempts, 6)))) * time.Second
		_, err = tx.Exec(finishCtx, "UPDATE dispatch_jobs SET lease_until=NULL,available_at=$2,last_error=$3 WHERE id=$1", j.ID, now().Add(delay), Redact(dispatchErr.Error()))
		if err == nil {
			err = audit(finishCtx, tx, r, "workflow.dispatch_failed", "control-plane", "Dispatch failed and will retry: "+Redact(dispatchErr.Error()))
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit(finishCtx)
}

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
	initialTitle := "rilian:" + r.CorrelationID + ":initial"
	var a Approval
	continuation := gh.Title != initialTitle
	if continuation {
		a, err = readJSON[Approval](ctx, tx, "SELECT document FROM approvals WHERE run_id=$1 FOR UPDATE", r.ID)
		if err != nil {
			return err
		}
		if ignoredRun(a, gh.ID) {
			return ErrUnauthorized
		}
		if gh.Title != "rilian:"+r.CorrelationID+":approval:"+a.ID || gh.Workflow != r.Agent.ContinuationWorkflow {
			return ErrUnauthorized
		}
		if a.GitHubRunID != 0 && a.GitHubRunID != gh.ID {
			return ErrUnauthorized
		}
		a.GitHubRunID = gh.ID
		a.GitHubURL = gh.URL
	} else {
		if gh.Workflow != r.Agent.Workflow {
			return ErrUnauthorized
		}
		if err = bindInitial(&r, Identity{RunID: gh.ID}); err != nil {
			return err
		}
	}
	if !refMatches(r.Agent.Ref, gh.Ref) {
		return ErrUnauthorized
	}
	status := r.Status
	if !continuation {
		if gh.Status == "in_progress" {
			r.Status = Transition(r.Status, "running")
		}
		if gh.Status == "completed" && gh.Conclusion != "success" {
			pending, e := readJSON[Approval](ctx, tx, "SELECT document FROM approvals WHERE run_id=$1 FOR UPDATE", r.ID)
			if e != nil && e != ErrNotFound {
				return e
			}
			next := "failed"
			if gh.Conclusion == "cancelled" {
				next = "cancelled"
			}
			if !Terminal(r.Status) && pending.AttemptedAt == nil {
				r.Status = next
			}
			if e == nil && pending.AttemptedAt == nil && (pending.Status == "pending" || pending.Status == "approved") {
				pending.Status = "expired"
				if err = saveApproval(ctx, tx, pending); err != nil {
					return err
				}
			} else if e != nil && e != ErrNotFound {
				return e
			}
		}
	}
	event := Event{ID: fmt.Sprintf("system:github:%d:%s:%s", gh.ID, gh.Status, gh.Conclusion), RunID: r.ID, CorrelationID: r.CorrelationID, AgentID: r.Agent.ID, AgentVersion: r.Agent.Version, Timestamp: now(), Type: "github.workflow_run", Status: r.Status, Message: "GitHub workflow " + gh.Status + " " + gh.Conclusion, Metadata: mustJSON(map[string]any{"github_run_id": gh.ID, "continuation": continuation})}
	inserted, err := insertEvent(ctx, tx, event)
	if err != nil {
		return err
	}
	if !inserted && r.Status == status {
		// Still commit new webhook delivery IDs to prevent reprocessing.
		return tx.Commit(ctx)
	}
	if continuation {
		if err = saveApproval(ctx, tx, a); err != nil {
			return err
		}
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Webhook(ctx context.Context, delivery, repository string, ghID int64, title string) error {
	pieces := strings.Split(title, ":")
	if len(pieces) < 3 || pieces[0] != "rilian" {
		return nil
	}
	r, err := readJSON[Run](ctx, s.DB, "SELECT document FROM runs WHERE correlation_id=$1", pieces[1])
	if err == ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if repository != r.Agent.Owner+"/"+r.Agent.Repository {
		return ErrUnauthorized
	}
	gh, err := s.Backend.GetRun(ctx, r.Agent, ghID)
	if err != nil {
		return err
	}
	if gh.Title != title {
		return ErrUnauthorized
	}
	return s.ApplyLifecycle(ctx, r.ID, gh, delivery)
}

func (s *Service) Reconcile(ctx context.Context) error {
	var f Fleet
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE status IN ('queued','running','waiting_approval') ORDER BY created_at")
	if err != nil {
		return err
	}
	f.Runs, err = collect[Run](rows)
	if err != nil {
		return err
	}
	rows, err = s.DB.Query(ctx, "SELECT document FROM approvals WHERE status IN ('pending','approved','executing','execution_unknown') ORDER BY created_at")
	if err != nil {
		return err
	}
	f.Approvals, err = collect[Approval](rows)
	if err != nil {
		return err
	}
	// Recover uncertain side effects first, even if the original request's process crashed.
	for _, a := range f.Approvals {
		if (a.Status == "execution_unknown" || a.Status == "executing") && a.AttemptedAt != nil && a.AttemptedAt.Before(now().Add(-time.Minute)) {
			r, e := s.Run(ctx, a.RunID)
			if e != nil {
				return e
			}
			url, e := s.Backend.FindIssue(ctx, r.Agent, a.ID)
			if e != nil {
				continue
			}
			if url != "" {
				if e = s.finishExecution(ctx, a.ID, url, true); e != nil {
					return e
				}
			} else if a.Status == "executing" {
				if e = s.finishExecution(ctx, a.ID, "", false); e != nil {
					return e
				}
			}
		}
		if (a.Status == "pending" || a.Status == "approved") && !a.ExpiresAt.After(now()) {
			if e := s.expire(ctx, a.ID); e != nil {
				return e
			}
		}
	}
	for _, r := range f.Runs {
		if Terminal(r.Status) {
			continue
		}
		if r.GitHubRunID == 0 {
			items, e := s.Backend.FindRuns(ctx, r.Agent, "rilian:"+r.CorrelationID+":initial", r.CreatedAt)
			if e != nil {
				continue
			}
			if len(items) > 0 {
				if e = s.ApplyLifecycle(ctx, r.ID, items[0], ""); e != nil && e != ErrUnauthorized {
					return e
				}
			}
		} else {
			gh, e := s.Backend.GetRun(ctx, r.Agent, r.GitHubRunID)
			if e != nil {
				continue
			}
			if e = s.ApplyLifecycle(ctx, r.ID, gh, ""); e != nil {
				return e
			}
			// Success without a final event is never fabricated into completion.
			if gh.Status == "completed" && gh.Conclusion == "success" && r.Status != "waiting_approval" && r.UpdatedAt.Before(now().Add(-10*time.Minute)) {
				if e = s.missingEvents(ctx, r.ID); e != nil {
					return e
				}
			}
		}
		for _, a := range f.Approvals {
			if a.RunID != r.ID || a.Status != "approved" {
				continue
			}
			if a.GitHubRunID == 0 {
				items, e := s.Backend.FindRuns(ctx, r.Agent, "rilian:"+r.CorrelationID+":approval:"+a.ID, a.CreatedAt)
				items = eligibleRuns(items, a)
				if e == nil && len(items) > 0 {
					_ = s.ApplyLifecycle(ctx, r.ID, items[0], "")
				}
			} else {
				gh, e := s.Backend.GetRun(ctx, r.Agent, a.GitHubRunID)
				if e == nil {
					_ = s.ApplyLifecycle(ctx, r.ID, gh, "")
				}
			}
		}
	}
	return nil
}

func ignoredRun(a Approval, id int64) bool {
	for _, ignored := range a.IgnoredRunIDs {
		if ignored == id {
			return true
		}
	}
	return false
}
func eligibleRuns(items []WorkflowRun, a Approval) []WorkflowRun {
	result := make([]WorkflowRun, 0, len(items))
	for _, item := range items {
		if !ignoredRun(a, item.ID) {
			result = append(result, item)
		}
	}
	return result
}
func (s *Service) expire(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, a, err := s.lockApproval(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.AttemptedAt != nil || a.ExpiresAt.After(now()) || (a.Status != "pending" && a.Status != "approved") {
		return nil
	}
	a.Status = "expired"
	if !Terminal(r.Status) {
		r.Status = "cancelled"
	}
	if err = saveApproval(ctx, tx, a); err != nil {
		return err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	if err = audit(ctx, tx, r, "approval.expired", "control-plane", "Approval expired without execution"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) missingEvents(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", id)
	if err != nil {
		return err
	}
	if Terminal(r.Status) || r.Status == "waiting_approval" {
		return nil
	}
	r.Status = "failed"
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	if err = audit(ctx, tx, r, "run.missing_events", "control-plane", "GitHub completed successfully but no final execution event arrived within 10 minutes"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
