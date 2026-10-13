package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agbredo/internal/core"
)

type testAuth struct {
	user      string
	validCSRF bool
}

func (a testAuth) User(*http.Request) string { return a.user }
func (a testAuth) CSRF(*http.Request) string { return "test-csrf" }
func (a testAuth) CheckCSRF(r *http.Request) bool {
	return a.validCSRF && r.FormValue("csrf") == "test-csrf"
}

type testService struct {
	agent           core.Agent
	run             core.Run
	approval        core.Approval
	events          []core.Event
	decisions       int
	actor, decision string
	decisionErr     error
	saved           core.Agent
}

func (s *testService) Fleet(context.Context) (core.Fleet, error) {
	return core.Fleet{Agents: []core.Agent{s.agent}, Runs: []core.Run{s.run}, Approvals: []core.Approval{s.approval}}, nil
}
func (s *testService) Agent(context.Context, string) (core.Agent, error) { return s.agent, nil }
func (s *testService) SaveAgent(_ context.Context, a core.Agent) (core.Agent, error) {
	s.saved = a
	a.ID = "agent-1"
	return a, nil
}
func (s *testService) DeleteAgent(context.Context, string) error                { return nil }
func (s *testService) Launch(context.Context, string, string) (core.Run, error) { return s.run, nil }
func (s *testService) Run(context.Context, string) (core.Run, error)            { return s.run, nil }
func (s *testService) Events(context.Context, string) ([]core.Event, error)     { return s.events, nil }
func (s *testService) Approval(context.Context, string) (core.Approval, error) {
	return s.approval, nil
}
func (s *testService) Decide(_ context.Context, _, actor, decision, _ string) error {
	s.decisions++
	s.actor, s.decision = actor, decision
	return s.decisionErr
}

func serve(service *testService, auth testAuth, method, path string, form url.Values) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	New(service, auth).Register(mux)
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestApprovalAuthorizationAndCSRF(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth testAuth
		form url.Values
		want int
	}{
		{"unauthenticated", testAuth{"", true}, url.Values{"csrf": {"test-csrf"}, "decision": {"approve"}}, http.StatusUnauthorized},
		{"missing csrf", testAuth{"admin", true}, url.Values{"decision": {"approve"}}, http.StatusForbidden},
		{"invalid csrf", testAuth{"admin", false}, url.Values{"csrf": {"test-csrf"}, "decision": {"approve"}}, http.StatusForbidden},
		{"invalid decision", testAuth{"admin", true}, url.Values{"csrf": {"test-csrf"}, "decision": {"executed"}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &testService{}
			w := serve(s, tc.auth, http.MethodPost, "/approvals/a", tc.form)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if s.decisions != 0 {
				t.Fatal("unauthorized request reached decision service")
			}
		})
	}
}

