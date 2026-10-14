package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("record not found")
	ErrConflict     = errors.New("operation conflicts with current state")
	ErrUnauthorized = errors.New("not authorized")
	ErrInvalid      = errors.New("invalid input")
)

type invalidInput struct{ message string }

func (e invalidInput) Error() string { return e.message }
func (e invalidInput) Unwrap() error { return ErrInvalid }
func invalid(message string) error   { return invalidInput{message: message} }

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Agent struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Owner          string `json:"owner"`
	Repository     string `json:"repository"`
	Workflow       string `json:"workflow"`
	Ref            string `json:"ref"`
	InstallationID int64  `json:"installation_id"`
	BudgetMicros   int64  `json:"budget_micros"`
	Enabled        bool   `json:"enabled"`
	Version        int64  `json:"version"`
}

type Run struct {
	RunAttempt             int64        `json:"run_attempt,omitempty"`
	ClaudeUsage            *ClaudeUsage `json:"claude_usage,omitempty"`
	EstimatedCostAvailable bool         `json:"estimated_cost_available,omitempty"`
	RateLimitFailures      int64        `json:"rate_limit_failures,omitempty"`
	APIErrors              int64        `json:"api_errors,omitempty"`
	ID                     string       `json:"id"`
	CorrelationID          string       `json:"correlation_id"`
	Agent                  Agent        `json:"agent"`
	Status                 string       `json:"status"`
	BudgetMicros           int64        `json:"budget_micros"`
	CostMicros             int64        `json:"cost_micros"`
	InputTokens            int64        `json:"input_tokens"`
	OutputTokens           int64        `json:"output_tokens"`
	OverBudget             bool         `json:"over_budget"`
	GitHubRunID            int64        `json:"github_run_id"`
	GitHubURL              string       `json:"github_url"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
	// Per Claude Code step, from telemetry sent straight to Agbredo. The totals above include these.
	Sources map[string]*SourceUsage `json:"sources,omitempty"`
}

// Costs are deltas, expressed as integer microdollars. A stable event ID makes retries safe.
type Event struct {
	Claude        *Report         `json:"claude,omitempty"`
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	CorrelationID string          `json:"correlation_id"`
	AgentID       string          `json:"agent_id"`
	AgentVersion  int64           `json:"agent_version"`
	Timestamp     time.Time       `json:"timestamp"`
	Type          string          `json:"type"`
	Action        string          `json:"action"`
	Resource      string          `json:"resource"`
	Status        string          `json:"status"`
	DurationMS    int64           `json:"duration_ms"`
	InputTokens   int64           `json:"input_tokens"`
	OutputTokens  int64           `json:"output_tokens"`
	CostMicros    int64           `json:"cost_micros"`
	Message       string          `json:"message"`
	Error         string          `json:"error"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

type Identity struct {
	EventName  string
	Repository string
	Workflow   string
	Ref        string
	RunID      int64
	RunAttempt int64
}

type WorkflowRun struct {
	Attempt    int64
	ID         int64
	URL        string
	Title      string
	Status     string
	Conclusion string
	Workflow   string
	Ref        string
}

type Backend interface {
	GetRun(context.Context, Agent, int64) (WorkflowRun, error)
	GetRunAttempt(context.Context, Agent, int64, int64) (WorkflowRun, error)
}

type Fleet struct {
	Agents []Agent
	Runs   []Run
}
