package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	DB      *pgxpool.Pool
	Backend Backend
	// DefaultThresholdMicros is the threshold of a deployment created by its
	// first run (see MintTokenForWorkflow).
	DefaultThresholdMicros int64
}

func (s *Service) Deployment(ctx context.Context, id string) (Deployment, error) {
	return readJSON[Deployment](ctx, s.DB, "SELECT document FROM agents WHERE id=$1 AND NOT deleted", id)
}
func (s *Service) Run(ctx context.Context, id string) (Run, error) {
	return readJSON[Run](ctx, s.DB, "SELECT document FROM runs WHERE id=$1", id)
}
func (s *Service) SaveDeployment(ctx context.Context, a Deployment) (Deployment, error) {
	if err := ValidateDeployment(a); err != nil {
		return a, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	if a.ID == "" {
		a.ID = ID()
		a.Revision = 1
		_, err = tx.Exec(ctx, "INSERT INTO agents(id,document) VALUES($1,$2)", a.ID, mustJSON(a))
	} else {
		previous, e := readJSON[Deployment](ctx, tx, "SELECT document FROM agents WHERE id=$1 AND NOT deleted FOR UPDATE", a.ID)
		if e != nil {
			return a, e
		}
		if a.Revision != previous.Revision {
			return a, fmt.Errorf("deployment was updated by another user; reload its configuration: %w", ErrConflict)
		}
		a.Revision = previous.Revision + 1
		_, err = tx.Exec(ctx, "UPDATE agents SET document=$2 WHERE id=$1", a.ID, mustJSON(a))
	}
	if isDuplicateDeployment(err) {
		return a, invalid(fmt.Sprintf("a deployment of %s/%s with workflow %s is already registered", a.Owner, a.Repository, a.Workflow))
	}
	if err != nil {
		return a, err
	}
	return a, tx.Commit(ctx)
}

// isDuplicateDeployment says whether err is a violation of the unique index on
// repository + workflow file (migrations/006_unique_deployment.sql).
func isDuplicateDeployment(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "agents_repository_workflow"
}
func (s *Service) DeleteDeployment(ctx context.Context, id string) error {
	tag, err := s.DB.Exec(ctx, "UPDATE agents SET deleted=true WHERE id=$1 AND NOT deleted", id)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
func (s *Service) Fleet(ctx context.Context) (Fleet, error) {
	var f Fleet
	rows, err := s.DB.Query(ctx, "SELECT document FROM agents WHERE NOT deleted ORDER BY document->>'name'")
	if err != nil {
		return f, err
	}
	f.Deployments, err = collect[Deployment](rows)
	if err != nil {
		return f, err
	}
	rows, err = s.DB.Query(ctx, "SELECT document FROM runs ORDER BY created_at DESC")
	if err != nil {
		return f, err
	}
	f.Runs, err = collect[Run](rows)
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
