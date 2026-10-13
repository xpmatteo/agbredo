// Package web implements Rilian's server-rendered administration dashboard.
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

	"rilian/internal/core"
)

//go:embed templates.html style.css
var assets embed.FS

type Service interface {
	Fleet(context.Context) (core.Fleet, error)
	Agent(context.Context, string) (core.Agent, error)
	SaveAgent(context.Context, core.Agent) (core.Agent, error)
	DeleteAgent(context.Context, string) error
	Launch(context.Context, string, string) (core.Run, error)
	Run(context.Context, string) (core.Run, error)
	Events(context.Context, string) ([]core.Event, error)
	Approval(context.Context, string) (core.Approval, error)
	Decide(context.Context, string, string, string, string) error
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
	Title                   string
	User                    string
	CSRF                    string
	Fleet                   core.Fleet
	Agent                   core.Agent
	Run                     core.Run
	Approval                core.Approval
	Events                  []core.Event
	RunApprovals            []core.Approval
	Active, Failed, Pending int
	Spending                int64
	Error                   string
}

func New(service Service, auth Auth) *Server {
	funcs := template.FuncMap{
		"usd": money,
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		"pending": func(a core.Approval) bool { return a.Status == "pending" && time.Now().Before(a.ExpiresAt) },
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
	mux.HandleFunc("POST /agents/{id}/launch", s.protect(s.launch, true))
	mux.HandleFunc("GET /runs", s.protect(s.runs, false))
	mux.HandleFunc("GET /runs/{id}", s.protect(s.run, false))
	mux.HandleFunc("GET /approvals", s.protect(s.approvals, false))
	mux.HandleFunc("GET /approvals/{id}", s.protect(s.approval, false))
	mux.HandleFunc("POST /approvals/{id}", s.protect(s.decide, true))
	mux.HandleFunc("POST /approvals/{id}/retry", s.protect(s.retry, true))
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
				http.Error(w, "Invalid or oversized form.", http.StatusBadRequest)
				return
			}
			if !s.auth.CheckCSRF(r) {
				http.Error(w, "Form verification failed. Reload the page and try again.", http.StatusForbidden)
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
	p := page{Title: "Rilian — Agent Fleet Control Plane", Fleet: f}
	for _, run := range f.Runs {
		switch run.Status {
		case "queued", "running", "waiting_approval":
			p.Active++
		case "failed":
			p.Failed++
		}
		p.Spending += run.CostMicros
	}
	for _, a := range f.Approvals {
		if a.Status == "pending" && time.Now().Before(a.ExpiresAt) {
			p.Pending++
		}
	}
	s.render(w, r, "dashboard", p)
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "agents", page{Title: "Agent registry", Fleet: f, Agent: core.Agent{Ref: "main", Workflow: "rilian-agent.yml", ContinuationWorkflow: "rilian-continue.yml", Enabled: true}})
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
	a := core.Agent{ID: r.PathValue("id"), Name: strings.TrimSpace(r.FormValue("name")), Description: strings.TrimSpace(r.FormValue("description")), Owner: strings.TrimSpace(r.FormValue("owner")), Repository: strings.TrimSpace(r.FormValue("repository")), Workflow: strings.TrimSpace(r.FormValue("workflow")), ContinuationWorkflow: strings.TrimSpace(r.FormValue("continuation_workflow")), Ref: strings.TrimSpace(r.FormValue("ref")), InstallationID: installation, BudgetMicros: budget, Enabled: r.FormValue("enabled") == "on", Version: version}
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

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	run, err := s.service.Launch(r.Context(), r.PathValue("id"), s.auth.User(r))
	if err != nil {
		s.failure(w, err)
		return
	}
	http.Redirect(w, r, "/runs/"+run.ID, http.StatusSeeOther)
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
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	p := page{Title: run.Agent.Name + " · Run", Run: run, Events: events}
	for _, a := range f.Approvals {
		if a.RunID == run.ID {
			p.RunApprovals = append(p.RunApprovals, a)
		}
	}
	s.render(w, r, "run", p)
}

func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Fleet(r.Context())
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "approvals", page{Title: "Approval queue and history", Fleet: f})
}

func (s *Server) approval(w http.ResponseWriter, r *http.Request) {
	a, err := s.service.Approval(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failure(w, err)
		return
	}
	s.render(w, r, "approval", page{Title: "Review proposed action", Approval: a})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	decision := r.FormValue("decision")
	if decision != "approve" && decision != "reject" {
		http.Error(w, "Choose approve or reject.", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) > 2000 {
		http.Error(w, "Decision reason must be 2,000 characters or fewer.", http.StatusBadRequest)
		return
	}
	if err := s.service.Decide(r.Context(), r.PathValue("id"), s.auth.User(r), decision, reason); err != nil {
		s.failure(w, err)
		return
	}
	http.Redirect(w, r, "/approvals/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	service, ok := s.service.(interface {
		Retry(context.Context, string, string) error
	})
	if !ok {
		http.Error(w, "Continuation retry is unavailable.", http.StatusNotImplemented)
		return
	}
	if err := service.Retry(r.Context(), r.PathValue("id"), s.auth.User(r)); err != nil {
		s.failure(w, err)
		return
	}
	http.Redirect(w, r, "/approvals/"+r.PathValue("id"), http.StatusSeeOther)
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