func TestApprovalDecisionAndServiceAuthorization(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		s := &testService{}
		w := serve(s, testAuth{"admin", true}, http.MethodPost, "/approvals/a", url.Values{"csrf": {"test-csrf"}, "decision": {decision}})
		if w.Code != http.StatusSeeOther || s.actor != "admin" || s.decision != decision {
			t.Fatalf("decision not passed to service: status %d, actor %q, decision %q", w.Code, s.actor, s.decision)
		}
	}
	s := &testService{decisionErr: core.ErrUnauthorized}
	w := serve(s, testAuth{"not-allowed", true}, http.MethodPost, "/approvals/a", url.Values{"csrf": {"test-csrf"}, "decision": {"approve"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("service authorization failure status = %d", w.Code)
	}
}

func TestBudgetDecimalConversion(t *testing.T) {
	for _, tc := range []struct {
		value  string
		micros int64
	}{{"0", 0}, {"0.000001", 1}, {"12.345678", 12345678}, {"5.1", 5100000}, {"9223372036854.775807", 9223372036854775807}} {
		got, err := parseMoney(tc.value)
		if err != nil || got != tc.micros {
			t.Fatalf("parseMoney(%q) = %d, %v; want %d", tc.value, got, err, tc.micros)
		}
	}
	for _, value := range []string{"", "-1", "+1", "NaN", "1e6", ".5", "1.", "0.0000001", "9223372036854.775808", "1.1.1"} {
		if _, err := parseMoney(value); err == nil {
			t.Fatalf("parseMoney(%q) accepted invalid amount", value)
		}
	}
}

func TestAgentFormExactCost(t *testing.T) {
	s := &testService{}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents", url.Values{"csrf": {"test-csrf"}, "name": {"Example"}, "owner": {"example"}, "repository": {"example"}, "workflow": {"propose.yml"}, "continuation_workflow": {"continue.yml"}, "ref": {"main"}, "installation_id": {"123"}, "budget": {"0.000003"}, "enabled": {"on"}})
	if w.Code != http.StatusSeeOther || s.saved.BudgetMicros != 3 {
		t.Fatalf("save status = %d, micros = %d", w.Code, s.saved.BudgetMicros)
	}
}

func TestObservationDashboardSeparatesEstimatesFromChargesAndMissingUsage(t *testing.T) {
	s := &testService{agent: core.Agent{ID: "agent-1", Mode: "observe", Enabled: true}, run: core.Run{ID: "run-1", Agent: core.Agent{Mode: "observe"}, Status: "running"}}
	w := serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil)
	if strings.Contains(w.Body.String(), "Launch workflow") || !strings.Contains(w.Body.String(), "Observe existing workflows") {
		t.Fatal("observation UI offered dispatch", w.Body.String())
	}
	w = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil)
	for _, text := range []string{"Unavailable", "Actual subscription charges unknown", "Observation only; execution remains in GitHub Actions"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("missing usage distinction: %s", text)
		}
	}
	zero := int64(0)
	s.run.ClaudeUsage = &core.ClaudeUsage{InputTokens: &zero}
	s.run.EstimatedCostAvailable = true
	s.events = []core.Event{{Type: "claude.execution", Claude: &core.Observation{Version: "2.1.296", Phase: "finished", ExitCode: new(int), Usage: s.run.ClaudeUsage, EstimatedCostMicros: &zero}}}
	w = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil)
	if !strings.Contains(w.Body.String(), "Exit 0") || !strings.Contains(w.Body.String(), "API-equivalent estimate $0.000000") {
		t.Fatal("measured zero or CLI status lost", w.Body.String())
	}
}

func TestPagesEscapeUntrustedContent(t *testing.T) {
	now := time.Now()
	s := &testService{agent: core.Agent{ID: "agent", Name: "Example", Version: 1}, run: core.Run{ID: "run", Agent: core.Agent{Name: "Example"}}, approval: core.Approval{ID: "approval", RunID: "run", Status: "pending", ExpiresAt: now.Add(time.Hour), Proposal: core.Proposal{Title: "<script>alert(1)</script>", Body: "<img src=x onerror=alert(1)>"}}, events: []core.Event{{Message: "<script>secret()</script>"}}}
	for _, path := range []string{"/", "/agents", "/agents/agent", "/runs", "/runs/run", "/approvals", "/approvals/approval"} {
		w := serve(s, testAuth{"admin", true}, http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("%s template did not render completely", path)
		}
		if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<img src=x") {
			t.Fatalf("%s rendered executable untrusted content", path)
		}
	}
	// Internal errors may contain credentials and must not be reflected to browsers.
	s.decisionErr = errors.New("postgres://user:password@internal")
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/approvals/a", url.Values{"csrf": {"test-csrf"}, "decision": {"approve"}})
	if strings.Contains(w.Body.String(), "password") {
		t.Fatal("internal error leaked to browser")
	}
}

func TestOversizedApprovalForm(t *testing.T) {
	s := &testService{}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/approvals/a", url.Values{"csrf": {"test-csrf"}, "decision": {"approve"}, "reason": {strings.Repeat("a", 65<<10)}})
	if w.Code != http.StatusBadRequest || s.decisions != 0 {
		t.Fatalf("oversized approval accepted: status = %d decisions = %d", w.Code, s.decisions)
	}
}
