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
	Ping(ctx context.Context) error
}

// Register adds the pages, behind require, and /readyz to mux.
func Register(mux *http.ServeMux, s Store, require func(http.Handler) http.Handler) {
	mux.Handle("GET /{$}", require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs, err := s.LatestRuns(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		render(w, "runs", runs)
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
		render(w, "run", map[string]any{"Run": events[len(events)-1], "Events": events})
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
