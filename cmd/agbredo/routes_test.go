package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agbredo/internal/auth"
	"agbredo/internal/webapi"
)

// The former report route answers 410 on the composed server. Without its own
// handler, the dashboard's catch-all would answer 401 and hide the reason.
func TestReportRouteIsGoneOnTheServer(t *testing.T) {
	login, err := auth.New(auth.Config{BaseURL: "https://agbredo.example", ClientID: "test", ClientSecret: "test", SessionKey: "routes-test-only-session-key-32-characters", Admins: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Nil service and verifier: the route must answer before either is used.
	api := &webapi.API{WebhookSecret: strings.Repeat("s", 32), Logger: logger}
	mux := newMux(&registrationService{}, login, api, func(context.Context) error { return nil })
	r := httptest.NewRequest(http.MethodPost, "/api/agents/agent/observations", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer signed-token")
	w := httptest.NewRecorder()
	security(mux, logger).ServeHTTP(w, r)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "claude-code-otlp") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}
