// Package web serves the dashboard pages.
package web

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"agbredo/internal/openobserve"
	"agbredo/internal/store"
)

//go:embed pages.html
var files embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"time": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
}).ParseFS(files, "pages.html"))

type Store interface {
	LatestRuns(ctx context.Context) ([]store.Event, error)
	RunEvents(ctx context.Context, runID int64, attempt int) ([]store.Event, error)
	OpenedBy(ctx context.Context, runIDs []int64) ([]store.Opened, error)
	Ping(ctx context.Context) error
}

// Agents tells which Agents ran in a Run.
type Agents interface {
	AgentSteps(ctx context.Context, runIDs []int64, since time.Time) ([]openobserve.AgentStep, error)
}

// run is a Run with its Agents and what it opened.
type run struct {
	store.Event
	Agents []openobserve.AgentStep
	Opened []store.Opened
}

// details adds to each Run its Agents and the PRs and Issues it opened. When
// OpenObserve fails we still show the Runs, and agentsOK is false.
func details(ctx context.Context, s Store, a Agents, events []store.Event) (runs []run, agentsOK bool, err error) {
	var ids []int64
	since := time.Now()
	for _, e := range events {
		ids = append(ids, e.RunID)
		if !e.RunStartedAt.IsZero() && e.RunStartedAt.Before(since) {
			since = e.RunStartedAt
		}
	}
	opened, err := s.OpenedBy(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	agents, err := a.AgentSteps(actx, ids, since.Add(-time.Minute))
	if err != nil {
		log.Printf("web: agent steps: %v", err)
	}
	runs = make([]run, len(events))
	for i, e := range events {
		runs[i].Event = e
		for _, o := range opened {
			if o.RunID == e.RunID && o.RunAttempt == e.RunAttempt {
				runs[i].Opened = append(runs[i].Opened, o)
			}
		}
		for _, step := range agents {
			if step.RunID == e.RunID && step.RunAttempt == e.RunAttempt {
				runs[i].Agents = append(runs[i].Agents, step)
			}
		}
	}
	return runs, err == nil, nil
}

// Register adds the pages, behind require, and /readyz to mux.
func Register(mux *http.ServeMux, s Store, a Agents, require func(http.Handler) http.Handler) {
	mux.Handle("GET /{$}", require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		latest, err := s.LatestRuns(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		runs, agentsOK, err := details(r.Context(), s, a, latest)
		if err != nil {
			serverError(w, err)
			return
		}
		render(w, "runs", map[string]any{"Runs": runs, "AgentsOK": agentsOK})
	})))
	mux.Handle("GET /runs/{run_id}/{attempt}", require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runID, err1 := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		attempt, err2 := strconv.Atoi(r.PathValue("attempt"))
		if err1 != nil || err2 != nil {
			http.NotFound(w, r)
			return
		}
		events, err := s.RunEvents(r.Context(), runID, attempt)
		if err != nil {
			serverError(w, err)
			return
		}
		if len(events) == 0 {
			http.NotFound(w, r)
			return
		}
		runs, agentsOK, err := details(r.Context(), s, a, events[len(events)-1:])
		if err != nil {
			serverError(w, err)
			return
		}
		render(w, "run", map[string]any{"Run": runs[0], "Events": events, "AgentsOK": agentsOK})
	})))
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "Internal error", http.StatusInternalServerError)
}
