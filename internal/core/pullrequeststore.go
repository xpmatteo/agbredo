package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PullRequestWebhook records a pull_request delivery. GitHub signed the
// payload, so its facts are used as they are; nothing is asked of GitHub.
//
//   - opened: a pull request of a repository with a live deployment (any
//     workflow, disabled or not) is recorded if its author is a bot or its
//     body has a trailer, and linked to a run if it can be (LinkPullRequest).
//     Its facts, trailer and findings are those of the opening.
//   - Other actions (edited, closed, reopened, ...), and pull requests
//     Agbredo does not record: ignored. An edit never changes what was
//     recorded: a trailer added later does not count.
//
// The delivery is indexed as handled, ignored or failed. A handled delivery
// is not applied again.
func (s *Service) PullRequestWebhook(ctx context.Context, d Delivery, f PullRequestFacts) error {
	return s.indexDelivery(ctx, d, func() (bool, error) { return s.pullRequestWebhook(ctx, d, f) })
}

func (s *Service) pullRequestWebhook(ctx context.Context, d Delivery, f PullRequestFacts) (bool, error) {
	owner, repository, ok := strings.Cut(f.Repository, "/")
	if !ok || owner == "" || repository == "" || f.RepositoryID <= 0 || f.Number <= 0 || f.CreatedAt.IsZero() {
		return false, invalid("invalid pull_request payload")
	}
	if d.Action != "opened" {
		return false, nil
	}
	p := newPullRequest(f, d.ID)
	if !p.recorded() {
		return false, nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	first, err := markHandled(ctx, tx, d)
	if err != nil {
		return false, err
	}
	if !first {
		return true, nil // handled before
	}
	var watched bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE NOT deleted
		AND lower(document->>'owner')=lower($1) AND lower(document->>'repository')=lower($2))`, owner, repository).Scan(&watched); err != nil {
		return false, err
	}
	if !watched {
		return false, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO pull_requests(id, owner, repository, number, link, document) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO NOTHING`, p.ID, p.Owner, p.Repository, p.Number, p.Link, mustJSON(p)); err != nil {
		return false, err
	}
	// Locked, so that two deliveries, or a delivery and Reconcile, link it
	// once. The same opening sent again with another delivery ID finds the
	// record of the first, and keeps its facts.
	current, err := readJSON[PullRequest](ctx, tx, "SELECT document FROM pull_requests WHERE id=$1 FOR UPDATE", p.ID)
	if err != nil {
		return false, err
	}
	if current.Link != LinkUnlinked {
		return false, nil // a link never changes
	}
	if err = linkAndSave(ctx, tx, current, true); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// markHandled indexes a delivery as handled, in the transaction that applies
