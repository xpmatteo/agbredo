package core

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

func tokenDeployment(t *testing.T) (*Service, *fakeBackend, Deployment, Identity) {
	s, f := testService(t)
	a, i := registeredDeployment(t, s, f)
	return s, f, a, i
}

func TestMintTokenChecksIdentityAndRegistersRun(t *testing.T) {
	s, f, a, i := tokenDeployment(t)
	ctx := context.Background()
	token, expires, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil || len(token) < 40 || time.Until(expires) < 5*time.Hour || time.Until(expires) > TokenLifetime {
		t.Fatalf("%q %v %v", token, expires, err)
	}
	r, err := s.Run(ctx, attemptRunID(a.ID, i.RunID, i.RunAttempt))
	if err != nil || r.Status != "running" {
		t.Fatalf("run was not registered: %#v %v", r, err)
	}
	if r.Ref != "refs/heads/main" {
		t.Fatalf("run ref = %q", r.Ref)
	}
	if r.ClaudeUsage != nil || r.EstimatedCostAvailable || r.AgentRuns != nil {
		t.Fatalf("a new run must have no measurements yet, not zeros: %#v", r)
	}
	var stored int
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM run_tokens WHERE token_hash=$1 OR token_hash=$2", token, hashToken(token)).Scan(&stored); err != nil || stored != 1 {
		t.Fatal("the token must be stored hashed only", stored, err)
	}
	for name, change := range map[string]func(*Identity){
		"repository": func(i *Identity) { i.Repository = "evil/repo" }, "workflow": func(i *Identity) { i.Workflow = "other.yml" },
		"attempt": func(i *Identity) { i.RunAttempt = 2 },
	} {
		wrong := i
		change(&wrong)
		if _, _, err = s.MintToken(ctx, a.ID, "review", wrong); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The ref is not part of the deployment: a run from another branch belongs to it.
	f.put(WorkflowRun{ID: 778, Attempt: 1, Workflow: a.Workflow, HeadBranch: "feature", Status: "in_progress"})
	branch := Identity{Repository: "acme/agent", Workflow: a.Workflow, Ref: "refs/heads/feature", RunID: 778, RunAttempt: 1, EventName: "push"}
	if other := startRun(t, s, a.ID, "review", branch); other.Ref != "refs/heads/feature" || other.Deployment.ID != a.ID {
		t.Errorf("run from another branch: %#v", other)
	}
	if tp := TraceParent(a.ID, i); !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(tp) || tp[3:35] != r.ID[:32] {
		t.Errorf("traceparent %q does not follow the run %s", tp, r.ID)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "bad agent run!", i); !errors.Is(err, ErrInvalid) {
		t.Errorf("agent run: %v", err)
	}
}

