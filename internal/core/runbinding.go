package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A runner proves who it is with its GitHub OIDC identity. The code here binds
// that identity to one run attempt of a deployment, and creates the run on
// first use. Telemetry tokens are minted through it.

// ClaudeUsage keeps absent measurements distinct from a measured zero. Input
// excludes cache reads and cache writes, which Claude Code reports separately.
type ClaudeUsage struct {
	InputTokens         *int64 `json:"input_tokens,omitempty"`
	OutputTokens        *int64 `json:"output_tokens,omitempty"`
	CacheReadTokens     *int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64 `json:"cache_creation_tokens,omitempty"`
}

// The "observe:" prefix is kept so IDs of runs stored earlier stay stable.
func attemptRunID(deployment string, run int64, attempt int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("observe:%s:%d:%d", deployment, run, attempt)))
	return hex.EncodeToString(h[:])
}

// sameRepository compares GitHub repository names ("owner/repository"). GitHub
// ignores case in owner and repository names, and so does the unique index on
// deployments (migrations/006_unique_deployment.sql).
func sameRepository(a, b string) bool { return strings.EqualFold(a, b) }

func verifyIdentity(a Deployment, i Identity, gh WorkflowRun) error {
	if !sameRepository(i.Repository, a.Owner+"/"+a.Repository) || i.Workflow != a.Workflow || i.RunID <= 0 || i.RunAttempt <= 0 || gh.ID != i.RunID || gh.Attempt != i.RunAttempt || gh.Workflow != a.Workflow {
		return ErrUnauthorized
	}
	return nil
}

// lockRun finds the run for a verified GitHub run attempt, or creates it, and
// locks it for the rest of the transaction. The first valid request creates it.
func (s *Service) lockRun(ctx context.Context, tx pgx.Tx, deploymentID string, i Identity) (Run, WorkflowRun, error) {
	var gh WorkflowRun
	id := attemptRunID(deploymentID, i.RunID, i.RunAttempt)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1", id)
	if err != nil && err != ErrNotFound {
		return r, gh, err
	}
	a := r.Deployment
	if err == ErrNotFound {
		a, err = readJSON[Deployment](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR SHARE", deploymentID)
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
	if err = verifyIdentity(a, i, gh); err != nil {
		return r, gh, err
	}
	if r.ID == "" {
		r = Run{ID: id, CorrelationID: id, Deployment: a, RunAttempt: i.RunAttempt, Status: "running", ThresholdMicros: a.ThresholdMicros, CreatedAt: now(), UpdatedAt: now()}
		r.GitHubRunID, r.Ref, r.Pipeline = i.RunID, i.Ref, PipelineOf(i)
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
	// A racing first request must also match the snapshot which won insertion.
	if err = verifyIdentity(r.Deployment, i, gh); err != nil {
		return r, gh, err
	}
	return r, gh, nil
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
func statusFromGitHub(current string, gh WorkflowRun) string {
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
