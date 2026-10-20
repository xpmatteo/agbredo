package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agbredo/internal/store"
)

type fakeStore struct {
	events  []store.Event // events of run 38696780798/2, in order
	err     error
	pingErr error
}

func (f *fakeStore) LatestRuns(context.Context) ([]store.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.events[len(f.events)-1:], nil
}

func (f *fakeStore) RunEvents(_ context.Context, runID int64, attempt int) ([]store.Event, error) {
	if f.err != nil || runID != 38696780798 || attempt != 2 {
		return nil, f.err
	}
	return f.events, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

var t0 = time.Date(2026, 10, 15, 7, 8, 40, 0, time.UTC)

func sample() *fakeStore {
	e := store.Event{
		Repository: "xpmatteo/rpg-interpreter", RunID: 38696780798, RunAttempt: 2,
		WorkflowName: "Agent <b>audit</b>", HTMLURL: "https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798",
		HeadBranch: "main", EventName: "schedule", Actor: "xpmatteo", RunStartedAt: t0, ReceivedAt: t0,
		Pipelines: []store.Pipeline{
			{ID: "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml", Version: "refs/tags/v1"},
			{ID: "xpmatteo/agbredo-pipelines/.github/workflows/open-pr.yml", Version: "89abcdef"},
		},
	}
	requested, progress, completed := e, e, e
	requested.Action, requested.Status, requested.UpdatedAt = "requested", "queued", t0
	progress.Action, progress.Status, progress.UpdatedAt = "in_progress", "in_progress", t0.Add(10*time.Second)
	completed.Action, completed.Status, completed.Conclusion, completed.UpdatedAt = "completed", "completed", "failure", t0.Add(time.Minute)
	return &fakeStore{events: []store.Event{requested, progress, completed}}
}

func serve(s Store, method, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	Register(mux, s, func(h http.Handler) http.Handler { return h })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func checkContains(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("missing %q in\n%s", s, body)
		}
	}
}

func TestRunsPage(t *testing.T) {
	w := serve(sample(), "GET", "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	checkContains(t, w.Body.String(),
		"xpmatteo/rpg-interpreter",
		"xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@refs/tags/v1",
		"xpmatteo/agbredo-pipelines/.github/workflows/open-pr.yml@89abcdef",
		`href="/runs/38696780798/2"`,
		"failure",
		"2026-10-15 07:08:40",
		"2026-10-15 07:09:40",
		`href="https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798"`,
		`action="/auth/logout"`,
	)
}

func TestRunPage(t *testing.T) {
	w := serve(sample(), "GET", "/runs/38696780798/2")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	checkContains(t, body,
		"Agent &lt;b&gt;audit&lt;/b&gt;",
		"xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@refs/tags/v1",
		"main", "schedule",
		`href="https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798"`,
	)
	r, p, c := strings.Index(body, "requested"), strings.Index(body, "in_progress"), strings.Index(body, "completed")
	if r < 0 || r > p || p > c {
		t.Errorf("events not in order: %d %d %d", r, p, c)
	}
}

func TestRunPageNotFound(t *testing.T) {
	for _, target := range []string{"/runs/1/1", "/runs/x/1", "/runs/38696780798/x"} {
		if w := serve(sample(), "GET", target); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", target, w.Code)
		}
	}
}

func TestStoreErrors(t *testing.T) {
	s := sample()
	s.err = errors.New("db down")
	for _, target := range []string{"/", "/runs/38696780798/2"} {
		if w := serve(s, "GET", target); w.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d", target, w.Code)
		}
	}
}

func TestReadyz(t *testing.T) {
	s := sample()
	if w := serve(s, "GET", "/readyz"); w.Code != http.StatusOK {
		t.Errorf("status %d", w.Code)
	}
	s.pingErr = errors.New("db down")
	if w := serve(s, "GET", "/readyz"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d", w.Code)
	}
}

func TestPagesNeedLoginButReadyzDoesNot(t *testing.T) {
	mux := http.NewServeMux()
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	}
	Register(mux, sample(), deny)
	for target, want := range map[string]int{"/": http.StatusTeapot, "/runs/38696780798/2": http.StatusTeapot, "/readyz": http.StatusOK} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != want {
			t.Errorf("%s: status %d, want %d", target, w.Code, want)
		}
	}
}
