package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"agbredo/internal/auth"
	"agbredo/internal/core"
	"agbredo/internal/forward"
	"agbredo/internal/github"
	"agbredo/internal/requestlog"
	"agbredo/internal/store"
	"agbredo/internal/web"
	"agbredo/internal/webapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("Agbredo stopped", "error", core.Redact(err.Error()))
		os.Exit(1)
	}
}
func run(logger *slog.Logger) error {
	if len(os.Args) != 2 || (os.Args[1] != "serve" && os.Args[1] != "migrate") {
		return errors.New("usage: agbredo serve | agbredo migrate")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databaseURL := os.Getenv("AGBREDO_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		return errors.New("AGBREDO_DATABASE_URL is required (Railway DATABASE_URL is also accepted)")
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := store.Open(initCtx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = store.Migrate(initCtx, pool); err != nil {
		return err
	}
	if os.Args[1] == "migrate" {
		logger.Info("migrations applied")
		return nil
	}
	appID, err := strconv.ParseInt(os.Getenv("AGBREDO_GITHUB_APP_ID"), 10, 64)
	if err != nil {
		return errors.New("AGBREDO_GITHUB_APP_ID must be a positive integer")
	}
	key := []byte(os.Getenv("AGBREDO_GITHUB_PRIVATE_KEY"))
	if file := os.Getenv("AGBREDO_GITHUB_PRIVATE_KEY_FILE"); file != "" {
		key, err = os.ReadFile(file)
		if err != nil {
			return errors.New("cannot read AGBREDO_GITHUB_PRIVATE_KEY_FILE")
		}
	}
	backend, err := github.New(appID, key)
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(os.Getenv("AGBREDO_BASE_URL"), "/")
	admins := strings.Split(os.Getenv("AGBREDO_ADMIN_USERS"), ",")
	for i := range admins {
		admins[i] = strings.TrimSpace(admins[i])
	}
	login, err := auth.New(auth.Config{BaseURL: baseURL, ClientID: os.Getenv("AGBREDO_GITHUB_CLIENT_ID"), ClientSecret: os.Getenv("AGBREDO_GITHUB_CLIENT_SECRET"), SessionKey: os.Getenv("AGBREDO_SESSION_KEY"), Admins: admins})
	if err != nil {
		return err
	}
	secret := os.Getenv("AGBREDO_GITHUB_WEBHOOK_SECRET")
	if len(secret) < 32 {
		return errors.New("AGBREDO_GITHUB_WEBHOOK_SECRET must contain at least 32 random characters")
	}
	service := &core.Service{DB: pool, Backend: backend}
	// Optional: forward accepted telemetry to an OTLP store such as OpenObserve.
	// OTLP_FORWARD_URL is the OTLP base, e.g. http://openobserve:5080/api/default.
	forwarder := forward.New(os.Getenv("OTLP_FORWARD_URL"), os.Getenv("OTLP_AUTH_TOKEN"), logger)
	api := &webapi.API{Service: service, Verifier: auth.NewOIDC(baseURL), WebhookSecret: secret, Logger: logger, Forward: forwarder}
	mux := newMux(service, login, api, pool.Ping)
	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("AGBREDO_PORT")
	}
	if port == "" {
		port = "8080"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("PORT or AGBREDO_PORT must be between 1 and 65535")
	}
	server := &http.Server{Addr: ":" + port, Handler: security(mux, logger), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); service.Work(ctx, logger) }()
	workers.Add(1)
	go func() { defer workers.Done(); forwarder.Run(ctx) }()
	serverErr := make(chan error, 1)
	go func() { logger.Info("Agbredo listening", "address", server.Addr); serverErr <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			workers.Wait()
			return err
		}
	}
	stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return fmt.Errorf("HTTP shutdown: %w", err)
	}
	workers.Wait()
	return nil
}

// newMux puts every route together: sign-in, the dashboard behind the admin
// check (it also catches every path nothing else matches), the runner and
// GitHub routes, and health. ping checks the database.
func newMux(service web.Service, login *auth.Auth, api *webapi.API, ping func(context.Context) error) *http.ServeMux {
	mux := http.NewServeMux()
	login.Register(mux)
	dashboard := http.NewServeMux()
	web.New(service, login).Register(dashboard)
	mux.Handle("/", login.RequireAdmin(dashboard))
	api.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if ping(checkCtx) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
func security(next http.Handler, logger *slog.Logger) http.Handler {
	return requestlog.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("HTTP panic", "request_id", requestlog.ID(r), "method", r.Method, "path", r.URL.Path)
				http.Error(w, "Internal server error", 500)
			}
		}()
		next.ServeHTTP(w, r)
	}), logger)
}
