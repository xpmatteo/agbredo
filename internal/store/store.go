// Package store keeps every workflow_run webhook in the events table, and
// the PRs and Issues that Runs opened in the opened table.
package store

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Pipeline is one entry of workflow_run.referenced_workflows.
type Pipeline struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Event is one workflow_run webhook. A Run is (RunID, RunAttempt).
type Event struct {
	DeliveryID   string
	ReceivedAt   time.Time
	Repository   string
	RunID        int64
	RunAttempt   int
	Action       string
	Status       string
	Conclusion   string
	WorkflowName string
	HTMLURL      string
	HeadBranch   string
	EventName    string
	Actor        string
	RunStartedAt time.Time
	UpdatedAt    time.Time
	Pipelines    []Pipeline
	Payload      []byte
}

// State is the conclusion once the run is completed, else the status.
func (e Event) State() string {
	if e.Status == "completed" && e.Conclusion != "" {
		return e.Conclusion
	}
	return e.Status
}

// Opened is a PR or Issue opened by a Run.
type Opened struct {
	DeliveryID string
	Repository string
	Kind       string // pull_request | issue
	Number     int
	Title      string
	HTMLURL    string
	OpenedAt   time.Time
	RunID      int64
	RunAttempt int
}

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and applies the schema.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InsertEvent stores e. A delivery that is already stored is ignored.
func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO events (delivery_id, repository, run_id, run_attempt, action, status, conclusion,
			workflow_name, html_url, head_branch, event, actor, run_started_at, updated_at, pipelines, payload)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (delivery_id) DO NOTHING`,
		e.DeliveryID, e.Repository, e.RunID, e.RunAttempt, e.Action, e.Status, e.Conclusion,
		e.WorkflowName, e.HTMLURL, e.HeadBranch, e.EventName, e.Actor, e.RunStartedAt, e.UpdatedAt, e.Pipelines, e.Payload)
	return err
}

const columns = `delivery_id, received_at, repository, run_id, run_attempt, action, status, coalesce(conclusion, ''),
	workflow_name, html_url, coalesce(head_branch, ''), coalesce(event, ''), coalesce(actor, ''), run_started_at, updated_at, pipelines`

// LatestRuns returns the latest event of each Run, newest Run first.
func (s *Store) LatestRuns(ctx context.Context) ([]Event, error) {
	return s.query(ctx, `
		SELECT * FROM (
			SELECT DISTINCT ON (run_id, run_attempt) `+columns+` FROM events
			ORDER BY run_id, run_attempt, updated_at DESC, received_at DESC
		) latest
		ORDER BY run_started_at DESC, run_id DESC, run_attempt DESC
		LIMIT 100`)
}

// RunEvents returns all events of a Run, oldest first.
func (s *Store) RunEvents(ctx context.Context, runID int64, attempt int) ([]Event, error) {
	return s.query(ctx, `SELECT `+columns+` FROM events WHERE run_id = $1 AND run_attempt = $2
		ORDER BY updated_at, received_at`, runID, attempt)
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]Event, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.DeliveryID, &e.ReceivedAt, &e.Repository, &e.RunID, &e.RunAttempt, &e.Action, &e.Status,
			&e.Conclusion, &e.WorkflowName, &e.HTMLURL, &e.HeadBranch, &e.EventName, &e.Actor,
			&e.RunStartedAt, &e.UpdatedAt, &e.Pipelines)
		return e, err
	})
}

// InsertOpened stores o. A delivery that is already stored is ignored.
func (s *Store) InsertOpened(ctx context.Context, o Opened) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO opened (delivery_id, repository, kind, number, title, html_url, opened_at, run_id, run_attempt)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (delivery_id) DO NOTHING`,
		o.DeliveryID, o.Repository, o.Kind, o.Number, o.Title, o.HTMLURL, o.OpenedAt, o.RunID, o.RunAttempt)
	return err
}

// OpenedBy returns the PRs and Issues opened by the Runs with these run IDs,
// all attempts, oldest first.
func (s *Store) OpenedBy(ctx context.Context, runIDs []int64) ([]Opened, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT delivery_id, repository, kind, number, title, html_url, opened_at, run_id, run_attempt
		FROM opened WHERE run_id = ANY($1) ORDER BY opened_at, received_at`, runIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Opened, error) {
		var o Opened
		err := row.Scan(&o.DeliveryID, &o.Repository, &o.Kind, &o.Number, &o.Title, &o.HTMLURL, &o.OpenedAt,
			&o.RunID, &o.RunAttempt)
		return o, err
	})
}
