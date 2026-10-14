package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
	deployment core.Deployment
	run        core.Run
	recent     []core.Run
	events     []core.Event
	saved      core.Deployment
	saveErr    error
}

func (s *testService) Fleet(context.Context) (core.Fleet, error) {
	return core.Fleet{Deployments: []core.Deployment{s.deployment}, Runs: []core.Run{s.run}}, nil
}
func (s *testService) Deployment(context.Context, string) (core.Deployment, error) {
	return s.deployment, nil
}
func (s *testService) SaveDeployment(_ context.Context, a core.Deployment) (core.Deployment, error) {
	s.saved = a
	a.ID = "agent-1"
	return a, s.saveErr
}
func (s *testService) DeleteDeployment(context.Context, string) error { return nil }
func (s *testService) Run(context.Context, string) (core.Run, error)  { return s.run, nil }
func (s *testService) RecentRuns(context.Context, string, int) ([]core.Run, error) {
	return s.recent, nil
}
func (s *testService) Events(context.Context, string) ([]core.Event, error) { return s.events, nil }

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

func TestMutationsRequireAuthenticationAndCSRF(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth testAuth
		form url.Values
		want int
	}{
		{"unauthenticated", testAuth{"", true}, url.Values{"csrf": {"test-csrf"}}, http.StatusUnauthorized},
		{"missing csrf", testAuth{"admin", true}, url.Values{}, http.StatusForbidden},
		{"invalid csrf", testAuth{"admin", false}, url.Values{"csrf": {"test-csrf"}}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &testService{}
			w := serve(s, tc.auth, http.MethodPost, "/agents/agent-1/delete", tc.form)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestManagedRoutesAreGone(t *testing.T) {
	for _, path := range []string{"/approvals", "/approvals/a"} {
		if w := serve(&testService{}, testAuth{"admin", true}, http.MethodGet, path, nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, w.Code)
		}
	}
	if w := serve(&testService{}, testAuth{"admin", true}, http.MethodPost, "/agents/agent-1/launch", url.Values{"csrf": {"test-csrf"}}); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("launch status = %d", w.Code)
	}
}

func TestThresholdDecimalConversion(t *testing.T) {
	for _, tc := range []struct {
		value  string
		micros int64
	}{{"0", 0}, {"0.000001", 1}, {"12.345678", 12345678}, {"5.1", 5100000}, {"9223372036854.775807", 9223372036854775807}} {
		got, err := core.ParseUSD(tc.value)
		if err != nil || got != tc.micros {
			t.Fatalf("core.ParseUSD(%q) = %d, %v; want %d", tc.value, got, err, tc.micros)
		}
	}
	for _, value := range []string{"", "-1", "+1", "NaN", "1e6", ".5", "1.", "0.0000001", "9223372036854.775808", "1.1.1"} {
		if _, err := core.ParseUSD(value); err == nil {
			t.Fatalf("core.ParseUSD(%q) accepted invalid amount", value)
		}
	}
}

func TestDeploymentFormExactCost(t *testing.T) {
	s := &testService{}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents", url.Values{"csrf": {"test-csrf"}, "name": {"Example"}, "owner": {"example"}, "repository": {"example"}, "workflow": {"agent.yml"}, "installation_id": {"123"}, "budget": {"0.000003"}, "enabled": {"on"}})
	if w.Code != http.StatusSeeOther || s.saved.ThresholdMicros != 3 {
		t.Fatalf("save status = %d, micros = %d", w.Code, s.saved.ThresholdMicros)
	}
}

