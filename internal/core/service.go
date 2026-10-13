package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	DB          *pgxpool.Pool
	Backend     Backend
	BaseURL     string
	ApprovalTTL time.Duration
	Admins      []string
}

func (s *Service) allowed(actor string) bool {
	for _, a := range s.Admins {
		if strings.EqualFold(actor, a) && actor != "" {
			return true
		}
	}
	return false
}
func (s *Service) Agent(ctx context.Context, id string) (Agent, error) {
	return readJSON[Agent](ctx, s.DB, "SELECT document FROM agents WHERE id=$1 AND NOT deleted", id)
}
func (s *Service) Run(ctx context.Context, id string) (Run, error) {
	return readJSON[Run](ctx, s.DB, "SELECT document FROM runs WHERE id=$1", id)
}
func (s *Service) Approval(ctx context.Context, id string) (Approval, error) {
	return readJSON[Approval](ctx, s.DB, "SELECT document FROM approvals WHERE id=$1", id)
}
func (s *Service) SaveAgent(ctx context.Context, a Agent) (Agent, error) {
	if err := ValidateAgent(a); err != nil {
		return a, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	if a.ID == "" {
		a.ID = ID()
		a.Version = 1
		_, err = tx.Exec(ctx, "INSERT INTO agents(id,document) VALUES($1,$2)", a.ID, mustJSON(a))
	} else {
		previous, e := readJSON[Agent](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR UPDATE", a.ID)
		if e != nil {
			return a, e
		}
		if a.Version != previous.Version {
			return a, fmt.Errorf("agent was updated by another user; reload its configuration: %w", ErrConflict)
		}
		a.Version = previous.Version + 1
		_, err = tx.Exec(ctx, "UPDATE agents SET document=$2 WHERE id=$1", a.ID, mustJSON(a))
	}
	if err != nil {
		return a, err
	}
	return a, tx.Commit(ctx)
}
func (s *Service) DeleteAgent(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, "UPDATE agents SET deleted=true WHERE id=$1 AND NOT deleted", id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
func (s *Service) Launch(ctx context.Context, id, actor string) (Run, error) {
	if !s.allowed(actor) {
		return Run{}, ErrUnauthorized
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx)
	a, err := readJSON[Agent](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR SHARE", id)
	if err != nil {
		return Run{}, err
	}
	if !a.Enabled {
		return Run{}, fmt.Errorf("agent is disabled: %w", ErrConflict)
	}
	r := Run{ID: ID(), CorrelationID: ID(), Agent: a, Status: "queued", BudgetMicros: a.BudgetMicros, CreatedAt: now(), UpdatedAt: now()}
	if _, err = tx.Exec(ctx, "INSERT INTO runs(id,agent_id,correlation_id,status,document) VALUES($1,$2,$3,$4,$5)", r.ID, a.ID, r.CorrelationID, r.Status, mustJSON(r)); err != nil {
		return r, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO dispatch_jobs(id,run_id) VALUES($1,$2)", r.ID+":initial", r.ID); err != nil {
		return r, err
	}
	if err = audit(ctx, tx, r, "run.launched", actor, "Workflow dispatch queued"); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}
func (s *Service) Fleet(ctx context.Context) (Fleet, error) {
	var f Fleet
	rows, err := s.DB.Query(ctx, "SELECT document FROM agents WHERE NOT deleted ORDER BY document->>'name'")
	if err != nil {
		return f, err
	}
	f.Agents, err = collect[Agent](rows)
	if err != nil {
		return f, err
	}
	rows, err = s.DB.Query(ctx, "SELECT document FROM runs ORDER BY created_at DESC")
	if err != nil {
		return f, err
	}
	f.Runs, err = collect[Run](rows)
	if err != nil {
		return f, err
	}
	rows, err = s.DB.Query(ctx, "SELECT document FROM approvals ORDER BY created_at DESC")
	if err != nil {
		return f, err
	}
	f.Approvals, err = collect[Approval](rows)
	return f, err
}
func collect[T any](rows pgx.Rows) ([]T, error) {
	defer rows.Close()
	items := []T{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (s *Service) Events(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM events WHERE run_id=$1 ORDER BY received_at,id", id)
	if err != nil {
		return nil, err
	}
	return collect[Event](rows)
}

func (s *Service) verifyRunner(ctx context.Context, r Run, a *Approval, identity Identity) error {
	workflow := r.Agent.Workflow
	title := "agbredo:" + r.CorrelationID + ":initial"
	if a != nil {
		if ignoredRun(*a, identity.RunID) {
			return ErrUnauthorized
		}
		workflow = r.Agent.ContinuationWorkflow
		title = "agbredo:" + r.CorrelationID + ":approval:" + a.ID
	}
	if identity.Repository != r.Agent.Owner+"/"+r.Agent.Repository || identity.Workflow != workflow || !refMatches(r.Agent.Ref, identity.Ref) || identity.RunID <= 0 {
		return ErrUnauthorized
	}
	gh, err := s.Backend.GetRun(ctx, r.Agent, identity.RunID)
	if err != nil {
		return fmt.Errorf("verify GitHub workflow run: %w", err)
	}
	if gh.Title != title || gh.Workflow != workflow || !refMatches(r.Agent.Ref, gh.Ref) {
		return ErrUnauthorized
	}
	return nil
}
func bindInitial(r *Run, identity Identity) error {
	if r.GitHubRunID != 0 && r.GitHubRunID != identity.RunID {
		return ErrUnauthorized
	}
	r.GitHubRunID = identity.RunID
	r.GitHubURL = fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", r.Agent.Owner, r.Agent.Repository, identity.RunID)
	return nil
}
func (s *Service) Ingest(ctx context.Context, id string, e Event, identity Identity) (bool, error) {
	snapshot, err := s.Run(ctx, id)
	if err != nil {
		return false, err
	}
	var continuation *Approval
	if e.ApprovalID != "" {
		a, lookupErr := s.Approval(ctx, e.ApprovalID)
		if lookupErr != nil {
			return false, lookupErr
		}
		if a.RunID != id || a.Status == "pending" || a.Status == "rejected" || a.Status == "expired" {
			return false, ErrUnauthorized
		}
		continuation = &a
		if e.Proposal != nil {
			return false, ErrUnauthorized
		}
	}
	if err = s.verifyRunner(ctx, snapshot, continuation, identity); err != nil {
		return false, err
	}
	if e.ID == "" || len(e.ID) > 160 || strings.HasPrefix(e.ID, "system:") {
		return false, invalid("event ID must be stable, nonempty, and at most 160 characters")
	}
	if e.RunID != "" && e.RunID != id {
		return false, ErrUnauthorized
	}
	if e.CorrelationID != snapshot.CorrelationID {
		return false, ErrUnauthorized
	}
	if e.CostMicros < 0 || e.CostMicros > 1_000_000_000_000 || e.InputTokens < 0 || e.InputTokens > 1_000_000_000 || e.OutputTokens < 0 || e.OutputTokens > 1_000_000_000 || e.DurationMS < 0 {
		return false, invalid("usage and duration must be nonnegative and within reporting limits")
	}
	if len(e.Message) > 16000 || len(e.Error) > 8000 || len(e.Type) > 100 || e.Type == "" || len(e.Action) > 100 || len(e.Resource) > 300 {
		return false, invalid("event fields exceed reporting limits")
	}
	if e.Status != "" && e.Status != "queued" && e.Status != "running" && e.Status != "waiting_approval" && !Terminal(e.Status) {
		return false, invalid("invalid execution status")
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = now()
	}
	if e.Timestamp.After(now().Add(5 * time.Minute)) {
		return false, invalid("event timestamp is in the future")
	}
	// Freeform metadata is permitted but redacted before persistence.
	if len(e.Metadata) > 16000 {
		return false, invalid("metadata exceeds reporting limits")
	}
	e.Metadata = redactJSON(e.Metadata)
	e.Message = Redact(e.Message)
	e.Error = Redact(e.Error)
	e.Type = Redact(e.Type)
	e.Action = Redact(e.Action)
	e.Resource = Redact(e.Resource)
	e.RunID = id
	e.AgentID = snapshot.Agent.ID
	e.AgentVersion = snapshot.Agent.Version
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", id)
	if err != nil {
		return false, err
	}
	if continuation == nil {
		if err = bindInitial(&r, identity); err != nil {
			return false, err
		}
	} else {
		a, lookupErr := readJSON[Approval](ctx, tx, "SELECT document FROM approvals WHERE id=$1 FOR UPDATE", e.ApprovalID)
		if lookupErr != nil {
			return false, lookupErr
		}
		if ignoredRun(a, identity.RunID) || (a.GitHubRunID != 0 && a.GitHubRunID != identity.RunID) {
			return false, ErrUnauthorized
		}
	}
	inserted, err := insertEvent(ctx, tx, e)
	if err != nil {
		return false, err
	}
	if !inserted {
		return false, tx.Commit(ctx)
	}
	if r.CostMicros > math.MaxInt64-e.CostMicros || r.InputTokens > math.MaxInt64-e.InputTokens || r.OutputTokens > math.MaxInt64-e.OutputTokens {
		return false, fmt.Errorf("reported usage overflow")
	}
	r.CostMicros += e.CostMicros
	r.InputTokens += e.InputTokens
	r.OutputTokens += e.OutputTokens
	r.OverBudget = r.CostMicros > r.BudgetMicros
	if e.Proposal != nil {
		if err = validateProposal(*e.Proposal, r.Agent); err != nil {
			return false, invalid(err.Error())
		}
		if Terminal(r.Status) {
			return false, fmt.Errorf("run is already closed: %w", ErrConflict)
		}
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM approvals WHERE run_id=$1)", id).Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			return false, fmt.Errorf("run already has an immutable proposal: %w", ErrConflict)
		}
		payload := mustJSON(e.Proposal)
		hash := sha256.Sum256(payload)
		ttl := s.ApprovalTTL
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		a := Approval{ID: ID(), RunID: id, Proposal: *e.Proposal, PayloadHash: hex.EncodeToString(hash[:]), Status: "pending", CreatedAt: now(), ExpiresAt: now().Add(ttl)}
		if _, err = tx.Exec(ctx, "INSERT INTO approvals(id,run_id,status,document) VALUES($1,$2,$3,$4)", a.ID, id, a.Status, mustJSON(a)); err != nil {
			return false, err
		}
		r.Status = "waiting_approval"
		if err = audit(ctx, tx, r, "approval.requested", "runner", "Proposed issue creation; execution awaits administrator approval"); err != nil {
			return false, err
		}
	} else if continuation == nil {
		r.Status = Transition(r.Status, e.Status)
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func redactJSON(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	var walk func(any) any
	walk = func(x any) any {
		switch p := x.(type) {
		case string:
			return Redact(p)
		case map[string]any:
			for k, value := range p {
				key := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
				if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "authorization") || strings.Contains(key, "apikey") || strings.Contains(key, "privatekey") || strings.Contains(key, "accesskey") || strings.Contains(key, "credential") || strings.Contains(key, "cookie") {
					p[k] = "[REDACTED]"
				} else {
					p[k] = walk(value)
				}
			}
		case []any:
			for i := range p {
				p[i] = walk(p[i])
			}
		}
		return x
	}
	return mustJSON(walk(v))
}

// All critical operations lock the run before its approval to avoid lock inversion.
func (s *Service) lockApproval(ctx context.Context, tx pgx.Tx, id string) (Run, Approval, error) {
	a, err := readJSON[Approval](ctx, tx, "SELECT document FROM approvals WHERE id=$1", id)
	if err != nil {
		return Run{}, a, err
	}
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", a.RunID)
	if err != nil {
		return r, a, err
	}
	a, err = readJSON[Approval](ctx, tx, "SELECT document FROM approvals WHERE id=$1 FOR UPDATE", id)
	return r, a, err
}
func (s *Service) Decide(ctx context.Context, id, actor, decision, reason string) error {
	if !s.allowed(actor) {
		return ErrUnauthorized
	}
	if decision != "approve" && decision != "reject" {
		return fmt.Errorf("decision must be approve or reject")
	}
	if len(reason) > 2000 {
		return fmt.Errorf("decision reason exceeds 2000 characters")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, a, err := s.lockApproval(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.Status != "pending" {
		return fmt.Errorf("approval is already %s: %w", a.Status, ErrConflict)
	}
	if !a.ExpiresAt.After(now()) || r.Status != "waiting_approval" {
		return fmt.Errorf("approval is expired or its run is closed: %w", ErrConflict)
	}
	if decision == "approve" && r.OverBudget {
		return fmt.Errorf("run exceeds its spending budget: %w", ErrConflict)
	}
	if decision == "approve" {
		current, e := readJSON[Agent](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR SHARE", r.Agent.ID)
		if e != nil || !current.Enabled || current.Version != r.Agent.Version {
			return fmt.Errorf("agent configuration changed; launch a new run: %w", ErrConflict)
		}
	}
	decided := now()
	a.DecidedAt = &decided
	a.DecidedBy = actor
	a.DecisionReason = Redact(reason)
	if decision == "approve" {
		a.Status = "approved"
		if _, err = tx.Exec(ctx, "INSERT INTO dispatch_jobs(id,run_id,approval_id) VALUES($1,$2,$3)", a.ID+":continuation", r.ID, a.ID); err != nil {
			return err
		}
	} else {
		a.Status = "rejected"
		r.Status = "cancelled"
	}
	if err = saveApproval(ctx, tx, a); err != nil {
		return err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	if err = audit(ctx, tx, r, "approval."+a.Status, actor, a.DecisionReason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) RunnerApproval(ctx context.Context, id string, identity Identity) (Approval, error) {
	a, err := s.Approval(ctx, id)
	if err != nil {
		return a, err
	}
	r, err := s.Run(ctx, a.RunID)
	if err != nil {
		return a, err
	}
	if err = s.verifyRunner(ctx, r, &a, identity); err != nil {
		return a, err
	}
	if a.Status == "pending" || a.Status == "rejected" || a.Status == "expired" {
		return a, ErrUnauthorized
	}
	return a, nil
}

// Execute never blindly retries GitHub's non-idempotent issue-creation endpoint.
// A durable attempted_at is committed BEFORE the request; uncertain outcomes are reconciled by marker.
func (s *Service) Execute(ctx context.Context, id, runID, correlation, hash string, identity Identity) (Approval, error) {
	a, err := s.RunnerApproval(ctx, id, identity)
	if err != nil {
		return a, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	r, a, err := s.lockApproval(ctx, tx, id)
	if err != nil {
		return a, err
	}
	if runID != r.ID || correlation != r.CorrelationID || hash != a.PayloadHash {
		return a, ErrUnauthorized
	}
	if ignoredRun(a, identity.RunID) || (a.GitHubRunID != 0 && a.GitHubRunID != identity.RunID) {
		return a, ErrUnauthorized
	}
	if a.Status == "executed" {
		return a, nil
	}
	if a.AttemptedAt != nil {
		return a, fmt.Errorf("execution is already attempted; recovery is pending: %w", ErrConflict)
	}
	if a.Status != "approved" || !a.ExpiresAt.After(now()) || r.Status != "waiting_approval" || r.OverBudget {
		return a, fmt.Errorf("approval is stale, closed, or over budget: %w", ErrConflict)
	}
	current, err := readJSON[Agent](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR SHARE", r.Agent.ID)
	if err != nil || !current.Enabled || current.Version != r.Agent.Version {
		return a, fmt.Errorf("agent configuration changed; launch a new run: %w", ErrConflict)
	}
	hashNow := sha256.Sum256(mustJSON(a.Proposal))
	if hex.EncodeToString(hashNow[:]) != a.PayloadHash {
		return a, ErrUnauthorized
	}
	at := now()
	a.AttemptedAt = &at
	a.Status = "executing"
	a.GitHubRunID = identity.RunID
	a.GitHubURL = fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", r.Agent.Owner, r.Agent.Repository, identity.RunID)
	if err = saveApproval(ctx, tx, a); err != nil {
		return a, err
	}
	if err = audit(ctx, tx, r, "action.started", "continuation", "Durable authorization claimed for issue creation"); err != nil {
		return a, err
	}
	if err = tx.Commit(ctx); err != nil {
		return a, err
	}
	url, createErr := s.Backend.CreateIssue(ctx, r.Agent, a.Proposal, a.ID)
	// Do not lose the final result just because the requesting runner disconnected.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if createErr != nil {
		_ = s.finishExecution(finishCtx, id, "", false)
		return a, fmt.Errorf("issue creation outcome is uncertain; Agbredo will reconcile without repeating creation")
	}
	if err = s.finishExecution(finishCtx, id, url, true); err != nil {
		return a, err
	}
	return s.Approval(finishCtx, id)
}
func (s *Service) finishExecution(ctx context.Context, id, url string, success bool) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, a, err := s.lockApproval(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.Status == "executed" {
		return nil
	}
	if success {
		a.Status = "executed"
		a.ResultURL = url
		r.Status = "completed"
	} else {
		a.Status = "execution_unknown"
	}
	if err = saveApproval(ctx, tx, a); err != nil {
		return err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	message := "Issue creation outcome uncertain; creation will not be retried"
	if success {
		message = "Approved issue created: " + url
	}
	if err = audit(ctx, tx, r, "action."+a.Status, "control-plane", message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) Retry(ctx context.Context, id, actor string) error {
	if !s.allowed(actor) {
		return ErrUnauthorized
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, a, err := s.lockApproval(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.Status == "executing" || a.Status == "execution_unknown" {
		// Reconciliation can only read GitHub; it cannot authorize another create attempt.
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		url, err := s.Backend.FindIssue(ctx, r.Agent, a.ID)
		if err != nil {
			return err
		}
		if url == "" {
			return fmt.Errorf("no issue marker found; execution remains unresolved and will not be repeated: %w", ErrConflict)
		}
		return s.finishExecution(ctx, id, url, true)
	}
	if a.Status != "approved" || a.AttemptedAt != nil || !a.ExpiresAt.After(now()) || r.OverBudget || r.Status != "waiting_approval" {
		return fmt.Errorf("continuation cannot be retried in its current state: %w", ErrConflict)
	}
	// If GitHub already assigned a run, retry its dispatch only after that workflow finished.
	if a.GitHubRunID != 0 {
		gh, e := s.Backend.GetRun(ctx, r.Agent, a.GitHubRunID)
		if e != nil {
			return e
		}
		if gh.Status != "completed" {
			return fmt.Errorf("continuation is still active: %w", ErrConflict)
		}
		a.GitHubRunID = 0
		a.IgnoredRunIDs = append(a.IgnoredRunIDs, gh.ID)
		a.GitHubURL = ""
	}
	if err = saveApproval(ctx, tx, a); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE dispatch_jobs SET finished=false,available_at=now(),lease_until=NULL WHERE id=$1 AND (lease_until IS NULL OR lease_until<now())", id+":continuation")
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("dispatch worker is still active: %w", ErrConflict)
	}
	if err = audit(ctx, tx, r, "continuation.retry", actor, "Continuation dispatch retry requested"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }
