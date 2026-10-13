// Package auth implements GitHub login and administrator sessions. Browser
// credentials and runner credentials are separate trust domains.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	BaseURL, ClientID, ClientSecret, SessionKey string
	Admins                                      []string
}
type Auth struct {
	config                      Config
	admins                      map[string]bool
	client                      *http.Client
	secure                      bool
	oauthURL, tokenURL, userURL string
}
type session struct {
	Login   string `json:"login"`
	CSRF    string `json:"csrf"`
	Expires int64  `json:"expires"`
}
type oauthState struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"expires"`
}
type contextKey struct{}

func New(c Config) (*Auth, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("RILIAN_BASE_URL must be an absolute origin URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return nil, errors.New("RILIAN_BASE_URL must use HTTPS (HTTP is allowed on localhost)")
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, errors.New("GitHub OAuth client ID and secret are required")
	}
	if len(c.SessionKey) < 32 {
		return nil, errors.New("RILIAN_SESSION_KEY must contain at least 32 random characters")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	a := &Auth{config: c, admins: map[string]bool{}, secure: u.Scheme == "https", client: &http.Client{Timeout: 15 * time.Second}, oauthURL: "https://github.com/login/oauth/authorize", tokenURL: "https://github.com/login/oauth/access_token", userURL: "https://api.github.com/user"}
	for _, admin := range c.Admins {
		if admin = strings.TrimSpace(strings.ToLower(admin)); admin != "" {
			a.admins[admin] = true
		}
	}
	if len(a.admins) == 0 {
		return nil, errors.New("RILIAN_ADMIN_USERS must explicitly allow at least one GitHub login")
	}
	return a, nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (a *Auth) encode(value any) string {
	b, _ := json.Marshal(value)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(a.config.SessionKey))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *Auth) decode(value string, out any) bool {
	if len(value) > 4096 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.config.SessionKey))
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && json.Unmarshal(b, out) == nil
}
func (a *Auth) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: age})
}

func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.Handle("POST /auth/logout", a.RequireAdmin(http.HandlerFunc(a.logout)))
}
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state := oauthState{State: randomToken(), Verifier: randomToken(), Expires: time.Now().Add(10 * time.Minute).Unix()}
	a.cookie(w, "rilian_oauth", a.encode(state), 600)
	sum := sha256.Sum256([]byte(state.Verifier))
	q := url.Values{"client_id": {a.config.ClientID}, "redirect_uri": {a.config.BaseURL + "/auth/callback"}, "state": {state.State}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "allow_signup": {"false"}}
	http.Redirect(w, r, a.oauthURL+"?"+q.Encode(), http.StatusSeeOther)
}
func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie("rilian_oauth")
	a.cookie(w, "rilian_oauth", "", -1)
	var state oauthState
	if err != nil || !a.decode(c.Value, &state) || state.Expires <= time.Now().Unix() || len(state.State) < 32 || subtle.ConstantTimeCompare([]byte(state.State), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "GitHub login state is invalid or expired; sign in again", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 1024 {
		http.Error(w, "GitHub did not return a valid authorization code", http.StatusBadRequest)
		return
	}
	form := url.Values{"client_id": {a.config.ClientID}, "client_secret": {a.config.ClientSecret}, "code": {code}, "code_verifier": {state.Verifier}, "redirect_uri": {a.config.BaseURL + "/auth/callback"}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "GitHub login unavailable", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		http.Error(w, "GitHub token exchange unavailable; sign in again", http.StatusBadGateway)
		return
	}
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&token)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || token.AccessToken == "" || token.Error != "" {
		http.Error(w, "GitHub rejected the authorization code; sign in again", http.StatusUnauthorized)
		return
	}
	req, err = http.NewRequestWithContext(r.Context(), http.MethodGet, a.userURL, nil)
	if err != nil {
		http.Error(w, "GitHub login unavailable", http.StatusBadGateway)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err = a.client.Do(req)
	if err != nil {
		http.Error(w, "GitHub identity lookup unavailable; sign in again", http.StatusBadGateway)
		return
	}
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&user)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || user.ID <= 0 || !a.admins[strings.ToLower(user.Login)] {
		http.Error(w, "Your GitHub account is not in Rilian's administrator allowlist", http.StatusForbidden)
		return
	}
	s := session{Login: user.Login, CSRF: randomToken(), Expires: time.Now().Add(8 * time.Hour).Unix()}
	a.cookie(w, "rilian_session", a.encode(s), 8*60*60)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("rilian_session")
		var s session
		if err != nil || !a.decode(c.Value, &s) || s.Expires <= time.Now().Unix() || s.CSRF == "" || !a.admins[strings.ToLower(s.Login)] {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			} else {
				http.Error(w, "Administrator authentication required", http.StatusUnauthorized)
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, s))
		next.ServeHTTP(w, r)
	})
}
func User(r *http.Request) string           { s, _ := r.Context().Value(contextKey{}).(session); return s.Login }
func CSRF(r *http.Request) string           { s, _ := r.Context().Value(contextKey{}).(session); return s.CSRF }
func (a *Auth) User(r *http.Request) string { return User(r) }
func (a *Auth) CSRF(r *http.Request) string { return CSRF(r) }
func CheckCSRF(r *http.Request) bool {
	if User(r) == "" || CSRF(r) == "" {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	token := r.PostForm.Get("csrf")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	return len(token) <= 256 && subtle.ConstantTimeCompare([]byte(token), []byte(CSRF(r))) == 1
}
func (a *Auth) CheckCSRF(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != a.config.BaseURL {
		return false
	}
	return CheckCSRF(r)
}
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if !a.CheckCSRF(r) {
		http.Error(w, "Invalid CSRF token", http.StatusForbidden)
		return
	}
	a.cookie(w, "rilian_session", "", -1)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}
