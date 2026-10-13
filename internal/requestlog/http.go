// Package requestlog correlates HTTP failures with structured application logs.
// It never logs request bodies, query strings, cookies, authorization headers or
// verification tokens.
package requestlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

type contextKey struct{}

var requestIDPattern = regexp.MustCompile(`^(?:[a-fA-F0-9]{32}|[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})$`)

func ID(r *http.Request) string {
	id, _ := r.Context().Value(contextKey{}).(string)
	return id
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func Middleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				panic(err)
			}
			id = hex.EncodeToString(b[:])
		}
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, id))
		w.Header().Set("X-Request-ID", id)
		response := &responseWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 400 {
				level = slog.LevelWarn
			}
			if status >= 500 {
				level = slog.LevelError
			}
			logger.Log(r.Context(), level, "HTTP request", "request_id", id, "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(response, r)
	})
}

// RejectForm records only a bounded failure category, never the submitted
// values. The response names the cause and gives an ID for finding its logs.
func RejectForm(w http.ResponseWriter, r *http.Request, reason string) {
	message := "The form could not be verified against your login session. Reload the form page and try again."
	switch reason {
	case "origin_unavailable":
		message = "Your browser did not provide a usable page origin. Reload the form page and try again."
	case "origin_mismatch":
		message = "The page address does not match Agbredo's configured address. Open the configured address and try again."
	case "csrf_token_missing":
		message = "This form is missing its verification token. Reload the form page and try again."
	case "csrf_token_mismatch":
		message = "This form's verification token does not match your current login session. Reload the form page and try again."
	case "invalid_form":
		message = "The form is invalid or exceeds the size limit. Reload the form page and try again."
	}
	slog.WarnContext(r.Context(), "browser form rejected", "request_id", ID(r), "method", r.Method, "path", r.URL.Path, "reason", reason)
	if id := ID(r); id != "" {
		message += " Request ID: " + id
	}
	status := http.StatusForbidden
	if reason == "invalid_form" {
		status = http.StatusBadRequest
	}
	http.Error(w, "Form verification failed. "+message, status)
}
