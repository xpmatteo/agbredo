package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound     = errors.New("record not found")
	ErrConflict     = errors.New("operation conflicts with current state")
	ErrUnauthorized = errors.New("not authorized")
	ErrInvalid      = errors.New("invalid input")
	// ErrNotInstalled says the GitHub App is not installed on a repository.
	// Runners are refused, as for any other identity that does not match.
	ErrNotInstalled = fmt.Errorf("the GitHub App is not installed on the repository: %w", ErrUnauthorized)
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

// JSON keys keep older names (agent, version, budget_micros, over_budget)
// so that stored documents still load. Do not rename them.
//
// A deployment is identified by repository + workflow file. Runs from any ref
// belong to it; each run records its own ref.
type Deployment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       string `json:"owner"`
	Repository  string `json:"repository"`
	Workflow    string `json:"workflow"`
	// Ref is legacy: deployments were once tied to one ref. Nothing sets or
	// matches it any more. It is kept so that stored deployments, and the
	// copies kept in older runs, still load unchanged, and so that a run
	// without its own Ref can show the ref its deployment had.
	Ref             string `json:"ref,omitempty"`
	InstallationID  int64  `json:"installation_id"`
	ThresholdMicros int64  `json:"budget_micros"`
	Enabled         bool   `json:"enabled"`
	Revision        int64  `json:"version"`
}

type Run struct {
	RunAttempt             int64        `json:"run_attempt,omitempty"`
	ClaudeUsage            *ClaudeUsage `json:"claude_usage,omitempty"`
	EstimatedCostAvailable bool         `json:"estimated_cost_available,omitempty"`
	RateLimitFailures      int64        `json:"rate_limit_failures,omitempty"`
	APIErrors              int64        `json:"api_errors,omitempty"`
	ID                     string       `json:"id"`
	CorrelationID          string       `json:"correlation_id"`
	Deployment             Deployment   `json:"agent"`
	Status                 string       `json:"status"`
	ThresholdMicros        int64        `json:"budget_micros"`
	CostMicros             int64        `json:"cost_micros"`
	InputTokens            int64        `json:"input_tokens"`
	OutputTokens           int64        `json:"output_tokens"`
	OverThreshold          bool         `json:"over_budget"`
	GitHubRunID            int64        `json:"github_run_id"`
	GitHubURL              string       `json:"github_url"`
	// The git ref the run ran from, e.g. refs/heads/main, from the job's OIDC
	// identity when the run is first created. Older runs have none.
	Ref string `json:"ref,omitempty"`
	// The pipeline the run used and its version, from the job's OIDC identity
	// when the run is first created. Missing for a caller workflow that calls no
	// reusable workflow, and for older runs.
	Pipeline *Pipeline `json:"pipeline,omitempty"`
	// GitHub logins, from GitHub when the run is first seen. Older runs have none.
	Actor           string    `json:"actor,omitempty"`
	TriggeringActor string    `json:"triggering_actor,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	// Per agent run, from telemetry sent straight to Agbredo. The totals above include these.
	// The JSON key "sources" is kept on purpose: stored rows and clients use it.
	AgentRuns map[string]*AgentRunUsage `json:"sources,omitempty"`
}

// Event is one entry in a run's history: a GitHub status change. A stable
// event ID makes retries safe. Rows stored by the former report route also
// hold "claude", "duration_ms", "input_tokens", "output_tokens" and
// "cost_micros" keys; loading ignores them.
type Event struct {
	ID                 string          `json:"id"`
	RunID              string          `json:"run_id"`
	CorrelationID      string          `json:"correlation_id"`
	DeploymentID       string          `json:"agent_id"`
	DeploymentRevision int64           `json:"agent_version"`
	Timestamp          time.Time       `json:"timestamp"`
	Type               string          `json:"type"`
	Action             string          `json:"action"`
	Resource           string          `json:"resource"`
	Status             string          `json:"status"`
	Message            string          `json:"message"`
	Error              string          `json:"error"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
}

type Identity struct {
	EventName  string
	Repository string
	Workflow   string
	Ref        string
	RunID      int64
	RunAttempt int64
	// The OIDC job_workflow_ref and job_workflow_sha claims: the workflow the
	// job ran, which is a pipeline when the caller workflow calls a reusable
	// workflow. Recorded on the run (see PipelineOf); never used for identity.
	JobWorkflowRef string
	JobWorkflowSHA string
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
	// GitHub logins: who started the run, and who started this attempt.
	Actor           string
	TriggeringActor string
}

type Backend interface {
	GetRun(context.Context, Deployment, int64) (WorkflowRun, error)
	GetRunAttempt(context.Context, Deployment, int64, int64) (WorkflowRun, error)
	// Installation is the ID of the GitHub App installation on a repository,
	// asked as the App itself. ErrNotInstalled when the App is not installed.
	Installation(ctx context.Context, owner, repository string) (int64, error)
}

type Fleet struct {
	Deployments []Deployment
	Runs        []Run
}
