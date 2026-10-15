package webapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agbredo/internal/core"
	"agbredo/internal/forward"
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
	// Forward sends accepted telemetry on to a store. Nil sends nothing.
	Forward *forward.Forwarder
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agents/{id}/observations", reportRouteGone)
	mux.HandleFunc("POST /api/otlp-token", a.mintTokenForWorkflow)
	mux.HandleFunc("POST /api/agents/{id}/otlp-token", a.mintToken)
	mux.HandleFunc("POST /v1/metrics", a.otlp("metrics", (*core.Service).IngestMetrics))
	mux.HandleFunc("POST /v1/logs", a.otlp("logs", (*core.Service).IngestLogs))
	mux.HandleFunc("POST /v1/traces", a.otlp("traces", (*core.Service).IngestTraces))
	mux.HandleFunc("POST /api/github/webhook", a.webhook)
}

// reportRouteGone answers workflows that still call the former report route.
// Without it the dashboard's catch-all would answer 401, which misleads.
func reportRouteGone(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "The report route was removed; use integrations/claude-code-otlp", http.StatusGone)
}

// tokenRequest is the body of POST /api/otlp-token and POST /api/agents/{id}/otlp-token.
type tokenRequest struct {
	// "source" is the wire name of the agent run; kept for deployed workflows.
	AgentRun string `json:"source"`
}

// mintToken gives an agent run a token for sending its telemetry here.
func (a *API) mintToken(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identity(w, r)
	if !ok {
		return
	}
	var request tokenRequest
	if !decode(w, r, &request) {
		return
	}
	token, expires, err := a.Service.MintToken(r.Context(), r.PathValue("id"), request.AgentRun, identity)
	if err != nil {
		a.failure(w, err)
		return
	}
	write(w, map[string]any{"token": token, "expires_at": expires, "traceparent": core.TraceParent(r.PathValue("id"), identity)})
}

// mintTokenForWorkflow is mintToken without a deployment ID in the URL. The
// deployment is found by the repository + workflow file of the verified
// identity, and created by its first valid request.
func (a *API) mintTokenForWorkflow(w http.ResponseWriter, r *http.Request) {
	identity, ok := a.identity(w, r)
	if !ok {
		return
	}
	var request tokenRequest
	if !decode(w, r, &request) {
		return
	}
	g, err := a.Service.MintTokenForWorkflow(r.Context(), request.AgentRun, identity)
	if err != nil {
		a.failure(w, err)
		return
	}
	write(w, map[string]any{"token": g.Token, "expires_at": g.Expires, "traceparent": core.TraceParent(g.DeploymentID, identity)})
}

const maxOTLPBody = 4 << 20

// otlp receives OpenTelemetry JSON from Claude Code. The token identifies the
// run and the step; nothing in the payload is trusted for that. Accepted
// telemetry is forwarded, labelled with that run and step.
func (a *API) otlp(signal string, ingest func(*core.Service, context.Context, string, []byte) (core.TelemetryOrigin, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		if token == header || !strings.HasPrefix(token, "agb_") || len(token) > 100 {
			http.Error(w, "Telemetry token required", 401)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "Send OTLP as JSON: set OTEL_EXPORTER_OTLP_PROTOCOL=http/json", 415)
			return
		}
		var reader io.Reader = http.MaxBytesReader(w, r.Body, maxOTLPBody)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(reader)
			if err != nil {
				http.Error(w, "Invalid gzip body", 400)
				return
			}
			defer zr.Close()
			reader = io.LimitReader(zr, maxOTLPBody+1)
		}
		body, err := io.ReadAll(reader)
		if err != nil || len(body) > maxOTLPBody {
			http.Error(w, "Telemetry exceeds 4 MiB", 413)
			return
		}
		origin, err := ingest(a.Service, r.Context(), token, body)
		if err != nil {
			a.failure(w, err)
			return
		}
		a.Forward.Send(signal, body, labels(origin))
		write(w, struct{}{})
	}
}

