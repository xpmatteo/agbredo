package core

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
)

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readJSON[T any](ctx context.Context, q querier, sql string, args ...any) (T, error) {
	var result T
	var data []byte
	err := q.QueryRow(ctx, sql, args...).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	return result, err
}
func saveRun(ctx context.Context, tx pgx.Tx, r Run) error {
	r.UpdatedAt = now()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE runs SET document=$2,status=$3,updated_at=now() WHERE id=$1", r.ID, data, r.Status)
	return err
}
func insertEvent(ctx context.Context, tx pgx.Tx, e Event) (bool, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "INSERT INTO events(run_id,id,document) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", e.RunID, e.ID, data)
	return tag.RowsAffected() == 1, err
}
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
