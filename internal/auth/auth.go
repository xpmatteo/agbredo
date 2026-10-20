// Package auth logs users in with GitHub and lets in only the logins in
// AGBREDO_USERS.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "agbredo_session"
	stateCookie   = "agbredo_oauth_state"
	sessionLength = 8 * time.Hour
)

type Auth struct {
	BaseURL, ClientID, ClientSecret string
	SessionKey                      []byte
	Users                           map[string]bool // lower case logins
	// GitHub endpoints; tests point them elsewhere.
	AuthorizeURL, TokenURL, UserURL string
	Client                          *http.Client
}

// New makes an Auth for GitHub. users is a comma-separated list of logins.
func New(baseURL, clientID, clientSecret, sessionKey, users string) *Auth {
	a := &Auth{
		BaseURL: strings.TrimRight(baseURL, "/"), ClientID: clientID, ClientSecret: clientSecret,
		SessionKey: []byte(sessionKey), Users: map[string]bool{},
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserURL:      "https://api.github.com/user",
		Client:       &http.Client{Timeout: 15 * time.Second},
	}
	for _, u := range strings.Split(users, ",") {
		if u = strings.ToLower(strings.TrimSpace(u)); u != "" {
			a.Users[u] = true
		}
	}
	return a
}

func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

// Require lets the request through only with a valid session of an allowed user.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !a.valid(c.Value) {
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	rand.Read(b)
	state := hex.EncodeToString(b)
	a.setCookie(w, stateCookie, state, 600)
	q := url.Values{"client_id": {a.ClientID}, "redirect_uri": {a.BaseURL + "/auth/callback"}, "state": {state}}
	http.Redirect(w, r, a.AuthorizeURL+"?"+q.Encode(), http.StatusSeeOther)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	a.setCookie(w, stateCookie, "", -1)
	if err != nil || !hmac.Equal([]byte(c.Value), []byte(r.URL.Query().Get("state"))) {
		http.Error(w, "Login expired, please try again", http.StatusBadRequest)
		return
	}
	login, err := a.githubLogin(r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "GitHub login failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !a.Users[strings.ToLower(login)] {
		http.Error(w, login+" is not allowed to see Agbredo", http.StatusForbidden)
		return
	}
	a.setCookie(w, sessionCookie, a.session(login, time.Now().Add(sessionLength)), int(sessionLength.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// githubLogin exchanges the OAuth code for a token and asks GitHub whose it is.
func (a *Auth) githubLogin(code string) (string, error) {
	form := url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {code},
		"redirect_uri": {a.BaseURL + "/auth/callback"}}
	req, _ := http.NewRequest("POST", a.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := a.getJSON(req, &token); err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("no access token (%s)", token.Error)
	}
	req, _ = http.NewRequest("GET", a.UserURL, nil)
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	var user struct{ Login string }
	if err := a.getJSON(req, &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("no login")
	}
	return user.Login, nil
}

func (a *Auth) getJSON(req *http.Request, out any) error {
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", req.URL.Path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// session returns "login:expiry:signature".
func (a *Auth) session(login string, expires time.Time) string {
	value := login + ":" + strconv.FormatInt(expires.Unix(), 10)
	return value + ":" + a.sign(value)
}

func (a *Auth) valid(session string) bool {
	parts := strings.Split(session, ":")
	if len(parts) != 3 || !hmac.Equal([]byte(parts[2]), []byte(a.sign(parts[0]+":"+parts[1]))) {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	return err == nil && time.Now().Unix() < expires && a.Users[strings.ToLower(parts[0])]
}

func (a *Auth) sign(value string) string {
	mac := hmac.New(sha256.New, a.SessionKey)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: strings.HasPrefix(a.BaseURL, "https://"), SameSite: http.SameSiteLaxMode})
}