func TestIngestCountsEachExportOnceAndPerAgentRun(t *testing.T) {
	s, f, a, i := tokenDeployment(t)
	ctx := context.Background()
	review, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	fix, _, _ := s.MintToken(ctx, a.ID, "fix", i)
	id := attemptRunID(a.ID, i.RunID, i.RunAttempt)

	delta := func(end, value string) []byte {
		return metricsPayload("claude_code.token.usage", 1, "true", typeInput+`,{"key":"app.version","value":{"stringValue":"2.1.296"}}`, "1", end, `"asInt":"`+value+`"`)
	}
	for range 3 { // a retried export must count once
		if _, err = s.IngestMetrics(ctx, review, delta("2", "100")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.IngestMetrics(ctx, review, delta("3", "5")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestMetrics(ctx, fix, delta("2", "7")); err != nil {
		t.Fatal(err)
	}
	// A cumulative cost counter: only the growth is added; an older or lower export is ignored.
	cost := func(end, value string) []byte {
		return metricsPayload("claude_code.cost.usage", 2, "true", ``, "1", end, `"asDouble":`+value)
	}
	for _, p := range [][]byte{cost("10", "0.5"), cost("20", "0.75"), cost("15", "0.9"), cost("30", "0.1")} {
		if _, err = s.IngestMetrics(ctx, review, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.IngestLogs(ctx, review, logsPayload(str("event.name", "api_error")+`,`+str("status_code", "429"))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestLogs(ctx, review, logsPayload(str("event.name", "api_error")+`,`+str("status_code", "429"))); err != nil {
		t.Fatal(err)
	}

	r, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rv, fx := r.AgentRuns["review"], r.AgentRuns["fix"]
	if rv == nil || fx == nil || *rv.Usage.InputTokens != 105 || *fx.Usage.InputTokens != 7 || rv.Version != "2.1.296" {
		t.Fatalf("per-agent-run tokens wrong: %+v %+v", rv, fx)
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
	if _, err = s.IngestMetrics(ctx, "agb_unknown", delta("9", "1")); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unknown token accepted", err)
	}
	if _, err = s.IngestMetrics(ctx, review, []byte("nope")); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid body not rejected", err)
	}
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, HeadBranch: "main", Status: "completed", Conclusion: "success"})
	if err = webhook(s, "otlp-done", "acme/agent", WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, HeadBranch: "main", Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestMetrics(ctx, review, delta("9", "1")); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("telemetry accepted after the run finished", err)
	}
	if _, _, err = s.MintToken(ctx, a.ID, "late", i); !errors.Is(err, ErrConflict) {
		t.Fatal("token minted for a finished run", err)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	s, _, a, i := tokenDeployment(t)
	ctx := context.Background()
	token, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, "UPDATE run_tokens SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestLogs(ctx, token, logsPayload(str("event.name", "api_error"))); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token accepted", err)
	}
}

func TestIngestTracesChecksTokenAndReturnsOrigin(t *testing.T) {
	s, f, a, i := tokenDeployment(t)
	ctx := context.Background()
	f.put(WorkflowRun{ID: 777, Attempt: 1, Workflow: a.Workflow, HeadBranch: "main", Status: "in_progress", Actor: "starter", TriggeringActor: "rerunner"})
	token, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := s.IngestTraces(ctx, token, []byte(`{"resourceSpans":[]}`))
	if err != nil || origin.AgentRun != "review" || origin.Run.Deployment.ID != a.ID {
		t.Fatalf("%#v %v", origin, err)
	}
	if origin.Run.Actor != "starter" || origin.Run.TriggeringActor != "rerunner" {
		t.Fatalf("actors not stored: %q %q", origin.Run.Actor, origin.Run.TriggeringActor)
	}
	if _, err = s.IngestTraces(ctx, "agb_unknown", nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("unknown token accepted", err)
	}
}

// A run records its configuration: the trigger event from the OIDC identity,
// and per agent run the models (with tokens per model) and effort levels.
func TestIngestKeepsModelsAndEffortPerAgentRun(t *testing.T) {
	s, _, a, i := tokenDeployment(t)
	ctx := context.Background()
	review, _, err := s.MintToken(ctx, a.ID, "review", i)
	if err != nil {
		t.Fatal(err)
	}
	fix, _, _ := s.MintToken(ctx, a.ID, "fix", i)
	idle, _, _ := s.MintToken(ctx, a.ID, "idle", i)
	ingest := func(token string, bodies ...[]byte) {
		t.Helper()
		for _, b := range bodies {
			if _, err := s.IngestMetrics(ctx, token, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	tokens := func(temporality int, kind, model, effort, end, value string) []byte {
		attrs := str("type", kind) + `,` + str("model", model)
		if effort != "" {
			attrs += `,` + str("effort", effort)
		}
		return metricsPayload("claude_code.token.usage", temporality, "true", attrs, "1", end, `"asInt":"`+value+`"`)
	}
	// Delta input of model a: a retried export counts once.
	ingest(review, tokens(1, "input", "claude-a", "high", "2", "100"), tokens(1, "input", "claude-a", "high", "2", "100"), tokens(1, "input", "claude-a", "high", "3", "5"))
	// Cumulative output of model b: only the growth counts; older or lower exports are ignored.
	ingest(review, tokens(2, "output", "claude-b", "", "10", "10"), tokens(2, "output", "claude-b", "", "20", "25"), tokens(2, "output", "claude-b", "", "15", "40"), tokens(2, "output", "claude-b", "", "30", "22"))
	// Model c is seen only on the cost counter: it has no tokens.
	ingest(review, metricsPayload("claude_code.cost.usage", 1, "true", str("model", "claude-c"), "1", "2", `"asDouble":0.01`))
	// Effort from traces; a repeated export changes nothing.
	trace := tracesPayload(span("claude_code.llm_request", str("effort", "low")) + `,` + span("claude_code.llm_request", str("effort", "high")))
	for range 2 {
		if _, err = s.IngestTraces(ctx, review, trace); err != nil {
			t.Fatal(err)
		}
	}
	// A trace without effort adds no agent run; a bad trace is refused.
	if _, err = s.IngestTraces(ctx, idle, tracesPayload(span("claude_code.tool", ""))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestTraces(ctx, idle, []byte(`{`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid trace accepted", err)
	}
	// Models beyond the limit still count in the totals.
	for n := range maxModels + 4 {
		ingest(fix, tokens(1, "input", "claude-"+string(rune('a'+n)), "", "2", "1"))
	}

	r, err := s.Run(ctx, attemptRunID(a.ID, i.RunID, i.RunAttempt))
	if err != nil {
		t.Fatal(err)
	}
	if r.TriggerEvent != "schedule" {
		t.Errorf("trigger event = %q", r.TriggerEvent)
	}
	rv := r.AgentRuns["review"]
	if len(rv.Models) != 3 {
		t.Fatalf("models: %v", rv.Models)
	}
	ma, mb, mc := rv.Models["claude-a"], rv.Models["claude-b"], rv.Models["claude-c"]
	if *ma.InputTokens != 105 || ma.OutputTokens != nil || *mb.OutputTokens != 25 || mb.InputTokens != nil || *mc != (ClaudeUsage{}) {
		t.Fatalf("tokens per model: %+v %+v %+v", ma, mb, mc)
	}
	if *rv.Usage.InputTokens != 105 || *rv.Usage.OutputTokens != 25 {
		t.Fatalf("agent run totals: %+v", rv.Usage)
	}
	if len(rv.Efforts) != 2 || rv.Efforts[0] != "low" || rv.Efforts[1] != "high" {
		t.Fatalf("efforts = %v", rv.Efforts)
	}
	fx := r.AgentRuns["fix"]
	if len(fx.Models) != maxModels || *fx.Usage.InputTokens != int64(maxModels+4) || r.InputTokens != 105+int64(maxModels+4) {
		t.Fatalf("model limit: %d models, %d tokens, run %d", len(fx.Models), *fx.Usage.InputTokens, r.InputTokens)
	}
	if fx.Efforts != nil {
		t.Fatalf("effort without any report must stay missing: %v", fx.Efforts)
	}
	if _, ok := r.AgentRuns["idle"]; ok {
		t.Fatal("a trace without effort added an agent run")
	}
}

func TestAddEffortKeepsASmallOrderedSet(t *testing.T) {
	u := &AgentRunUsage{}
	for _, e := range []string{"max", "low", "future", "high", "low", "", "medium", "xhigh", "a", "b", "c", "d"} {
		u.addEffort(e)
	}
	want := []string{"low", "medium", "high", "xhigh", "max", "a", "b", "future"}
	if len(u.Efforts) != len(want) {
		t.Fatalf("%v", u.Efforts)
	}
	for n := range want {
		if u.Efforts[n] != want[n] {
			t.Fatalf("%v, want %v", u.Efforts, want)
		}
	}
}
