package webapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agbredo/internal/core"
)

type fakeVerifier struct {
	identity core.Identity
	err      error
	calls    int
	token    string
}

func (v *fakeVerifier) Verify(_ context.Context, token string) (core.Identity, error) {
	v.calls++
	v.token = token
	return v.identity, v.err
}

func testAPI(verifier *fakeVerifier) *API {
	// A nil service deliberately makes any request which bypasses an expected
	// HTTP rejection fail the test before it can perform a persistence operation.
	return &API{Verifier: verifier, WebhookSecret: "test-webhook-secret-with-32-characters", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func requestAPI(a *API, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.Register(mux)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestRunnerRoutesRejectUnauthenticatedRequestsBeforeService(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/agents/agent/otlp-token"}, {http.MethodPost, "/api/otlp-token"},
	} {
		for _, authorization := range []string{"", "Basic unrelated", "Bearer " + strings.Repeat("a", 16384)} {
			v := &fakeVerifier{}
			w := requestAPI(testAPI(v), route.method, route.path, `{}`, map[string]string{"Authorization": authorization})
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s accepted header: status %d", route.method, route.path, w.Code)
			}
			if v.calls != 0 {
				t.Fatal("malformed authentication header reached verifier")
			}
		}
		v := &fakeVerifier{err: core.ErrUnauthorized}
		w := requestAPI(testAPI(v), route.method, route.path, `{}`, map[string]string{"Authorization": "Bearer expired-token"})
		if w.Code != http.StatusUnauthorized || v.calls != 1 || v.token != "expired-token" {
			t.Fatalf("invalid token was not rejected correctly: status %d, calls %d", w.Code, v.calls)
		}
	}
}

func TestRunnerIdentityUsesVerifiedClaims(t *testing.T) {
	expected := core.Identity{Repository: "owner/repo", Workflow: "owner/repo/.github/workflows/agent.yml@refs/heads/main", Ref: "refs/heads/main", RunID: 42, RunAttempt: 1}
	v := &fakeVerifier{identity: expected}
	r := httptest.NewRequest(http.MethodPost, "/api/agents/agent/otlp-token", nil)
	r.Header.Set("Authorization", "Bearer signed-token")
	w := httptest.NewRecorder()
	got, ok := testAPI(v).identity(w, r)
	if !ok || got != expected || v.token != "signed-token" {
		t.Fatalf("verified identity mismatch: got %+v, accepted %v", got, ok)
	}
}

func TestAuthenticatedRunnerRejectsMalformedAndOversizedJSON(t *testing.T) {
	for _, path := range []string{"/api/agents/agent/otlp-token", "/api/otlp-token"} {
		for _, body := range []string{``, `{`, `[]`, `{"unknown_field":true}`, `{} {}`, `{} trailing`, `{"id":"` + strings.Repeat("a", 65536) + `"}`} {
			v := &fakeVerifier{identity: core.Identity{Repository: "owner/repo", RunID: 1}}
			w := requestAPI(testAPI(v), http.MethodPost, path, body, map[string]string{"Authorization": "Bearer valid-token", "Content-Type": "application/json"})
			if w.Code != http.StatusBadRequest || v.calls != 1 {
				t.Fatalf("%s accepted invalid JSON: status %d, verification calls %d", path, w.Code, v.calls)
			}
		}
	}
}

func TestDecodeRequiresOneKnownJSONObject(t *testing.T) {
	var event core.Event
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"id":"stable-event","agent_version":3,"metadata":{"backend":"github"}}`))
	w := httptest.NewRecorder()
	if !decode(w, r, &event) || event.ID != "stable-event" || event.DeploymentRevision != 3 || string(event.Metadata) != `{"backend":"github"}` {
		t.Fatalf("valid extensible event not decoded: %+v", event)
	}
	for _, body := range []string{`{"agent_version":0.1}`, `{"agent_version":9223372036854775808}`, `{"timestamp":"invalid"}`, `{"proposal":{"unrecognized":true}}`, `{} null`} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		w := httptest.NewRecorder()
		if decode(w, r, &core.Event{}) || w.Code != http.StatusBadRequest {
			t.Fatalf("invalid typed payload was accepted: %s", body)
		}
	}
}

func signWebhook(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignatureAndPing(t *testing.T) {
	a := testAPI(&fakeVerifier{})
	body := `{"zen":"Keep it logically awesome."}`
	for _, signature := range []string{"", "sha1=bad", "sha256=" + strings.Repeat("0", 64), signWebhook("another-secret", body), signWebhook(a.WebhookSecret, body+" ")} {
		w := requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signature, "X-GitHub-Event": "ping", "X-GitHub-Delivery": "delivery-1"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("bad webhook signature accepted: status %d", w.Code)
		}
	}
	w := requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signWebhook(a.WebhookSecret, body), "X-GitHub-Event": "ping", "X-GitHub-Delivery": "delivery-1"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("genuinely signed ping rejected: status %d: %s", w.Code, w.Body.String())
	}
}

func TestWebhookDeliveryValidationAndLimits(t *testing.T) {
	a := testAPI(&fakeVerifier{})
	body := `{}`
	for _, delivery := range []string{"", strings.Repeat("d", 161)} {
		w := requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signWebhook(a.WebhookSecret, body), "X-GitHub-Event": "ping", "X-GitHub-Delivery": delivery})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid delivery ID accepted: status %d", w.Code)
		}
	}
	body = strings.Repeat("a", (1<<20)+1)
	w := requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signWebhook(a.WebhookSecret, body), "X-GitHub-Event": "ping", "X-GitHub-Delivery": "delivery-1"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook status = %d", w.Code)
	}
	for _, body := range []string{`not-json`, `{}`, `{"workflow_run":{"id":0}}`} {
		w := requestAPI(a, http.MethodPost, "/api/github/webhook", body, map[string]string{"X-Hub-Signature-256": signWebhook(a.WebhookSecret, body), "X-GitHub-Event": "workflow_run", "X-GitHub-Delivery": "delivery-1"})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid workflow payload accepted: status %d", w.Code)
		}
	}
}

// The former report route is gone; usage arrives as telemetry only.
func TestReportRouteIsGone(t *testing.T) {
	v := &fakeVerifier{}
	w := requestAPI(testAPI(v), http.MethodPost, "/api/agents/agent/observations", `{}`, map[string]string{"Authorization": "Bearer signed-token"})
	if w.Code != http.StatusGone || v.calls != 0 || !strings.Contains(w.Body.String(), "claude-code-otlp") {
		t.Fatalf("status %d, verifier calls %d, body %q", w.Code, v.calls, w.Body.String())
	}
}

// The agent run keeps its old wire name ("source") on purpose: deployed workflows send it.
func TestTokenRequestKeepsSourceWireName(t *testing.T) {
	var request tokenRequest
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"source":"review"}`))
	if !decode(httptest.NewRecorder(), r, &request) || request.AgentRun != "review" {
		t.Fatalf("decoded %+v", request)
	}
}

