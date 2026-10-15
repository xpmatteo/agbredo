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
//
// A run that a webhook recorded, but that has no telemetry token yet, is
// treated like a new run: its deployment must be live and enabled. Only a run
// that already has a token may get more while its deployment is disabled, as
// before. Recording a run grants it nothing.
//
// The first token of a run records the ref, trigger event and pipeline from
// the OIDC identity, which is more exact than the webhook's facts. Later
// requests change neither.
func (s *Service) lockRun(ctx context.Context, tx pgx.Tx, deploymentID string, i Identity) (Run, WorkflowRun, error) {
	var gh WorkflowRun
	id := attemptRunID(deploymentID, i.RunID, i.RunAttempt)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1", id)
	if err != nil && err != ErrNotFound {
		return r, gh, err
	}
	granted := false
	if err == nil {
		if granted, err = hasToken(ctx, tx, id); err != nil {
			return r, gh, err
		}
	}
	a := r.Deployment
	if !granted {
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
		r.GitHubRunID, r.Ref, r.Pipeline, r.TriggerEvent = i.RunID, i.Ref, PipelineOf(i), i.EventName
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
	// Asked again under the lock: of two first requests, only the one that
	// gets the lock first records its identity.
	if granted, err = hasToken(ctx, tx, id); err != nil {
		return r, gh, err
	}
	if !granted {
		r.Ref, r.TriggerEvent, r.Pipeline = i.Ref, i.EventName, PipelineOf(i)
		if r.Actor == "" {
			r.Actor = gh.Actor
		}
		if r.TriggeringActor == "" {
			r.TriggeringActor = gh.TriggeringActor
		}
	}
	return r, gh, nil
}

// hasToken says whether a run has been given a telemetry token. Tokens are
// never deleted.
func hasToken(ctx context.Context, tx pgx.Tx, runID string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM run_tokens WHERE run_id=$1)", runID).Scan(&found)
	return found, err
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

// statusFromGitHub is the run's next status. A run that waits to start stays
// queued; one that has started does not go back to queued. A run that waits
// for a maintainer to approve it (action_required, e.g. a pull request from a
// fork) is not finished: it stays queued, so that an approval can move it on.
// A skipped or neutral run is completed, not failed.
func statusFromGitHub(current string, gh WorkflowRun) string {
	if !finished(gh) {
		switch gh.Status {
		case "completed", "action_required", "requested", "queued", "pending", "waiting":
			return Transition(current, "queued")
		default:
			return Transition(current, "running")
		}
	}
	switch gh.Conclusion {
	case "success", "skipped", "neutral":
		return Transition(current, "completed")
	case "cancelled":
		return Transition(current, "cancelled")
	default:
		return Transition(current, "failed")
	}
}

// finished says whether GitHub has finished the run attempt. A completed run
// that waits for approval (conclusion action_required) has not.
func finished(gh WorkflowRun) bool {
	return gh.Status == "completed" && !awaitsApproval(gh)
}

// awaitsApproval says whether the run attempt waits for a maintainer's approval.
func awaitsApproval(gh WorkflowRun) bool {
	return gh.Status == "action_required" || (gh.Status == "completed" && gh.Conclusion == "action_required")
}
