package web

import (
	"context"
	"fmt"
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
	deployment core.Deployment
	run        core.Run
	recent     []core.Run
	events     []core.Event
	saved      core.Deployment
	saveErr    error
	// prs are the run's linked pull requests (Fleet and run page), or the
	// repository's unlinked ones (deployment page).
	prs   []core.PullRequest
	since time.Time
}

func (s *testService) Fleet(context.Context) (core.Fleet, error) {
	f := core.Fleet{Deployments: []core.Deployment{s.deployment}, Runs: []core.Run{s.run}, PullRequestsSince: s.since}
	if len(s.prs) > 0 {
		f.PullRequests = map[string][]core.PullRequest{s.run.ID: s.prs}
	}
	return f, nil
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
func (s *testService) RunPullRequests(context.Context, string) ([]core.PullRequest, error) {
	return s.prs, nil
}
func (s *testService) UnlinkedPullRequests(context.Context, string, string, int) ([]core.PullRequest, error) {
	return s.prs, nil
}
func (s *testService) PullRequestsSince(context.Context) (time.Time, error) { return s.since, nil }

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
		{"head branch is not a ref", core.Run{ID: "run-1", HeadBranch: "v1.2"}, `<dt>Ref</dt><dd class="mono">Unavailable</dd><dt>Head branch</dt><dd class="mono">v1.2</dd>`},
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

// A run known only from GitHub shows what GitHub said, and that no
// telemetry came: its cost and tokens are missing, not zero.
func TestRunPageOfARunWithoutTelemetry(t *testing.T) {
	started := time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)
	run := core.Run{ID: "run-1", Status: "completed", HeadBranch: "main", HeadSHA: "4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a", GitHubStartedAt: started}
	body := serve(&testService{run: run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	for _, want := range []string{"No telemetry received", `<dt>Head branch</dt><dd class="mono">main</dd>`, `<dd class="mono">4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a</dd>`,
		"<dd>2026-10-15 07:08:00 UTC / Unavailable</dd>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	if !strings.Contains(body, "<span>Estimated API-equivalent consumption</span><strong>Unavailable</strong>") {
		t.Fatal("missing cost not shown as missing", body)
	}
	runs := serve(&testService{run: run}, testAuth{"admin", true}, http.MethodGet, "/runs", nil).Body.String()
	if !strings.Contains(runs, "<td>Unavailable<small>") {
		t.Fatal("runs list shows a missing cost as zero", runs)
	}
}

// A run waiting for a maintainer's approval is labelled, and is not counted as active.
func TestRunWaitingForApproval(t *testing.T) {
	s := &testService{run: core.Run{ID: "run-1", Status: "queued", AwaitingApproval: true}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, "<span>Active runs</span><strong>0</strong>") || !strings.Contains(body, "Waiting for approval") {
		t.Fatal(body)
	}
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(); !strings.Contains(body, "Waiting for approval on GitHub") {
		t.Fatal(body)
	}
	s.run.AwaitingApproval = false
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/", nil).Body.String(); !strings.Contains(body, "<span>Active runs</span><strong>1</strong>") || strings.Contains(body, "Waiting for approval") {
		t.Fatal(body)
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
	if !strings.Contains(w.Body.String(), "$0.00") || !strings.Contains(w.Body.String(), "<strong>0</strong>") {
		t.Fatal("measured zero lost", w.Body.String())
	}
}

func TestPagesEscapeUntrustedContent(t *testing.T) {
	s := &testService{deployment: core.Deployment{ID: "agent", Name: "Example", Revision: 1}, run: core.Run{ID: "run", Deployment: core.Deployment{Name: "Example"}}, events: []core.Event{{Message: "<script>secret()</script>"}},
		prs: []core.PullRequest{{Number: 1, Title: "<script>secret()</script>", Author: "<script>", Link: "<script>", Reason: "<script>", Findings: []string{"<script>"}, Trailer: "<script>"}}}
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
	if !strings.Contains(review, "2.1.296") || !strings.Contains(review, "$0.00") || !strings.Contains(review, "1200") || !strings.Contains(review, "<td>Unavailable / Unavailable</td>") {
		t.Fatal("measured values wrong:", review)
	}
	if strings.Contains(fix, "$") || strings.Contains(fix, ">0<") || strings.Count(fix, "Unavailable") < 5 {
		t.Fatal("missing values shown as zero:", fix)
	}
	if strings.Contains(body, "No telemetry received") {
		t.Fatal("agent runs said to be missing")
	}
	s.run.AgentRuns = nil
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(); strings.Contains(body, "<th>Agent run</th>") || !strings.Contains(body, "No telemetry received") {
		t.Fatal("empty table shown", body)
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

// The run page shows the trigger event, and the models and effort levels of
// each agent run. Older runs without them show them as missing.
func TestRunPageShowsConfiguration(t *testing.T) {
	in, out := int64(1200), int64(80)
	s := &testService{run: core.Run{ID: "run-1", TriggerEvent: "schedule", AgentRuns: map[string]*core.AgentRunUsage{
		"review": {Models: map[string]*core.ClaudeUsage{"claude-sonnet-4-5": {InputTokens: &in, OutputTokens: &out}, "claude-haiku-4-5": {}}, Efforts: []string{"low", "high"}},
		"old":    {},
	}}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	if !strings.Contains(body, `<dt>Trigger event</dt><dd class="mono">schedule</dd>`) || !strings.Contains(body, "<th>Models</th><th>Effort</th>") {
		t.Fatalf("configuration missing: %s", body)
	}
	review := body[strings.Index(body, ">review<"):]
	review = review[:strings.Index(review, "</tr>")]
	for _, want := range []string{
		`<span class="mono">claude-haiku-4-5</span><small>Input / output: Unavailable / Unavailable`,
		`<span class="mono">claude-sonnet-4-5</span><small>Input / output: 1200 / 80<br>Cache read / creation: Unavailable / Unavailable</small>`,
		`<td>low, high</td>`,
	} {
		if !strings.Contains(review, want) {
			t.Fatalf("want %s in %s", want, review)
		}
	}
	old := body[strings.Index(body, ">old<"):]
	old = old[:strings.Index(old, "</tr>")]
	if !strings.Contains(old, "<td>Unavailable</td><td>Unavailable</td><td>Unavailable</td>") {
		t.Fatalf("older agent run: %s", old)
	}
	s.run.TriggerEvent = ""
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(); !strings.Contains(body, `<dt>Trigger event</dt><dd class="mono">Unavailable</dd>`) {
		t.Fatalf("older run: %s", body)
	}
}

func TestMoneyShowsTwoDigits(t *testing.T) {
	for micros, want := range map[int64]string{0: "$0.00", 1: "$0.00", 4_999: "$0.00", 5_000: "$0.01", 12_345_678: "$12.35", 5_000_000: "$5.00", 999_995_000: "$1000.00"} {
		if got := money(micros); got != want {
			t.Errorf("money(%d) = %s, want %s", micros, got, want)
		}
	}
}

// The run page lists each job with its steps in GitHub's order, numbers as
// given, and missing times as missing.
func TestRunPageShowsJobsAndSteps(t *testing.T) {
	started := time.Date(2026, 10, 15, 7, 8, 2, 0, time.UTC)
	run := core.Run{ID: "run-1", Status: "completed", Jobs: []core.Job{{ID: 501, Name: "audit / audit", Status: "completed", Conclusion: "success",
		StartedAt: started, RunnerName: "GitHub Actions 7", URL: "https://github.com/acme/agent/actions/runs/777/job/501", Steps: []core.Step{
			{Number: 2, Name: "Gate", Status: "completed", Conclusion: "success", StartedAt: started, CompletedAt: started.Add(2 * time.Second)},
			{Number: 3, Name: "Audit with Claude", Status: "completed", Conclusion: "skipped"},
			{Number: 28, Name: "Post <Gate>", Status: "in_progress"}}},
		{ID: 502, Name: "report", Status: "completed", Conclusion: "skipped"}}}
	body := serve(&testService{run: run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	for _, want := range []string{
		"<h2>Jobs and steps</h2>",
		`<a href="https://github.com/acme/agent/actions/runs/777/job/501" target="_blank" rel="noopener noreferrer">audit / audit ↗</a></strong><span class="badge">success</span>`,
		"Start / end: 2026-10-15 07:08:02 UTC / Unavailable",
		`<tr><td>2</td><td>Gate</td><td><span class="badge">success</span></td><td>2026-10-15 07:08:02 UTC</td><td>2026-10-15 07:08:04 UTC</td></tr>`,
		`<tr><td>3</td><td>Audit with Claude</td><td><span class="badge">skipped</span></td><td>Unavailable</td><td>Unavailable</td></tr>`,
		`<tr><td>28</td><td>Post &lt;Gate&gt;</td><td><span class="badge">in_progress</span>`,
		`<strong>report</strong><span class="badge">skipped</span>`,
		"GitHub listed no steps for this job.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	if strings.Contains(body, "Agbredo keeps at most") {
		t.Fatal("says jobs were cut")
	}
	run.Jobs = make([]core.Job, core.MaxJobs)
	if body = serve(&testService{run: run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(); !strings.Contains(body, "Agbredo keeps at most 50 jobs per run and 100 steps per job.") {
		t.Fatal("does not say jobs may be cut", body)
	}
}

// No jobs, unknown jobs and a run still going each say so.
func TestRunPageWithoutJobs(t *testing.T) {
	for _, tc := range []struct {
		run  core.Run
		want string
	}{
		{core.Run{ID: "run-1", Status: "completed", Jobs: []core.Job{}}, "GitHub listed no jobs for this run attempt."},
		{core.Run{ID: "run-1", Status: "failed"}, "<h2>Jobs and steps</h2><div class=\"panel\">Unavailable."},
		{core.Run{ID: "run-1", Status: "running"}, "Recorded when the run finishes."},
	} {
		body := serve(&testService{run: tc.run}, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
		if !strings.Contains(body, tc.want) {
			t.Fatalf("want %s in %s", tc.want, body)
		}
	}
}

// openedPR is pull request 13 of acme/agent, linked to run-1 at the given level.
func openedPR(link string) core.PullRequest {
	p := core.PullRequest{ID: "42:13", Owner: "acme", Repository: "agent", Number: 13, Title: "Audit <fixes>", URL: "https://github.com/acme/agent/pull/13",
		Author: "github-actions[bot]", AuthorType: "Bot", CreatedAt: time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC), Link: link, RunID: "run-1"}
	switch link {
	case core.LinkDeclared:
		p.Trailer = "Agbredo-Run: 777/1"
	case core.LinkInferred:
		p.Findings = []string{core.FindingMissingTrailer}
	}
	return p
}

// The runs table says whether each run opened a pull request. "No PR" only
// when the run has finished and Agbredo recorded pull requests for the whole
// of it; otherwise the outcome is unavailable.
func TestRunsTableShowsTheOutcome(t *testing.T) {
	started := time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)
	before, after := started.Add(-time.Hour), started.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		status string
		since  time.Time
		prs    []core.PullRequest
		want   string
		absent string
	}{
		{"declared", "completed", before, []core.PullRequest{openedPR(core.LinkDeclared)}, `<a href="https://github.com/acme/agent/pull/13" target="_blank" rel="noopener noreferrer">Opened PR #13</a></div>`, "inferred"},
		{"inferred", "running", before, []core.PullRequest{openedPR(core.LinkInferred)}, `Opened PR #13</a><small>inferred</small>`, "No PR"},
		{"finished, no PR", "completed", before, nil, "<td>No PR</td>", "Opened PR"},
		{"failed, no PR", "failed", started, nil, "<td>No PR</td>", "Opened PR"},
		{"running", "running", before, nil, "<td>Unavailable</td>", "No PR"},
		{"started before Agbredo recorded pull requests", "completed", after, nil, "<td>Unavailable</td>", "No PR"},
		{"recording time unknown", "completed", time.Time{}, nil, "<td>Unavailable</td>", "No PR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &testService{run: core.Run{ID: "run-1", Status: tc.status, GitHubStartedAt: started}, since: tc.since, prs: tc.prs}
			for _, path := range []string{"/", "/runs"} {
				body := serve(s, testAuth{"admin", true}, http.MethodGet, path, nil).Body.String()
				if !strings.Contains(body, "<th>Outcome</th>") || !strings.Contains(body, tc.want) || strings.Contains(body, tc.absent) {
					t.Fatalf("%s: want %s without %s in %s", path, tc.want, tc.absent, body)
				}
			}
		})
	}
	// A run without GitHub's start time counts from when Agbredo recorded it.
	s := &testService{run: core.Run{ID: "run-1", Status: "completed", CreatedAt: started}, since: before}
	if body := serve(s, testAuth{"admin", true}, http.MethodGet, "/runs", nil).Body.String(); !strings.Contains(body, "<td>No PR</td>") {
		t.Fatal(body)
	}
}

func TestRunPageShowsPullRequests(t *testing.T) {
	started := time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)
	s := &testService{run: core.Run{ID: "run-1", Status: "completed", GitHubStartedAt: started}, since: started.Add(-time.Hour),
		prs: []core.PullRequest{openedPR(core.LinkDeclared), openedPR(core.LinkInferred)}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String()
	for _, want := range []string{"<h2>Pull requests</h2>", `<a href="https://github.com/acme/agent/pull/13" target="_blank" rel="noopener noreferrer">#13 ↗</a>`,
		"Audit &lt;fixes&gt;", "github-actions[bot]<small>Bot</small>", "2026-10-15 07:08:10 UTC", `<span class="badge">declared</span><small class="mono">Agbredo-Run: 777/1</small>`,
		`<span class="badge">inferred</span>`, `<span class="mono">missing_trailer</span>`, "<td>None</td>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	if strings.Contains(body, "merged") || strings.Contains(body, "closed") {
		t.Fatal("shows the later state of a pull request", body)
	}
	for _, tc := range []struct {
		status string
		since  time.Time
		want   string
	}{
		{"completed", started.Add(-time.Hour), "No pull request. GitHub reported none that Agbredo could link to this run."},
		{"running", started.Add(-time.Hour), "None linked yet."},
		{"completed", started.Add(time.Hour), "Unavailable. This run started before Agbredo began recording pull requests."},
	} {
		s := &testService{run: core.Run{ID: "run-1", Status: tc.status, GitHubStartedAt: started}, since: tc.since}
		if body := serve(s, testAuth{"admin", true}, http.MethodGet, "/runs/run-1", nil).Body.String(); !strings.Contains(body, tc.want) {
			t.Fatalf("want %s in %s", tc.want, body)
		}
	}
}

func TestDeploymentPageShowsUnlinkedPullRequests(t *testing.T) {
	several := openedPR(core.LinkUnlinked)
	several.RunID, several.Reason, several.CandidateRuns = "", core.ReasonSeveralRunsInWindow, []string{"run-a", "run-b"}
	several.Findings = []string{core.FindingUnlinkedBotPR}
	human := openedPR(core.LinkUnlinked)
	human.Number, human.URL, human.Author, human.AuthorType, human.RunID = 14, "", "matt", "User", ""
	human.Reason, human.Trailer, human.Findings = core.ReasonTrailerByHuman, "Agbredo-Run: 777/1", []string{core.FindingTrailerMismatch}
	s := &testService{deployment: core.Deployment{ID: "agent-1", Owner: "acme", Repository: "agent"}, prs: []core.PullRequest{several, human}}
	body := serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil).Body.String()
	for _, want := range []string{"<h2>Unlinked pull requests in this repository</h2>", "several_runs_in_window", `<a href="/runs/run-a">Candidate run</a>`,
		`<a href="/runs/run-b">Candidate run</a>`, `<span class="mono">unlinked_bot_pr</span>`, "<td>#14<small>acme/agent</small></td>", "matt<small>User</small>",
		"trailer_by_human<small>Agbredo-Run: 777/1</small>", `<span class="mono">trailer_mismatch</span>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("want %s in %s", want, body)
		}
	}
	s.prs = nil
	if body = serve(s, testAuth{"admin", true}, http.MethodGet, "/agents/agent-1", nil).Body.String(); !strings.Contains(body, `<td colspan="6">None recorded.</td>`) {
		t.Fatal(body)
	}
}
