package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agbredo/internal/core"
	gh "agbredo/internal/github"
)

type Verifier interface {
	Verify(context.Context, string) (core.Identity, error)
}
type API struct {
	Service       *core.Service
	Verifier      Verifier
	WebhookSecret string
	Logger        *slog.Logger
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs/{id}/events", a.events)
	mux.HandleFunc("GET /api/approvals/{id}", a.approval)
	mux.HandleFunc("POST /api/approvals/{id}/execute", a.execute)
	mux.HandleFunc("POST /api/github/webhook", a.webhook)
}
func (a *API) identity(w http.ResponseWriter, r *http.Request) (core.Identity, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 16384 {
		http.Error(w, "GitHub runner authentication required", 401)
		return core.Identity{}, false
	}
	identity, err := a.Verifier.Verify(r.Context(), strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		http.Error(w, "Invalid or expired GitHub runner credential", 401)
		return identity, false
	}
	return identity, true
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		http.Error(w, "Invalid JSON or request exceeds 64 KiB", 400)
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "Request must contain one JSON object", 400)
		return false
	}
	return true
}
func write(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}
func (a *API) failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrInvalid):
		http.Error(w, core.Redact(err.Error()), http.StatusBadRequest)
	case errors.Is(err, core.ErrUnauthorized):
		http.Error(w, "Runner is not authorized for this run or action", 403)
	case errors.Is(err, core.ErrNotFound):
		http.Error(w, "Record not found", 404)
	case errors.Is(err, core.ErrConflict):
		http.Error(w, core.Redact(err.Error()), 409)
	default:
		a.Logger.Error("runner request failed", "error", core.Redact(err.Error()))
		http.Error(w, "Request failed; inspect Agbredo run history and server logs", 500)
	}
}
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identity(w, r)
	if !ok {
		return
	}
	var e core.Event
	if !decode(w, r, &e) {
		return
	}
	accepted, err := a.Service.Ingest(r.Context(), r.PathValue("id"), e, identity)
	if err != nil {
		a.failure(w, err)
		return
	}
	write(w, map[string]bool{"accepted": accepted})
}
func (a *API) approval(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identity(w, r)
	if !ok {
		return
	}
	approval, err := a.Service.RunnerApproval(r.Context(), r.PathValue("id"), identity)
	if err != nil {
		a.failure(w, err)
		return
	}
	write(w, approval)
}
func (a *API) execute(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identity(w, r)
	if !ok {
		return
	}
	var request struct {
		RunID         string `json:"run_id"`
		CorrelationID string `json:"correlation_id"`
		PayloadHash   string `json:"payload_hash"`
	}
	if !decode(w, r, &request) {
		return
	}
	approval, err := a.Service.Execute(r.Context(), r.PathValue("id"), request.RunID, request.CorrelationID, request.PayloadHash, identity)
	if err != nil {
		a.failure(w, err)
		return
	}
	write(w, approval)
}
func (a *API) webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Webhook exceeds 1 MiB", 413)
		return
	}
	if !gh.VerifyWebhook(a.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "Invalid GitHub webhook signature", 401)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" || len(delivery) > 160 {
		http.Error(w, "GitHub delivery ID is required", 400)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(204)
		return
	}
	if event != "workflow_run" {
		w.WriteHeader(204)
		return
	}
	var payload struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		WorkflowRun struct {
			ID           int64  `json:"id"`
			DisplayTitle string `json:"display_title"`
		} `json:"workflow_run"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.WorkflowRun.ID <= 0 {
		http.Error(w, "Invalid workflow_run payload", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err = a.Service.Webhook(ctx, delivery, payload.Repository.FullName, payload.WorkflowRun.ID, payload.WorkflowRun.DisplayTitle); err != nil {
		a.failure(w, err)
		return
	}
	w.WriteHeader(204)
}
