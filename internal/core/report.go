package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// ClaudeUsage keeps absent measurements distinct from a measured zero. Input
// excludes cache reads and cache writes, which Claude Code reports separately.
type ClaudeUsage struct {
	InputTokens         *int64 `json:"input_tokens,omitempty"`
	OutputTokens        *int64 `json:"output_tokens,omitempty"`
	CacheReadTokens     *int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64 `json:"cache_creation_tokens,omitempty"`
}

// Report is an allowlist, not a container for Claude output, OTLP payloads,
// environment variables or credentials. Costs are estimates of API consumption;
// subscription billing and remaining subscription quota are not measured here.
type Report struct {
	GitHubRunID         int64        `json:"github_run_id"`
	GitHubRunAttempt    int64        `json:"github_run_attempt"`
	Source              string       `json:"source"`
	Phase               string       `json:"phase"` // started or finished; one pair per source and workflow attempt
	Version             string       `json:"version"`
	ExitCode            *int         `json:"exit_code,omitempty"`
	ActionOutcome       string       `json:"action_outcome,omitempty"` // SDK action outcome; never fabricate a CLI exit code
	DurationMS          int64        `json:"duration_ms"`
	TelemetrySupported  bool         `json:"telemetry_supported"`
	TelemetryReceived   bool         `json:"telemetry_received"`
	Usage               *ClaudeUsage `json:"usage,omitempty"`
	EstimatedCostMicros *int64       `json:"estimated_cost_micros,omitempty"`
	APIErrors           int64        `json:"api_errors"`
	APIFailed           bool         `json:"api_failed"`
	RateLimitFailures   int64        `json:"rate_limit_failures"`
}

var claudeVersion = regexp.MustCompile(`^(unknown|[0-9]+\.[0-9]+\.[0-9]+)$`)

func (o Report) validate() error {
	if !slug.MatchString(o.Source) || len(o.Source) > 80 || !claudeVersion.MatchString(o.Version) || len(o.Version) > 40 {
		return invalid("provide a stable reporting source and Claude Code version")
	}
	if o.DurationMS < 0 || o.APIErrors < 0 || o.APIErrors > 1_000_000 || o.RateLimitFailures < 0 || o.RateLimitFailures > o.APIErrors {
		return invalid("invalid duration or API failure counts")
	}
	if o.APIFailed && o.APIErrors == 0 {
		return invalid("API failure requires a reported error")
	}
	if !o.TelemetryReceived && (o.Usage != nil || o.EstimatedCostMicros != nil || o.APIErrors != 0) {
		return invalid("measurements require received Claude telemetry")
	}
	if o.Phase != "started" && o.Phase != "finished" {
		return invalid("report phase must be started or finished")
	}
	if o.ActionOutcome != "" && o.ActionOutcome != "success" && o.ActionOutcome != "failure" && o.ActionOutcome != "cancelled" && o.ActionOutcome != "skipped" {
		return invalid("invalid GitHub action outcome")
	}
	if o.ExitCode != nil && o.ActionOutcome != "" {
		return invalid("report either a process exit code or an action outcome")
	}
	if (o.Phase == "started" && (o.ExitCode != nil || o.ActionOutcome != "" || o.Usage != nil || o.EstimatedCostMicros != nil || o.APIErrors != 0 || o.DurationMS != 0)) || (o.Phase == "finished" && o.ExitCode == nil && o.ActionOutcome == "") {
		return invalid("only a finished report may report execution results")
	}
	if o.ExitCode != nil && (*o.ExitCode < 0 || *o.ExitCode > 255) {
		return invalid("exit code must be between 0 and 255")
	}
	if o.EstimatedCostMicros != nil && (*o.EstimatedCostMicros < 0 || *o.EstimatedCostMicros > 1_000_000_000_000) {
		return invalid("estimated consumption exceeds reporting limits")
	}
	if o.Usage != nil {
		for _, n := range []*int64{o.Usage.InputTokens, o.Usage.OutputTokens, o.Usage.CacheReadTokens, o.Usage.CacheCreationTokens} {
			if n != nil && (*n < 0 || *n > 1_000_000_000) {
				return invalid("token count exceeds reporting limits")
			}
		}
	}
	return nil
}

// The "observe:" prefix is kept so IDs of runs stored earlier stay stable.
func reportRunID(agent string, run int64, attempt int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("observe:%s:%d:%d", agent, run, attempt)))
	return hex.EncodeToString(h[:])
}

func verifyReport(a Agent, i Identity, gh WorkflowRun) error {
	if i.Repository != a.Owner+"/"+a.Repository || i.Workflow != a.Workflow || !refMatches(a.Ref, i.Ref) || i.RunID <= 0 || i.RunAttempt <= 0 || gh.ID != i.RunID || gh.Attempt != i.RunAttempt || gh.Workflow != a.Workflow || !refMatches(a.Ref, gh.Ref) {
		return ErrUnauthorized
	}
	return nil
}

