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
	return readFixture(t, "workflow_run.json")
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
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
	opened []store.Opened
	err    error
}

func (f *fakeStore) InsertEvent(_ context.Context, e store.Event) error {
	f.events = append(f.events, e)
	return f.err
}

func (f *fakeStore) InsertOpened(_ context.Context, o store.Opened) error {
	f.opened = append(f.opened, o)
	return f.err
}

const secret = "a-webhook-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func post(h http.Handler, event, signature string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/github/webhook", bytes.NewReader(body))
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

func TestParseOpened(t *testing.T) {
	pr, ok, err := ParseOpened("d1", "pull_request", readFixture(t, "pull_request.json"))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	want := store.Opened{
		DeliveryID: "d1", Repository: "xpmatteo/agbredo", Kind: "pull_request", Number: 12,
		Title: "Add a haiku about robots at work", HTMLURL: "https://github.com/xpmatteo/agbredo/pull/12",
		OpenedAt: time.Date(2026, 10, 21, 6, 50, 12, 0, time.UTC), RunID: 18765432101, RunAttempt: 1,
	}
	if pr != want {
		t.Fatalf("got  %+v\nwant %+v", pr, want)
	}
	issue, ok, err := ParseOpened("d2", "issues", readFixture(t, "issues.json"))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if issue.Kind != "issue" || issue.Number != 11 || issue.HTMLURL != "https://github.com/xpmatteo/agbredo/issues/11" ||
		issue.RunID != 18765432101 || issue.RunAttempt != 1 {
		t.Fatalf("got %+v", issue)
	}
}

func TestParseOpenedNeedsOpenedAndTheMarkerOnTheLastLine(t *testing.T) {
	body := string(readFixture(t, "issues.json"))
	for name, b := range map[string]string{
		"edited":          strings.Replace(body, `"action": "opened"`, `"action": "edited"`, 1),
		"no marker":       strings.Replace(body, `Agbredo-Run: 18765432101/1`, `Thanks`, 1),
		"marker not last": strings.Replace(body, `Agbredo-Run: 18765432101/1\n`, `Agbredo-Run: 18765432101/1\nThanks`, 1),
		"no attempt":      strings.Replace(body, `18765432101/1`, `18765432101`, 1),
		"text after":      strings.Replace(body, `18765432101/1`, `18765432101/1 please`, 1),
		"not a number":    strings.Replace(body, `18765432101/1`, `abc/1`, 1),
		"empty body":      strings.Replace(body, `"body": "Please`, `"nobody": "Please`, 1),
	} {
		if o, ok, err := ParseOpened("d", "issues", []byte(b)); err != nil || ok {
			t.Errorf("%s: %v %+v %v", name, ok, o, err)
		}
	}
}

func TestHandlerStoresOpenedPRsAndIssues(t *testing.T) {
	s := &fakeStore{}
	h := &Handler{Secret: []byte(secret), Store: s}
	for event, file := range map[string]string{"pull_request": "pull_request.json", "issues": "issues.json"} {
		body := readFixture(t, file)
		if w := post(h, event, sign(body), body); w.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d: %s", event, w.Code, w.Body)
		}
	}
	if len(s.opened) != 2 || len(s.events) != 0 {
		t.Fatalf("stored %+v %+v", s.opened, s.events)
	}
}

func TestHandlerIgnoresPRsWithoutMarker(t *testing.T) {
	s := &fakeStore{}
	body := bytes.Replace(readFixture(t, "pull_request.json"), []byte("Agbredo-Run"), []byte("Not-Agbredo"), 1)
	if w := post(&Handler{Secret: []byte(secret), Store: s}, "pull_request", sign(body), body); w.Code != http.StatusNoContent {
		t.Fatalf("status %d", w.Code)
	}
	if len(s.opened) != 0 {
		t.Fatalf("stored %+v", s.opened)
	}
}

func TestHandlerOpenedErrors(t *testing.T) {
	bad := []byte("not json")
	if w := post(&Handler{Secret: []byte(secret), Store: &fakeStore{}}, "issues", sign(bad), bad); w.Code != http.StatusBadRequest {
		t.Errorf("bad payload: status %d", w.Code)
	}
	body := readFixture(t, "issues.json")
	s := &fakeStore{err: errors.New("db down")}
	if w := post(&Handler{Secret: []byte(secret), Store: s}, "issues", sign(body), body); w.Code != http.StatusInternalServerError {
		t.Errorf("db down: status %d", w.Code)
	}
}
