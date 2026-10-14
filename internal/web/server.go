// Package web implements Agbredo's server-rendered administration dashboard.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agbredo/internal/core"
	"agbredo/internal/requestlog"
)

//go:embed templates.html style.css
var assets embed.FS

type Service interface {
	Fleet(context.Context) (core.Fleet, error)
	Agent(context.Context, string) (core.Agent, error)
	SaveAgent(context.Context, core.Agent) (core.Agent, error)
	DeleteAgent(context.Context, string) error
	Run(context.Context, string) (core.Run, error)
	Events(context.Context, string) ([]core.Event, error)
}

type Auth interface {
	User(*http.Request) string
	CSRF(*http.Request) string
	CheckCSRF(*http.Request) bool
}

type Server struct {
	service   Service
	auth      Auth
	templates *template.Template
}

type page struct {
	Title          string
	User           string
	CSRF           string
	Fleet          core.Fleet
	Agent          core.Agent
	Run            core.Run
	Events         []core.Event
	Active, Failed int
	Spending       int64
	Error          string
}

func New(service Service, auth Auth) *Server {
	funcs := template.FuncMap{
		"usd":      money,
		"measured": measured,
		"measuredCost": func(n *int64) string {
			if n == nil {
				return "Unavailable"
			}
			return "$" + money(*n)
		},
		"runCost": func(r core.Run) string {
			if !r.EstimatedCostAvailable {
				return "Unavailable"
			}
			return "$" + money(r.CostMicros)
		},
		"runTokens": runTokens,
		"sourceCost": func(u *core.SourceUsage) string {
			if u.CostNanos == nil {
				return "Unavailable"
			}
			return "$" + money(u.CostMicros())
		},
		"sourceTokens": func(u *core.SourceUsage, kind string) string {
			return usageTokens(u.Usage, kind)
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
	}
	return &Server{service: service, auth: auth, templates: template.Must(template.New("pages").Funcs(funcs).ParseFS(assets, "templates.html"))}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.protect(s.dashboard, false))
	mux.HandleFunc("GET /agents", s.protect(s.agents, false))
	mux.HandleFunc("POST /agents", s.protect(s.saveAgent, true))
	mux.HandleFunc("GET /agents/{id}", s.protect(s.agent, false))
	mux.HandleFunc("POST /agents/{id}", s.protect(s.saveAgent, true))
	mux.HandleFunc("POST /agents/{id}/delete", s.protect(s.deleteAgent, true))
	mux.HandleFunc("GET /runs", s.protect(s.runs, false))
	mux.HandleFunc("GET /runs/{id}", s.protect(s.run, false))
	mux.HandleFunc("GET /static/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		css, _ := assets.ReadFile("style.css")
		_, _ = w.Write(css)
	})
}

func (s *Server) protect(next http.HandlerFunc, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.auth.User(r) == "" {
			http.Error(w, "Administrator authentication required.", http.StatusUnauthorized)
			return
		}
		if mutation {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			if err := r.ParseForm(); err != nil {
				requestlog.RejectForm(w, r, "invalid_form")
				return
			}
			if !s.auth.CheckCSRF(r) {
				reason := "csrf_verification_failed"
				if diagnostic, ok := s.auth.(interface{ CSRFReason(*http.Request) string }); ok {
					reason = diagnostic.CSRFReason(r)
				}
				requestlog.RejectForm(w, r, reason)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	p.User, p.CSRF = s.auth.User(r), s.auth.CSRF(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("render dashboard", "page", name, "error", err)
	}
}

func (s *Server) failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		http.Error(w, "Record not found.", http.StatusNotFound)
	case errors.Is(err, core.ErrUnauthorized):
		http.Error(w, "You are not authorized to perform this action.", http.StatusForbidden)
	case errors.Is(err, core.ErrConflict):
		http.Error(w, core.Redact(err.Error())+". Reload the record to review its status.", http.StatusConflict)
	default:
		slog.Error("dashboard operation failed", "error", core.Redact(err.Error()))
		http.Error(w, "The operation failed. Check the application logs and reload the record before retrying.", http.StatusInternalServerError)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	p := page{Title: "Agbredo — Agent Fleet Control Plane", Fleet: f}
	for _, run := range f.Runs {
		switch run.Status {
		case "queued", "running":
			p.Active++
		case "failed":
			p.Failed++
		}
		p.Spending += run.CostMicros
	}
	s.render(w, r, "dashboard", p)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "agents", page{Title: "Agent registry", Fleet: f, Agent: core.Agent{Ref: "main", Enabled: true}})
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	a, err := s.service.Agent(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "agent", page{Title: a.Name, Agent: a})
}

