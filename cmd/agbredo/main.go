// Command agbredo serves the Agbredo dashboard, receives GitHub webhooks and
// forwards Claude Code telemetry to OpenObserve.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"agbredo/internal/auth"
	"agbredo/internal/store"
	"agbredo/internal/telemetry"
	"agbredo/internal/web"
	"agbredo/internal/webhook"

	"github.com/MicahParks/keyfunc/v3"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "serve" {
		log.Fatal("usage: agbredo serve")
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

func serve() error {
	env := map[string]string{}
	for _, name := range []string{"AGBREDO_DATABASE_URL", "AGBREDO_BASE_URL", "AGBREDO_GITHUB_CLIENT_ID",
		"AGBREDO_GITHUB_CLIENT_SECRET", "AGBREDO_GITHUB_WEBHOOK_SECRET", "AGBREDO_SESSION_KEY", "AGBREDO_USERS",
		"AGBREDO_PIPELINE_REPOS", "AGBREDO_OPENOBSERVE_URL", "AGBREDO_OPENOBSERVE_AUTH"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(env["AGBREDO_SESSION_KEY"]) < 32 {
		return fmt.Errorf("AGBREDO_SESSION_KEY must have at least 32 characters")
	}
	openObserve, err := url.Parse(env["AGBREDO_OPENOBSERVE_URL"])
	if err != nil {
		return fmt.Errorf("AGBREDO_OPENOBSERVE_URL: %w", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = os.Getenv("AGBREDO_PORT")
	}
	if port == "" {
		port = "8080"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(ctx, env["AGBREDO_DATABASE_URL"])
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	jwks, err := keyfunc.NewDefaultCtx(context.Background(), []string{telemetry.JWKSURL})
	if err != nil {
		return fmt.Errorf("load GitHub OIDC keys: %w", err)
	}

	a := auth.New(env["AGBREDO_BASE_URL"], env["AGBREDO_GITHUB_CLIENT_ID"], env["AGBREDO_GITHUB_CLIENT_SECRET"],
		env["AGBREDO_SESSION_KEY"], env["AGBREDO_USERS"])
	mux := http.NewServeMux()
	mux.Handle("POST /api/github/webhook", &webhook.Handler{Secret: []byte(env["AGBREDO_GITHUB_WEBHOOK_SECRET"]), Store: db})
	mux.Handle("POST /api/otlp/v1/{signal}", telemetry.New(jwks.Keyfunc, env["AGBREDO_PIPELINE_REPOS"],
		openObserve, env["AGBREDO_OPENOBSERVE_AUTH"]))
	a.Register(mux)
	web.Register(mux, db, a.Require)

	log.Printf("listening on :%s", port)
	server := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}
