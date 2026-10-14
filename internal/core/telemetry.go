package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

const TokenLifetime = 6 * time.Hour

// SourceUsage is what one Claude Code step used. Cost is kept in nanodollars
// so that many small exports add up without drifting; it is shown as micros.
type SourceUsage struct {
	Version           string       `json:"version,omitempty"`
	Usage             *ClaudeUsage `json:"usage,omitempty"`
	CostNanos         *int64       `json:"cost_nanos,omitempty"`
	APIErrors         int64        `json:"api_errors,omitempty"`
	RateLimitFailures int64        `json:"rate_limit_failures,omitempty"`
}

func (u SourceUsage) CostMicros() int64 {
	if u.CostNanos == nil {
		return 0
	}
	return int64(math.Round(float64(*u.CostNanos) / 1000))
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// MintToken gives one Claude Code step a token for posting telemetry. The
// runner proves who it is with its GitHub identity, exactly as for a report.
// The token only identifies this run attempt and source; it is not an identity
// for anything else. This also registers the run.
func (s *Service) MintToken(ctx context.Context, deploymentID, source string, i Identity) (string, time.Time, error) {
	if !slug.MatchString(source) || len(source) > 80 {
		return "", time.Time{}, invalid("provide a stable source name")
	}
	if i.RunID <= 0 || i.RunAttempt <= 0 {
		return "", time.Time{}, ErrUnauthorized
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback(ctx)
	r, gh, err := s.lockRun(ctx, tx, deploymentID, i)
	if err != nil {
		return "", time.Time{}, err
	}
	r.Status = reportedStatus(r.Status, gh)
	if Terminal(r.Status) {
		return "", time.Time{}, ErrConflict
	}
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	token := "agb_" + hex.EncodeToString(b[:])
	expires := now().Add(TokenLifetime)
	if _, err = tx.Exec(ctx, "INSERT INTO run_tokens(token_hash,run_id,source,expires_at) VALUES($1,$2,$3,$4)", hashToken(token), r.ID, source, expires); err != nil {
		return "", time.Time{}, err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, tx.Commit(ctx)
}

// TraceParent is a W3C traceparent for one Claude Code step. The trace ID is
// the first half of the run ID, so every step of a run attempt shares one
// trace. The parent span ID is new each time.
func TraceParent(deploymentID string, i Identity) string {
	var span [8]byte
	if _, err := rand.Read(span[:]); err != nil {
		panic(err)
	}
	return "00-" + reportRunID(deploymentID, i.RunID, i.RunAttempt)[:32] + "-" + hex.EncodeToString(span[:]) + "-01"
}

// TelemetryOrigin is the run and Claude Code step a telemetry token belongs to.
type TelemetryOrigin struct {
	Run    Run
	Source string
}

// IngestMetrics and IngestLogs take a body of OTLP JSON from a Claude Code step.
// Telemetry for a finished run, or with an unknown or expired token, is refused.
func (s *Service) IngestMetrics(ctx context.Context, token string, body []byte) (TelemetryOrigin, error) {
	return s.ingest(ctx, token, func(tx pgx.Tx, r *Run, source string) error {
		samples, version, err := parseMetrics(body)
		if err != nil {
			return err
		}
		return applyMetrics(ctx, tx, r, source, samples, version)
	})
}
func (s *Service) IngestLogs(ctx context.Context, token string, body []byte) (TelemetryOrigin, error) {
	return s.ingest(ctx, token, func(tx pgx.Tx, r *Run, source string) error {
		events, err := parseLogs(body)
		if err != nil {
			return err
		}
		return applyLogs(ctx, tx, r, source, events)
	})
}

// IngestTraces only checks the token. Agbredo counts nothing from traces;
// they are forwarded when a store is configured.
func (s *Service) IngestTraces(ctx context.Context, token string, _ []byte) (TelemetryOrigin, error) {
	return s.ingest(ctx, token, func(pgx.Tx, *Run, string) error { return nil })
}

func (s *Service) ingest(ctx context.Context, token string, apply func(pgx.Tx, *Run, string) error) (TelemetryOrigin, error) {
	var origin TelemetryOrigin
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return origin, err
	}
	defer tx.Rollback(ctx)
	var runID, source string
	var expires time.Time
	err = tx.QueryRow(ctx, "SELECT run_id,source,expires_at FROM run_tokens WHERE token_hash=$1", hashToken(token)).Scan(&runID, &source, &expires)
	if err == pgx.ErrNoRows || (err == nil && !expires.After(now())) {
		return origin, ErrUnauthorized
	}
	if err != nil {
		return origin, err
	}
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", runID)
	if err != nil {
		return origin, err
	}
	if Terminal(r.Status) {
		return origin, ErrUnauthorized
	}
	if err = apply(tx, &r, source); err != nil {
		return origin, err
	}
	if err = saveRun(ctx, tx, r); err != nil {
		return origin, err
	}
	return TelemetryOrigin{Run: r, Source: source}, tx.Commit(ctx)
}

// markSeen reports whether this export is new.
func markSeen(ctx context.Context, tx pgx.Tx, runID, source, key string) (bool, error) {
	tag, err := tx.Exec(ctx, "INSERT INTO otlp_seen(run_id,source,key) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", runID, source, key)
	return tag.RowsAffected() == 1, err
}

func sourceUsage(r *Run, source string) *SourceUsage {
	if r.Sources == nil {
		r.Sources = map[string]*SourceUsage{}
	}
	if r.Sources[source] == nil {
		r.Sources[source] = &SourceUsage{}
	}
	return r.Sources[source]
}

func applyMetrics(ctx context.Context, tx pgx.Tx, r *Run, source string, samples []metricSample, version string) error {
	u := sourceUsage(r, source)
	if u.Version == "" {
		u.Version = version
	}
	for _, m := range samples {
		fresh, err := markSeen(ctx, tx, r.ID, source, m.Key)
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		amount := m.Amount
		if m.Cumulative {
			var lastTime int64
			var lastValue float64
			err = tx.QueryRow(ctx, "SELECT last_time,last_value FROM otlp_series WHERE run_id=$1 AND source=$2 AND series=$3", r.ID, source, m.Series).Scan(&lastTime, &lastValue)
			if err != nil && err != pgx.ErrNoRows {
				return err
			}
			// An older export, or a counter which went down, is not counted.
			if m.End <= lastTime || amount < lastValue {
				continue
			}
			if _, err = tx.Exec(ctx, "INSERT INTO otlp_series(run_id,source,series,last_time,last_value) VALUES($1,$2,$3,$4,$5) ON CONFLICT (run_id,source,series) DO UPDATE SET last_time=$4,last_value=$5", r.ID, source, m.Series, m.End, amount); err != nil {
				return err
			}
			amount -= lastValue
		}
		addUsage(r, u, m.Field, amount)
	}
	return nil
}

func addUsage(r *Run, u *SourceUsage, field string, amount float64) {
	if field == "cost" {
		before := u.CostMicros()
		if u.CostNanos == nil {
			u.CostNanos = new(int64)
		}
		*u.CostNanos += int64(math.Round(amount * 1e9))
		r.CostMicros += u.CostMicros() - before
		r.EstimatedCostAvailable = true
		r.OverBudget = r.CostMicros > r.BudgetMicros
		return
	}
	n := int64(math.Round(amount))
	if u.Usage == nil {
		u.Usage = &ClaudeUsage{}
	}
	if r.ClaudeUsage == nil {
		r.ClaudeUsage = &ClaudeUsage{}
	}
	switch field {
	case "input":
		r.InputTokens += n
		addMeasured(&u.Usage.InputTokens, &n)
		addMeasured(&r.ClaudeUsage.InputTokens, &n)
	case "output":
		r.OutputTokens += n
		addMeasured(&u.Usage.OutputTokens, &n)
		addMeasured(&r.ClaudeUsage.OutputTokens, &n)
	case "cacheRead":
		addMeasured(&u.Usage.CacheReadTokens, &n)
		addMeasured(&r.ClaudeUsage.CacheReadTokens, &n)
	case "cacheCreation":
		addMeasured(&u.Usage.CacheCreationTokens, &n)
		addMeasured(&r.ClaudeUsage.CacheCreationTokens, &n)
	}
}

func applyLogs(ctx context.Context, tx pgx.Tx, r *Run, source string, events []logEvent) error {
	u := sourceUsage(r, source)
	for _, e := range events {
		fresh, err := markSeen(ctx, tx, r.ID, source, e.Key)
		if err != nil {
			return err
		}
		if !fresh {
			continue
		}
		u.APIErrors++
		r.APIErrors++
		if e.RateLimit {
			u.RateLimitFailures++
			r.RateLimitFailures++
		}
	}
	return nil
}