func (s *Server) saveAgent(w http.ResponseWriter, r *http.Request) {
	budget, err := parseMoney(r.FormValue("budget"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	installation, err := strconv.ParseInt(r.FormValue("installation_id"), 10, 64)
	if err != nil || installation <= 0 {
		http.Error(w, "Installation ID must be a positive integer.", http.StatusBadRequest)
		return
	}
	version := int64(0)
	if r.PathValue("id") != "" {
		version, err = strconv.ParseInt(r.FormValue("version"), 10, 64)
		if err != nil || version <= 0 {
			http.Error(w, "Invalid agent version. Reload the page.", http.StatusBadRequest)
			return
		}
	}
	a := core.Agent{ID: r.PathValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Description: strings.TrimSpace(r.FormValue("description")), Owner: strings.TrimSpace(r.FormValue("owner")), Repository: strings.TrimSpace(r.FormValue("repository")), Workflow: strings.TrimSpace(r.FormValue("workflow")), Ref: strings.TrimSpace(r.FormValue("ref")), InstallationID: installation, BudgetMicros: budget, Enabled: r.FormValue("enabled") == "on", Version: version}
	if err := core.ValidateAgent(a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err = s.service.SaveAgent(r.Context(), a)
	if err != nil {
		s.failure(w, err)
		return
	}
	http.Redirect(w, r, "/agents/"+a.ID, http.StatusSeeOther)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteAgent(r.Context(), r.PathValue("id")); err != nil {
		s.failure(w, err)
		return
	}
	http.Redirect(w, r, "/agents", http.StatusSeeOther)
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "runs", page{Title: "Execution history", Fleet: f})
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, err := s.service.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	events, err := s.service.Events(r.Context(), run.ID)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "run", page{Title: run.Agent.Name + " · Run", Run: run, Events: events})
}

func money(micros int64) string { return fmt.Sprintf("%d.%06d", micros/1_000_000, micros%1_000_000) }

// Parse decimal dollars as integer microdollars, without floating-point rounding.
func parseMoney(raw string) (int64, error) {
	invalid := errors.New("Budget must be a non-negative USD amount with at most six decimal places.")
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, invalid
	}
	for _, part := range parts {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, invalid
			}
		}
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
		if len(frac) > 6 || frac == "" {
			return 0, invalid
		}
	}
	frac += strings.Repeat("0", 6-len(frac))
	value, err := strconv.ParseInt(parts[0]+frac, 10, 64)
	if err != nil {
		return 0, invalid
	}
	return value, nil
}

func measured(n *int64) string {
	if n == nil {
		return "Unavailable"
	}
	return strconv.FormatInt(*n, 10)
}
func runTokens(r core.Run, kind string) string { return usageTokens(r.ClaudeUsage, kind) }
func usageTokens(u *core.ClaudeUsage, kind string) string {
	if u == nil {
		return "Unavailable"
	}
	switch kind {
	case "input":
		return measured(u.InputTokens)
	case "output":
		return measured(u.OutputTokens)
	case "cacheRead":
		return measured(u.CacheReadTokens)
	default:
		return measured(u.CacheCreationTokens)
	}
}
