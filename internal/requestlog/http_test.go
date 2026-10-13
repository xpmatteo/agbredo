package requestlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectedFormHasCorrelatedSafeStructuredLogs(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	previous := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(previous)
	const id = "5a0cf3d8-a8e7-42e5-9aa0-4204d945529f"
	const private = "private-verification-value"
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ID(r) != id {
			t.Error("request ID not attached")
		}
		RejectForm(w, r, "origin_unavailable")
	}), logger)
	r := httptest.NewRequest(http.MethodPost, "/agents?token="+private, strings.NewReader("csrf="+private))
	r.Header.Set("X-Request-ID", id)
	r.Header.Set("Authorization", "Bearer "+private)
	r.Header.Set("Cookie", "session="+private)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || w.Header().Get("X-Request-ID") != id || !strings.Contains(w.Body.String(), "Request ID: "+id) || !strings.Contains(w.Body.String(), "page origin") {
		t.Fatal("failure lacks cause or correlation", w.Code, w.Body.String())
	}
	if strings.Contains(output.String(), private) || strings.Contains(w.Body.String(), private) {
		t.Fatal("private request data exposed")
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected rejection and completion logs: %s", output.String())
	}
	var rejected, completed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rejected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &completed); err != nil {
		t.Fatal(err)
	}
	if rejected["request_id"] != id || rejected["reason"] != "origin_unavailable" || completed["request_id"] != id || completed["status"] != float64(403) || completed["duration_ms"] == nil || completed["path"] != "/agents" {
		t.Fatalf("missing diagnostic fields: %s", output.String())
	}
}

func TestUntrustedRequestIDCannotBeReflectedIntoLogs(t *testing.T) {
	var output bytes.Buffer
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), slog.New(slog.NewJSONHandler(&output, nil)))
	r := httptest.NewRequest(http.MethodGet, "/agents", nil)
	r.Header.Set("X-Request-ID", "Bearer untrusted-private-data")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if len(w.Header().Get("X-Request-ID")) != 32 || strings.Contains(output.String(), "untrusted-private-data") {
		t.Fatal("untrusted request ID was logged")
	}
}

func TestRequestCompletionRecordsActualStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusSeeOther, http.StatusInternalServerError} {
		var output bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusOK {
				_, _ = w.Write([]byte("response"))
				return
			}
			w.WriteHeader(status)
		}), slog.New(slog.NewJSONHandler(&output, nil)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents", nil))
		var result map[string]any
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result["status"] != float64(status) {
			t.Fatalf("wrong status: %s", output.String())
		}
	}
}
