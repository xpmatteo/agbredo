package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func tokenAgent(t *testing.T) (*Service, *fakeBackend, Agent, Identity) {
	s, f := testService(t)
	a, i := reportAgent(t, s, f)
	return s, f, a, i
}

func TestMintTokenChecksIdentityAndRegistersRun(t *testing.T) {
	s, _, a, i := tokenAgent(t)
	ctx := context.Background()
	token, expires, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil || len(token) < 40 || time.Until(expires) < 5*time.Hour || time.Until(expires) > TokenLifetime {
		t.Fatalf("%q %v %v", token, expires, err)
	}
	r, err := s.Run(ctx, reportRunID(a.ID, i.RunID, i.RunAttempt))
	if err != nil || r.Status != "running" {
		t.Fatalf("run was not registered: %#v %v", r, err)
	}
	var stored int
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM run_tokens WHERE token_hash=$1 OR token_hash=$2", token, hashToken(token)).Scan(&stored); err != nil || stored != 1 {
		t.Fatal("the token must be stored hashed only", stored, err)
	}
	for name, change := range map[string]func(*Identity){
		"repository": func(i *Identity) { i.Repository = "evil/repo" }, "workflow": func(i *Identity) { i.Workflow = "other.yml" },
		"ref": func(i *Identity) { i.Ref = "refs/heads/other" }, "attempt": func(i *Identity) { i.RunAttempt = 2 },
	} {
		wrong := i
		change(&wrong)
		if _, _, err = s.MintToken(ctx, a.ID, "review", wrong); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err = s.MintToken(ctx, a.ID, "bad source!", i); !errors.Is(err, ErrInvalid) {
		t.Errorf("source: %v", err)
	}
}

func TestIngestCountsEachExportOnceAndPerSource(t *testing.T) {
	s, f, a, i := tokenAgent(t)
	ctx := context.Background()
	review, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	fix, _, _ := s.MintToken(ctx, a.ID, "fix", i)
	id := reportRunID(a.ID, i.RunID, i.RunAttempt)

	delta := func(end, value string) []byte {
		return metricsPayload("claude_code.token.usage", 1, "true", typeInput+`,{"key":"app.version","value":{"stringValue":"2.1.296"}}`, "1", end, `"asInt":"`+value+`"`)
	}
	for range 3 { // a retried export must count once
		if err = s.IngestMetrics(ctx, review, delta("2", "100")); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.IngestMetrics(ctx, review, delta("3", "5")); err != nil {
		t.Fatal(err)
	}
	if err = s.IngestMetrics(ctx, fix, delta("2", "7")); err != nil {
		t.Fatal(err)
	}
	// A cumulative cost counter: only the growth is added; an older or lower export is ignored.
	cost := func(end, value string) []byte {
		return metricsPayload("claude_code.cost.usage", 2, "true", ``, "1", end, `"asDouble":`+value)
	}
	for _, p := range [][]byte{cost("10", "0.5"), cost("20", "0.75"), cost("15", "0.9"), cost("30", "0.1")} {
		if err = s.IngestMetrics(ctx, review, p); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.IngestLogs(ctx, review, logsPayload(str("event.name", "api_error")+`,`+str("status_code", "429"))); err != nil {
		t.Fatal(err)
	}
	if err = s.IngestLogs(ctx, review, logsPayload(str("event.name", "api_error")+`,`+str("status_code", "429"))); err != nil {
		t.Fatal(err)
	}

	r, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rv, fx := r.Sources["review"], r.Sources["fix"]
	if rv == nil || fx == nil || *rv.Usage.InputTokens != 105 || *fx.Usage.InputTokens != 7 || rv.Version != "2.1.296" {
		t.Fatalf("per-source tokens wrong: %+v %+v", rv, fx)
	}
	if rv.Usage.OutputTokens != nil || fx.CostNanos != nil || r.ClaudeUsage.CacheReadTokens != nil {
		t.Fatal("a missing measurement was stored as zero")
	}
	if rv.CostMicros() != 750_000 || r.CostMicros != 750_000 || !r.EstimatedCostAvailable {
		t.Fatalf("cost wrong: %d %d", rv.CostMicros(), r.CostMicros)
	}
	if r.InputTokens != 112 || *r.ClaudeUsage.InputTokens != 112 || rv.APIErrors != 1 || rv.RateLimitFailures != 1 || r.APIErrors != 1 {
		t.Fatalf("totals wrong: %#v", r)
	}

	// Unknown tokens are refused, and so is telemetry after GitHub says the run finished.
	if err = s.IngestMetrics(ctx, "agb_unknown", delta("9", "1")); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unknown token accepted", err)
	}
	if err = s.IngestMetrics(ctx, review, []byte("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid body not rejected", err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, Ref: "main", Status: "completed", Conclusion: "success"})
	if err = s.Webhook(ctx, "otlp-done", "acme/agent", 777); err != nil {
		t.Fatal(err)
	}
	if err = s.IngestMetrics(ctx, review, delta("9", "1")); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("telemetry accepted after the run finished", err)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "late", i); !errors.Is(err, ErrConflict) {
		t.Fatal("token minted for a finished run", err)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	s, _, a, i := tokenAgent(t)
	ctx := context.Background()
	token, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE run_tokens SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if err = s.IngestLogs(ctx, token, logsPayload(str("event.name", "api_error"))); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token accepted", err)
	}
}