// it. It says false when the delivery was handled before; it is then not
// applied again. A failed or ignored delivery may be handled when GitHub
// sends it again. Rows from before outcomes were kept have none, and were
// all handled. A delivery without an ID is not indexed.
func markHandled(ctx context.Context, tx pgx.Tx, d Delivery) (bool, error) {
	if d.ID == "" {
		return true, nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries(id, event, action, repository, installation_id, outcome)
		VALUES($1, NULLIF($2,''), NULLIF($3,''), NULLIF($4,''), NULLIF($5,0), 'handled')
		ON CONFLICT (id) DO UPDATE SET event=EXCLUDED.event, action=EXCLUDED.action, repository=EXCLUDED.repository,
			installation_id=EXCLUDED.installation_id, outcome='handled', error=NULL
		WHERE webhook_deliveries.outcome IN ('failed','ignored')`, d.ID, d.Event, d.Action, d.Repository, d.InstallationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// indexDelivery applies a delivery and indexes it: handled (by apply, in its
// own transaction), ignored, or failed with the error.
func (s *Service) indexDelivery(ctx context.Context, d Delivery, apply func() (bool, error)) error {
	handled, err := apply()
	outcome := DeliveryIgnored
	if err != nil {
		outcome = DeliveryFailed
	} else if handled {
		return nil // indexed in apply's transaction
	}
	// The request may have timed out; the index is still written.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := s.RecordDelivery(recordCtx, d, outcome, err); e != nil {
		return errors.Join(err, e)
	}
	return err
}

// pullRequestRuns are the runs of the pull request's repository that may
// have opened it: those whose window may hold its opening, and the one its
// trailer names. LinkPullRequest decides among them.
func pullRequestRuns(ctx context.Context, tx pgx.Tx, p PullRequest) ([]Run, error) {
	rows, err := tx.Query(ctx, `SELECT document FROM runs
		WHERE lower(document->'agent'->>'owner')=lower($1) AND lower(document->'agent'->>'repository')=lower($2)
		AND ((document->>'github_run_id'=$3 AND document->>'run_attempt'=$4)
			OR (COALESCE((document->>'github_started_at')::timestamptz, (document->>'created_at')::timestamptz) <= $5::timestamptz
				AND (status IN ('queued','running') OR (document->>'github_completed_at')::timestamptz >= $5::timestamptz - interval '60 seconds')))
		ORDER BY id`, p.Owner, p.Repository, fmt.Sprint(p.TrailerRunID), fmt.Sprint(p.TrailerAttempt), p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return collect[Run](rows)
}

// linkAndSave links a locked, unlinked pull request to a run, if it can, and
// saves it. A linked pull request adds an event to its run. With always
// false, a pull request whose outcome did not change is not written again.
func linkAndSave(ctx context.Context, tx pgx.Tx, p PullRequest, always bool) error {
	runs, err := pullRequestRuns(ctx, tx, p)
	if err != nil {
		return err
	}
	l := LinkPullRequest(p, runs)
	if !always && l.Level == LinkUnlinked && l.Reason == p.Reason && reflect.DeepEqual(l.Candidates, p.CandidateRuns) && reflect.DeepEqual(l.Findings, p.Findings) {
		return nil
	}
	p.Link, p.RunID, p.Reason, p.CandidateRuns, p.Findings = l.Level, l.RunID, l.Reason, l.Candidates, l.Findings
	var run *string
	if l.RunID != "" {
		run = &p.RunID
		p.LinkedAt = now()
	}
	// The WHERE clause keeps a link from ever changing, even if a caller forgot the lock.
	tag, err := tx.Exec(ctx, "UPDATE pull_requests SET link=$2, run_id=$3, document=$4, updated_at=now() WHERE id=$1 AND link='unlinked'",
		p.ID, p.Link, run, mustJSON(p))
	if err != nil {
		return err
	}
	if l.RunID == "" || tag.RowsAffected() != 1 {
		return nil
	}
	for _, r := range runs {
		if r.ID == l.RunID {
			_, err = insertEvent(ctx, tx, pullRequestEvent(p, r))
			return err
		}
	}
	return fmt.Errorf("linked run %s not found", l.RunID)
}

// pullRequestEvent is the event on the linked run: the run opened a pull
// request. Its ID comes from the delivery that recorded the pull request, so
// linking it later (Reconcile) does not add it twice.
func pullRequestEvent(p PullRequest, r Run) Event {
	key := p.Delivery
	if key == "" {
		key = p.ID
	}
	return Event{ID: "github:pull_request:opened:" + key, RunID: r.ID, CorrelationID: r.CorrelationID, DeploymentID: r.Deployment.ID,
		DeploymentRevision: r.Deployment.Revision, Timestamp: p.CreatedAt, Type: "github.pull_request", Action: "opened",
		Resource: fmt.Sprintf("%s/%s#%d", p.Owner, p.Repository, p.Number), Status: r.Status,
		Message: fmt.Sprintf("Pull request #%d opened by %s; link %s", p.Number, p.Author, p.Link)}
}

// LinkPullRequests tries again to link the pull requests that are unlinked
// and were opened less than relinkWindow ago: the run's webhook may come
// after the pull request's. It reads only the database.
func (s *Service) LinkPullRequests(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT id FROM pull_requests WHERE link='unlinked'
		AND (document->>'created_at')::timestamptz > $1 ORDER BY id`, now().Add(-relinkWindow))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// One pull request that fails does not stop the others.
	var failed []error
	for _, id := range ids {
		if e := s.relink(ctx, id); e != nil {
			if ctx.Err() != nil {
				return errors.Join(append(failed, e)...)
			}
			failed = append(failed, fmt.Errorf("link pull request %s: %w", id, e))
		}
	}
	return errors.Join(failed...)
}

func (s *Service) relink(ctx context.Context, id string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	p, err := readJSON[PullRequest](ctx, tx, "SELECT document FROM pull_requests WHERE id=$1 FOR UPDATE", id)
	if err != nil {
		return err
	}
	if p.Link != LinkUnlinked {
		return nil
	}
	if err = linkAndSave(ctx, tx, p, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunPullRequests are the pull requests linked to a run, oldest first.
func (s *Service) RunPullRequests(ctx context.Context, runID string) ([]PullRequest, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM pull_requests WHERE run_id=$1 ORDER BY number", runID)
	if err != nil {
		return nil, err
	}
	return collect[PullRequest](rows)
}

// UnlinkedPullRequests are the newest unlinked pull requests of a
// repository, at most limit of them. Owner and repository ignore case.
func (s *Service) UnlinkedPullRequests(ctx context.Context, owner, repository string, limit int) ([]PullRequest, error) {
	rows, err := s.DB.Query(ctx, `SELECT document FROM pull_requests WHERE link='unlinked' AND lower(owner)=lower($1) AND lower(repository)=lower($2)
		ORDER BY (document->>'created_at')::timestamptz DESC, id LIMIT $3`, owner, repository, limit)
	if err != nil {
		return nil, err
	}
	return collect[PullRequest](rows)
}

// pullRequestsMigration is the migration that added pull requests. When it
// was applied, Agbredo started recording them.
const pullRequestsMigration = "008_pull_requests.sql"

// PullRequestsSince is when Agbredo started recording pull requests: when
// its database got the pull_requests table. A run that started before then
// may have opened one that Agbredo does not know.
func (s *Service) PullRequestsSince(ctx context.Context) (time.Time, error) {
	var since time.Time
	err := s.DB.QueryRow(ctx, "SELECT applied_at FROM schema_migrations WHERE name=$1", pullRequestsMigration).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return since, err
}

// linkedPullRequests are the linked pull requests of all runs, by run ID.
func (s *Service) linkedPullRequests(ctx context.Context) (map[string][]PullRequest, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM pull_requests WHERE run_id IS NOT NULL ORDER BY number")
	if err != nil {
		return nil, err
	}
	all, err := collect[PullRequest](rows)
	if err != nil {
		return nil, err
	}
	byRun := map[string][]PullRequest{}
	for _, p := range all {
		byRun[p.RunID] = append(byRun[p.RunID], p)
	}
	return byRun, nil
}