// labels say where telemetry came from. Empty values are left out.
func labels(o core.TelemetryOrigin) map[string]string {
	r := o.Run
	all := map[string]string{
		"agbredo.agent.id":        r.Deployment.ID,
		"agbredo.agent.name":      r.Deployment.Name,
		"agbredo.run.id":          r.ID,
		"agbredo.source":          o.AgentRun,
		"github.repository":       r.Deployment.Owner + "/" + r.Deployment.Repository,
		"github.workflow":         r.Deployment.Workflow,
		"github.run_id":           strconv.FormatInt(r.GitHubRunID, 10),
		"github.run_attempt":      strconv.FormatInt(r.RunAttempt, 10),
		"github.run_url":          r.GitHubURL,
		"github.actor":            r.Actor,
		"github.triggering_actor": r.TriggeringActor,
	}
	for k, v := range all {
		if v == "" {
			delete(all, k)
		}
	}
	return all
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
	case errors.Is(err, core.ErrNotInstalled):
		http.Error(w, "The Agbredo GitHub App is not installed on this repository", 403)
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
func (a *API) webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.Logger.Warn("webhook rejected", "reason", "body too large")
		http.Error(w, "Webhook exceeds 1 MiB", 413)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	if !gh.VerifyWebhook(a.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		a.Logger.Warn("webhook rejected", "reason", "invalid signature", "delivery", logValue(delivery), "event", logValue(event))
		http.Error(w, "Invalid GitHub webhook signature", 401)
		return
	}
	if delivery == "" || len(delivery) > 160 {
		a.Logger.Warn("webhook rejected", "reason", "missing or long delivery ID", "event", logValue(event))
		http.Error(w, "GitHub delivery ID is required", 400)
		return
	}
	// The headers are not signed. The event name is stored, so keep it short.
	if len(event) > 100 {
		a.Logger.Warn("webhook rejected", "reason", "long event name", "delivery", delivery)
		http.Error(w, "GitHub event name is too long", 400)
		return
	}
	// Any event may carry these. A payload without them, or not JSON at all, is still archived.
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	_ = json.Unmarshal(body, &payload)
	d := core.Delivery{ID: delivery, Event: event, Action: payload.Action, Repository: payload.Repository.FullName, InstallationID: payload.Installation.ID}
	installation := ""
	if d.InstallationID != 0 {
		installation = strconv.FormatInt(d.InstallationID, 10)
	}
	a.Forward.SendWebhook(delivery, event, body, map[string]string{"github.action": d.Action, "github.repository": d.Repository,
		"github.installation_id": installation, "github.sender": payload.Sender.Login})
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if event == "pull_request" && d.Action == "opened" {
		a.pullRequest(ctx, w, d, body)
		return
	}
	if event != "workflow_run" {
		a.Logger.Info("webhook received", "delivery", delivery, "event", logValue(event), "action", logValue(d.Action), "handled", false)
		if err = a.Service.RecordDelivery(ctx, d, core.DeliveryIgnored, nil); err != nil {
			a.Logger.Error("webhook not indexed", "delivery", delivery, "error", core.Redact(err.Error()))
			http.Error(w, "Webhook not indexed; see server logs", 500)
			return
		}
		w.WriteHeader(204)
		return
	}
	// The signed payload is trusted, but the event header is not signed: the
	// payload must have the shape of a workflow_run event.
	run, err := gh.ParseWorkflowRunEvent(body)
	if err != nil {
		a.Logger.Warn("webhook rejected", "reason", "invalid workflow_run payload", "delivery", delivery)
		if err = a.Service.RecordDelivery(ctx, d, core.DeliveryFailed, gh.ErrBadPayload); err != nil {
			a.Logger.Error("webhook not indexed", "delivery", delivery, "error", core.Redact(err.Error()))
		}
		http.Error(w, "Invalid workflow_run payload", 400)
		return
	}
	d.Repository = run.Repository // the same, checked by the parser
	a.Logger.Info("webhook received", "delivery", delivery, "event", event, "handled", true, "repository", logValue(run.Repository), "github_run_id", run.ID, "run_attempt", run.Attempt, "status", logValue(run.Status))
	if err = a.Service.Webhook(ctx, d, run); err != nil {
		a.failure(w, err)
		return
	}
	w.WriteHeader(204)
}

// pullRequest records an opened pull_request delivery. Other actions
// (edited, closed, reopened, ...) are ignored before this: Agbredo records
// only the opening of a pull request, as it was then.
func (a *API) pullRequest(ctx context.Context, w http.ResponseWriter, d core.Delivery, body []byte) {
	// The signed payload is trusted, but the event header is not signed: the
	// payload must have the shape of a pull_request event.
	pr, err := gh.ParsePullRequestEvent(body)
	if err != nil {
		a.Logger.Warn("webhook rejected", "reason", "invalid pull_request payload", "delivery", d.ID)
		if err = a.Service.RecordDelivery(ctx, d, core.DeliveryFailed, gh.ErrBadPullRequest); err != nil {
			a.Logger.Error("webhook not indexed", "delivery", d.ID, "error", core.Redact(err.Error()))
		}
		http.Error(w, "Invalid pull_request payload", 400)
		return
	}
	d.Repository = pr.Repository // the same, read from the same payload
	a.Logger.Info("webhook received", "delivery", d.ID, "event", d.Event, "action", d.Action, "handled", true, "repository", logValue(pr.Repository), "number", pr.Number)
	if err = a.Service.PullRequestWebhook(ctx, d, pr); err != nil {
		a.failure(w, err)
		return
	}
	w.WriteHeader(204)
}

// logValue shortens a header or field that a caller controls before it is logged.
func logValue(v string) string {
	if len(v) > 100 {
		return v[:100] + "..."
	}
	return v
}