// lockRun finds the run for a verified GitHub run attempt, or creates it, and
// locks it for the rest of the transaction. The first valid request creates it.
func (s *Service) lockRun(ctx context.Context, tx pgx.Tx, agentID string, i Identity) (Run, WorkflowRun, error) {
	var gh WorkflowRun
	id := reportRunID(agentID, i.RunID, i.RunAttempt)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1", id)
	if err != nil && err != ErrNotFound {
		return r, gh, err
	}
	a := r.Agent
	if err == ErrNotFound {
		a, err = readJSON[Agent](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR SHARE", agentID)
		if err != nil {
			return r, gh, err
		}
		if !a.Enabled {
			return r, gh, ErrUnauthorized
		}
	}
	gh, err = s.Backend.GetRunAttempt(ctx, a, i.RunID, i.RunAttempt)
	if err != nil {
		return r, gh, err
	}
	if err = verifyReport(a, i, gh); err != nil {
		return r, gh, err
	}
	if r.ID == "" {
		r = Run{ID: id, CorrelationID: id, Agent: a, RunAttempt: i.RunAttempt, Status: "running", BudgetMicros: a.BudgetMicros, CreatedAt: now(), UpdatedAt: now()}
		r.GitHubRunID = i.RunID
		r.Actor, r.TriggeringActor = gh.Actor, gh.TriggeringActor
		r.GitHubURL = fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d", a.Owner, a.Repository, i.RunID)
		if _, err = tx.Exec(ctx, "INSERT INTO runs(id,agent_id,correlation_id,status,document) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING", id, a.ID, id, r.Status, mustJSON(r)); err != nil {
			return r, gh, err
		}
	}
	r, err = readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", id)
	if err != nil {
		return r, gh, err
	}
	// A racing first report must also match the snapshot which won insertion.
	if err = verifyReport(r.Agent, i, gh); err != nil {
		return r, gh, err
	}
	return r, gh, nil
}

// Report binds independently triggered workflows using their signed GitHub
// identity. It never dispatches a workflow or performs an action.
func (s *Service) Report(ctx context.Context, agentID string, o Report, i Identity) (Run, bool, error) {
	if o.GitHubRunID != i.RunID || o.GitHubRunAttempt != i.RunAttempt || i.RunID <= 0 || i.RunAttempt <= 0 {
		return Run{}, false, ErrUnauthorized
	}
	if err := o.validate(); err != nil {
		return Run{}, false, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback(ctx)
	r, gh, err := s.lockRun(ctx, tx, agentID, i)
	if err != nil {
		return r, false, err
	}
	id := r.ID
	status := "running"
	if o.Phase == "finished" {
		status = "completed"
		if (o.ExitCode != nil && *o.ExitCode != 0) || o.ActionOutcome == "failure" || o.APIFailed {
			status = "failed"
		}
		if (o.ExitCode != nil && (*o.ExitCode == 130 || *o.ExitCode == 143)) || o.ActionOutcome == "cancelled" {
			status = "cancelled"
		}
		if o.ActionOutcome == "skipped" {
			status = "skipped"
		}
	}
	e := Event{ID: fmt.Sprintf("github:%d:%d:%s:%s", i.RunID, i.RunAttempt, o.Source, o.Phase), RunID: id, CorrelationID: id, AgentID: r.Agent.ID, AgentVersion: r.Agent.Version, Timestamp: now(), Type: "claude.execution", Status: status, DurationMS: o.DurationMS, Claude: &o, Message: "Claude Code execution report; consumption is an API-equivalent estimate, subscription charges are unknown"}
	if o.EstimatedCostMicros != nil {
		e.CostMicros = *o.EstimatedCostMicros
	}
	if o.Usage != nil {
		e.InputTokens = value(o.Usage.InputTokens)
		e.OutputTokens = value(o.Usage.OutputTokens)
	}
	inserted, err := insertEvent(ctx, tx, e)
	if err != nil {
		return r, false, err
	}
	if inserted {
		pairs := [][2]int64{{r.CostMicros, e.CostMicros}, {r.InputTokens, e.InputTokens}, {r.OutputTokens, e.OutputTokens}, {r.APIErrors, o.APIErrors}, {r.RateLimitFailures, o.RateLimitFailures}}
		if r.ClaudeUsage != nil && o.Usage != nil {
			pairs = append(pairs, [2]int64{value(r.ClaudeUsage.CacheReadTokens), value(o.Usage.CacheReadTokens)}, [2]int64{value(r.ClaudeUsage.CacheCreationTokens), value(o.Usage.CacheCreationTokens)})
		}
		for _, p := range pairs {
			if p[0] > math.MaxInt64-p[1] {
				return r, false, invalid("reported consumption overflow")
			}
		}
		r.CostMicros += e.CostMicros
		r.InputTokens += e.InputTokens
		r.OutputTokens += e.OutputTokens
		r.EstimatedCostAvailable = r.EstimatedCostAvailable || o.EstimatedCostMicros != nil
		r.APIErrors += o.APIErrors
		r.RateLimitFailures += o.RateLimitFailures
		if o.Usage != nil {
			if r.ClaudeUsage == nil {
				r.ClaudeUsage = &ClaudeUsage{}
			}
			addMeasured(&r.ClaudeUsage.InputTokens, o.Usage.InputTokens)
			addMeasured(&r.ClaudeUsage.OutputTokens, o.Usage.OutputTokens)
			addMeasured(&r.ClaudeUsage.CacheReadTokens, o.Usage.CacheReadTokens)
			addMeasured(&r.ClaudeUsage.CacheCreationTokens, o.Usage.CacheCreationTokens)
		}
		r.OverBudget = r.CostMicros > r.BudgetMicros // advisory; cannot interrupt a subscription agent
	}
	// The workflow can contain other steps; its lifecycle remains separate from
	// each Claude process report. A successful process does not finish the workflow.
	r.Status = reportedStatus(r.Status, gh)
	if err = saveRun(ctx, tx, r); err != nil {
		return r, false, err
	}
	return r, inserted, tx.Commit(ctx)
}

func value(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
func addMeasured(total **int64, n *int64) {
	if n == nil {
		return
	}
	if *total == nil {
		*total = new(int64)
	}
	**total += *n
}
func reportedStatus(current string, gh WorkflowRun) string {
	if gh.Status != "completed" {
		return Transition(current, "running")
	}
	next := "failed"
	if gh.Conclusion == "success" {
		next = "completed"
	}
	if gh.Conclusion == "cancelled" {
		next = "cancelled"
	}
	return Transition(current, next)
}