// A deployment is identified by repository + workflow file: the form asks for
// no ref, and a duplicate is refused with a readable message.
func TestDeploymentFormHasNoRefAndRefusesDuplicates(t *testing.T) {
	s := &testService{deployment: core.Deployment{ID: "agent-1", Owner: "acme", Repository: "agent", Workflow: "agent.yml", Ref: "main"}}
	for _, path := range []string{"/agents", "/agents/agent-1"} {
		body := serve(s, testAuth{"admin", true}, http.MethodGet, path, nil).Body.String()
		if strings.Contains(body, `name="ref"`) || strings.Contains(body, "@ main") {
			t.Fatalf("%s still shows a ref: %s", path, body)
		}
	}
	s.saveErr = fmt.Errorf("a deployment of acme/agent with workflow agent.yml already exists: %w", core.ErrInvalid)
	form := url.Values{"csrf": {"test-csrf"}, "name": {"Example"}, "owner": {"acme"}, "repository": {"agent"}, "workflow": {"agent.yml"}, "installation_id": {"123"}, "budget": {"1"}, "enabled": {"on"}}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents", form)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "already exists") {
		t.Fatalf("duplicate: status = %d, body = %s", w.Code, w.Body.String())
	}
}

// The run page shows the run's own ref. Older runs fall back to the ref of
// their deployment copy, and otherwise show it as missing.
func TestRunPageShowsRunRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  core.Run
		want string
	}{
		{"own ref", core.Run{ID: "run-1", Ref: "refs/heads/feature", Deployment: core.Deployment{Ref: "main"}}, `<dt>Ref</dt><dd class="mono">refs/heads/feature</dd>`},
		{"legacy copy", core.Run{ID: "run-1", Deployment: core.Deployment{Ref: "main"}}, `<dt>Ref</dt><dd class="mono">main</dd>`},
		{"none", core.Run{ID: "run-1"}, `<dt>Ref</dt><dd class="mono">Unavailable</dd>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := serve(&testService{run: tc.run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Fatalf("want %s in %s", tc.want, body)
			}
		})
	}
}

func TestDashboardSeparatesEstimatesFromChargesAndMissingUsage(t *testing.T) {
	s := &testService{deployment: core.Deployment{ID: "agent-1", Enabled: true}, run: core.Run{ID: "run-1", Status: "running"}}
	w := serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil)
	if strings.Contains(w.Body.String(), "Launch workflow") || !strings.Contains(w.Body.String(), "Reporting enabled") {
		t.Fatal("deployment page offered dispatch", w.Body.String())
	}
	w = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil)
	for _, text := range []string{"Unavailable", "Actual subscription charges unknown", "Runs in GitHub Actions; Agbredo only receives its telemetry and GitHub status"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Fatalf("missing usage distinction: %s", text)
		}
	}
	zero := int64(0)
	s.run.ClaudeUsage = &core.ClaudeUsage{InputTokens: &zero}
	s.run.EstimatedCostAvailable = true
	w = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil)
	if !strings.Contains(w.Body.String(), "$0.000000") || !strings.Contains(w.Body.String(), "<strong>0</strong>") {
		t.Fatal("measured zero lost", w.Body.String())
	}
}

func TestPagesEscapeUntrustedContent(t *testing.T) {
	s := &testService{deployment: core.Deployment{ID: "agent", Name: "Example", Revision: 1}, run: core.Run{ID: "run", Deployment: core.Deployment{Name: "Example"}}, events: []core.Event{{Message: "<script>secret()</script>"}}}
	for _, path := range []string{"/", "/agents", "/agents/agent", "/runs", "/runs/run"} {
		w := serve(s, testAuth{"admin", true}, http.MethodGet, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "</html>") {
			t.Fatalf("%s template did not render completely", path)
		}
		if strings.Contains(w.Body.String(), "<script>") {
			t.Fatalf("%s rendered executable untrusted content", path)
		}
	}
}

func TestOversizedForm(t *testing.T) {
	s := &testService{}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents", url.Values{"csrf": {"test-csrf"}, "name": {strings.Repeat("a", 65<<10)}})
	if w.Code != http.StatusBadRequest || s.saved.Name != "" {
		t.Fatalf("oversized form accepted: status = %d", w.Code)
	}
}

func TestRunPageShowsEachClaudeStepAndKeepsMissingValuesMissing(t *testing.T) {
	n := int64(1200)
	s := &testService{run: core.Run{ID: "run-1", AgentRuns: map[string]*core.AgentRunUsage{
		"review": {Version: "2.1.296", Usage: &core.ClaudeUsage{InputTokens: &n}, CostNanos: new(int64)},
		"fix":    {},
	}}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	if !strings.Contains(body, "Agent runs") || strings.Index(body, ">fix<") > strings.Index(body, ">review<") {
		t.Fatal("steps missing or not sorted", body)
	}
	review := body[strings.Index(body, ">review<"):]
	review = review[:strings.Index(review, "</tr>")]
	fix := body[strings.Index(body, ">fix<"):]
	fix = fix[:strings.Index(fix, "</tr>")]
	if !strings.Contains(review, "2.1.296") || !strings.Contains(review, "$0.000000") || !strings.Contains(review, "1200") || !strings.Contains(review, "<td>Unavailable / Unavailable</td>") {
		t.Fatal("measured values wrong:", review)
	}
	if strings.Contains(fix, "$") || strings.Contains(fix, ">0<") || strings.Count(fix, "Unavailable") < 5 {
		t.Fatal("missing values shown as zero:", fix)
	}
	s.run.AgentRuns = nil
	if strings.Contains(serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(), "Agent runs") {
		t.Fatal("empty table shown")
	}
}

func TestSaveDeploymentRevisionField(t *testing.T) {
	form := func(version string) url.Values {
		v := url.Values{"csrf": {"test-csrf"}, "name": {"Example"}, "owner": {"example"}, "repository": {"example"}, "workflow": {"agent.yml"}, "installation_id": {"123"}, "budget": {"1"}, "enabled": {"on"}}
		if version != "" {
			v.Set("version", version)
		}
		return v
	}
	s := &testService{}
	w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents/agent-1", form("3"))
	if w.Code != http.StatusSeeOther || s.saved.Revision != 3 {
		t.Fatalf("status = %d, revision = %d", w.Code, s.saved.Revision)
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		s := &testService{}
		w := serve(s, testAuth{"admin", true}, http.MethodPost, "/agents/agent-1", form(bad))
		if w.Code != http.StatusBadRequest || s.saved.Revision != 0 {
			t.Fatalf("version %q: status = %d", bad, w.Code)
		}
	}
}

// The run page shows the pipeline and its version: the ref and a short SHA
// linked to the pipeline's file at that commit. Missing stays missing.
func TestRunPageShowsPipelineVersion(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	run := core.Run{ID: "run-1", Pipeline: &core.Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1", SHA: sha}}
	body := serve(&testService{run: run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	for _, want := range []string{
		`<dt>Pipeline</dt><dd class="mono">org/agents/.github/workflows/triage.yml</dd>`,
		`<dt>Pipeline version</dt><dd class="mono">refs/tags/v1 · <a href="https://github.com/org/agents/blob/` + sha + `/.github/workflows/triage.yml"`,
		`>0123456</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	body = serve(&testService{run: core.Run{ID: "run-1"}}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	if !strings.Contains(body, `<dt>Pipeline</dt><dd class="mono">Unavailable</dd><dt>Pipeline version</dt><dd class="mono">Unavailable</dd>`) {
		t.Fatalf("missing pipeline not shown as Unavailable: %s", body)
	}
}

// The deployment page shows which pipeline versions its recent runs used.
func TestDeploymentPageShowsPipelineVersions(t *testing.T) {
	v1 := &core.Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1"}
	s := &testService{deployment: core.Deployment{ID: "agent-1"}, recent: []core.Run{{ID: "new", Pipeline: v1}, {ID: "old"}, {ID: "older", Pipeline: v1}}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil).Body.String()
	for _, want := range []string{"Pipeline versions", `<td class="mono">org/agents/.github/workflows/triage.yml</td><td class="mono">refs/tags/v1</td><td>2</td><td><a href="/runs/new">`, `<td>Unavailable</td><td>1</td><td><a href="/runs/old">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	s.recent = nil
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil).Body.String(); !strings.Contains(body, "No runs yet.") {
		t.Fatalf("no runs: %s", body)
	}
}
