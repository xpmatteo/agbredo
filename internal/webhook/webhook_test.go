package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agbredo/internal/store"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/workflow_run.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParse(t *testing.T) {
	body := fixture(t)
	got, err := Parse("delivery-1", body)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Event{
		DeliveryID: "delivery-1", Repository: "xpmatteo/rpg-interpreter", RunID: 38696780798, RunAttempt: 2,
		Action: "completed", Status: "completed", Conclusion: "success", WorkflowName: "Agent instructions audit",
		HTMLURL:    "https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798",
		HeadBranch: "main", EventName: "schedule", Actor: "xpmatteo",
		RunStartedAt: time.Date(2026, 10, 15, 7, 8, 40, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 10, 15, 7, 9, 17, 0, time.UTC),
		Pipelines: []store.Pipeline{
			{ID: "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml", Version: "refs/tags/v1"},
			{ID: "xpmatteo/agbredo-pipelines/.github/workflows/open-pr.yml", Version: "89abcdef0123456789abcdef0123456789abcdef"},
		},
		Payload: body,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseRejectsBadJSON(t *testing.T) {
	if _, err := Parse("d", []byte("not json")); err == nil {
		t.Fatal("accepted")
	}
}

type fakeStore struct {
	events []store.Event
	err    error
}

func (f *fakeStore) InsertEvent(_ context.Context, e store.Event) error {
	f.events = append(f.events, e)
	return f.err
}

const secret = "a-webhook-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func post(h http.Handler, event, signature string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", "delivery-1")
	r.Header.Set("X-Hub-Signature-256", signature)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerStoresAPipelineRun(t *testing.T) {
	s := &fakeStore{}
	body := fixture(t)
	w := post(&Handler{Secret: []byte(secret), Store: s}, "workflow_run", sign(body), body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(s.events) != 1 || s.events[0].DeliveryID != "delivery-1" || s.events[0].RunID != 38696780798 {
		t.Fatalf("stored %+v", s.events)
	}
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	s := &fakeStore{}
	body := fixture(t)
	h := &Handler{Secret: []byte(secret), Store: s}
	for name, signature := range map[string]string{
		"missing":        "",
		"wrong":          "sha256=" + strings.Repeat("0", 64),
		"other body":     sign([]byte("{}")),
		"without prefix": strings.TrimPrefix(sign(body), "sha256="),
	} {
		if w := post(h, "workflow_run", signature, body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if len(s.events) != 0 {
		t.Fatalf("stored %+v", s.events)
	}
}

func TestHandlerIgnoresOtherEvents(t *testing.T) {
	s := &fakeStore{}
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)
	if w := post(&Handler{Secret: []byte(secret), Store: s}, "ping", sign(body), body); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if len(s.events) != 0 {
		t.Fatalf("stored %+v", s.events)
	}
}

func TestHandlerIgnoresRunsThatAreNotPipelineRuns(t *testing.T) {
	s := &fakeStore{}
	body := fixture(t)
	start := bytes.Index(body, []byte(`"referenced_workflows": [`))
	end := bytes.Index(body, []byte(`"run_started_at"`))
	body = append(append(append([]byte{}, body[:start]...), `"referenced_workflows": [],
    `...), body[end:]...)
	if w := post(&Handler{Secret: []byte(secret), Store: s}, "workflow_run", sign(body), body); w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(s.events) != 0 {
		t.Fatalf("stored %+v", s.events)
	}
}

func TestHandlerRejectsBadPayload(t *testing.T) {
	body := []byte("not json")
	if w := post(&Handler{Secret: []byte(secret), Store: &fakeStore{}}, "workflow_run", sign(body), body); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}

func TestHandlerReportsStoreErrors(t *testing.T) {
	body := fixture(t)
	s := &fakeStore{err: errors.New("db down")}
	if w := post(&Handler{Secret: []byte(secret), Store: s}, "workflow_run", sign(body), body); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
}