func TestMintTokenRequiresRunnerIdentity(t *testing.T) {
	for _, path := range []string{"/api/agents/agent/otlp-token", "/api/otlp-token"} {
		v := &fakeVerifier{}
		w := requestAPI(testAPI(v), http.MethodPost, path, `{"source":"review"}`, nil)
		if w.Code != http.StatusUnauthorized || v.calls != 0 {
			t.Fatalf("%s: status %d, verifier calls %d", path, w.Code, v.calls)
		}
		v = &fakeVerifier{err: core.ErrUnauthorized}
		w = requestAPI(testAPI(v), http.MethodPost, path, `{"source":"review"}`, map[string]string{"Authorization": "Bearer expired"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expired identity accepted: %d", path, w.Code)
		}
	}
}

// A repository without the App is refused with 403, and says why.
func TestNotInstalledIsForbidden(t *testing.T) {
	w := httptest.NewRecorder()
	testAPI(&fakeVerifier{}).failure(w, core.ErrNotInstalled)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not installed") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// A GitHub failure is the server's problem, not a refusal.
	w = httptest.NewRecorder()
	testAPI(&fakeVerifier{}).failure(w, errors.New("find GitHub App installation: 502"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("GitHub failure answered %d", w.Code)
	}
}

func TestOTLPRoutesCheckTokenAndFormatBeforeService(t *testing.T) {
	json := map[string]string{"Authorization": "Bearer agb_x", "Content-Type": "application/json"}
	for _, path := range []string{"/v1/metrics", "/v1/logs", "/v1/traces"} {
		for name, c := range map[string]struct {
			headers map[string]string
			body    string
			want    int
		}{
			"no token":         {map[string]string{"Content-Type": "application/json"}, `{}`, 401},
			"oidc token":       {map[string]string{"Authorization": "Bearer eyJhbGciOi", "Content-Type": "application/json"}, `{}`, 401},
			"overlong token":   {map[string]string{"Authorization": "Bearer agb_" + strings.Repeat("a", 100), "Content-Type": "application/json"}, `{}`, 401},
			"protobuf":         {map[string]string{"Authorization": "Bearer agb_x", "Content-Type": "application/x-protobuf"}, `{}`, 415},
			"bad gzip":         {map[string]string{"Authorization": "Bearer agb_x", "Content-Type": "application/json", "Content-Encoding": "gzip"}, `not gzip`, 400},
			"declared too big": {json, strings.Repeat("a", maxOTLPBody+1), 413},
		} {
			// A nil service would panic if the request got past these checks.
			w := requestAPI(testAPI(&fakeVerifier{}), http.MethodPost, path, c.body, c.headers)
			if w.Code != c.want {
				t.Errorf("%s %s: status %d, want %d", path, name, w.Code, c.want)
			}
		}
	}
}

func TestTelemetryLabelsComeFromTheRun(t *testing.T) {
	run := core.Run{ID: "run1", GitHubRunID: 777, RunAttempt: 2, GitHubURL: "https://github.com/o/r/actions/runs/777", Actor: "starter",
		Deployment: core.Deployment{ID: "agent", Name: "Reviewer", Owner: "o", Repository: "r", Workflow: "review.yml"}}
	got := labels(core.TelemetryOrigin{Run: run, AgentRun: "review"})
	want := map[string]string{"agbredo.agent.id": "agent", "agbredo.agent.name": "Reviewer", "agbredo.run.id": "run1", "agbredo.source": "review",
		"github.repository": "o/r", "github.workflow": "review.yml", "github.run_id": "777", "github.run_attempt": "2",
		"github.run_url": "https://github.com/o/r/actions/runs/777", "github.actor": "starter"}
	if len(got) != len(want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
