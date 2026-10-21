// Command agbredo serves the Agbredo dashboard and receives GitHub webhooks.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"agbredo/internal/auth"
	"agbredo/internal/store"
	"agbredo/internal/web"
	"agbredo/internal/webhook"
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
		"AGBREDO_GITHUB_CLIENT_SECRET", "AGBREDO_GITHUB_WEBHOOK_SECRET", "AGBREDO_SESSION_KEY", "AGBREDO_USERS"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(env["AGBREDO_SESSION_KEY"]) < 32 {
		return fmt.Errorf("AGBREDO_SESSION_KEY must have at least 32 characters")
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

	a := auth.New(env["AGBREDO_BASE_URL"], env["AGBREDO_GITHUB_CLIENT_ID"], env["AGBREDO_GITHUB_CLIENT_SECRET"],
		env["AGBREDO_SESSION_KEY"], env["AGBREDO_USERS"])
	mux := http.NewServeMux()
	mux.Handle("POST /api/github/webhook", &webhook.Handler{Secret: []byte(env["AGBREDO_GITHUB_WEBHOOK_SECRET"]), Store: db})
	a.Register(mux)
	web.Register(mux, db, a.Require)

	log.Printf("listening on :%s", port)
	server := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}
