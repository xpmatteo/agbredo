package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agbredo/internal/auth"
	"agbredo/internal/core"
	"agbredo/internal/web"
)

type registrationService struct {
	web.Service
	registered core.Deployment
}

func (s *registrationService) Fleet(context.Context) (core.Fleet, error) {
	return core.Fleet{}, nil
}
func (s *registrationService) SaveDeployment(_ context.Context, a core.Deployment) (core.Deployment, error) {
	a.ID = "registered"
	s.registered = a
	return a, nil
}
func (s *registrationService) Deployment(context.Context, string) (core.Deployment, error) {
	return s.registered, nil
}

// Exercise a browser's ordinary form submission through the real security,
// session, CSRF, template and registration handlers. HTTP test clients cannot
// reproduce how Referrer-Policy changes the browser-generated Origin header.
func TestBrowserDeploymentRegistration(t *testing.T) {
	var chrome string
	for _, candidate := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "google-chrome", "chromium", "chromium-browser"} {
		if path, err := exec.LookPath(candidate); err == nil {
			chrome = path
			break
		}
	}
	if chrome == "" {
		t.Skip("Chrome/Chromium required to verify browser form Origin behavior")
	}

	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	const key = "browser-test-only-session-key-32-characters"
	login, err := auth.New(auth.Config{BaseURL: "http://" + server.Listener.Addr().String(), ClientID: "test", ClientSecret: "test", SessionKey: key, Admins: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	// A test-only signed session replaces the external GitHub login, while the
	// production middleware still authenticates and verifies the cookie itself.
	claims, err := json.Marshal(map[string]any{"login": "admin", "csrf": "browser-test-csrf", "expires": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(payload))
	cookie := &http.Cookie{Name: "agbredo_session", Value: payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}

	service := &registrationService{}
	mux := http.NewServeMux()
	web.New(service, login).Register(mux)
	mux.HandleFunc("GET /submit.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, `const f=document.querySelector('form[action="/agents"]');
for(const [key,value] of Object.entries({name:'Agent instructions audit',description:'Reviews CLAUDE.md and README.md',owner:'xpmatteo',repository:'rpg-interpreter',workflow:'agent-instructions-audit.yml',installation_id:'170927509',budget:'5'})){f.elements.namedItem(key).value=value;}
f.submit();`)
	})
	protected := login.RequireAdmin(mux)
	type submission struct {
		status int
		origin string
	}
	posts := make(chan submission, 1)
	routes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/agents" {
			http.SetCookie(w, cookie)
			r.AddCookie(cookie)
			page := httptest.NewRecorder()
			protected.ServeHTTP(page, r)
			for k, values := range page.Header() {
				w.Header()[k] = values
			}
			w.WriteHeader(page.Code)
			_, _ = io.WriteString(w, page.Body.String()+`<script src="/submit.js"></script>`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/agents" {
			response := httptest.NewRecorder()
			protected.ServeHTTP(response, r)
			for k, values := range response.Header() {
				w.Header()[k] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			posts <- submission{response.Code, r.Header.Get("Origin")}
			return
		}
		protected.ServeHTTP(w, r)
	})
	server.Config.Handler = security(routes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, chrome, "--headless", "--disable-gpu", "--no-sandbox", "--disable-background-networking", "--disable-extensions", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+filepath.Join(t.TempDir(), "chrome"), "--dump-dom", "--timeout=10000", server.URL+"/agents")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser did not complete: %v", err)
	}
	select {
	case got := <-posts:
		if got.status != http.StatusSeeOther || got.origin != server.URL {
			t.Fatalf("registration rejected: status=%d Origin=%q; form verification error=%t", got.status, got.origin, strings.Contains(string(output), "Form verification failed"))
		}
		if service.registered.Repository != "rpg-interpreter" {
			t.Fatal("registration not saved")
		}
	default:
		t.Fatalf("browser did not submit the registration form: %s", output)
	}
}
