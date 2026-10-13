package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := New(Config{BaseURL: "https://agbredo.example", ClientID: "client", ClientSecret: "secret", SessionKey: strings.Repeat("k", 32), Admins: []string{"Alice"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestConfigurationRejectsUnsafeDefaults(t *testing.T) {
	good := Config{BaseURL: "https://agbredo.example", ClientID: "c", ClientSecret: "s", SessionKey: strings.Repeat("k", 32), Admins: []string{"alice"}}
	cases := []Config{good, good, good, good}
	cases[0].BaseURL = "http://public.example"
	cases[1].SessionKey = "short"
	cases[2].Admins = nil
	cases[3].ClientSecret = ""
	for _, c := range cases {
		if _, err := New(c); err == nil {
			t.Fatalf("accepted unsafe configuration %#v", c)
		}
	}
	good.BaseURL = "http://localhost:8080"
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
}
func TestAdministratorSessionAndCSRF(t *testing.T) {
	a := testAuth(t)
	handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if User(r) != "Alice" || CSRF(r) != "nonce" {
			t.Error("identity not attached")
		}
		if !a.CheckCSRF(r) {
			http.Error(w, "CSRF", 403)
			return
		}
		w.WriteHeader(204)
	}))
	cookie := &http.Cookie{Name: "agbredo_session", Value: a.encode(session{Login: "Alice", CSRF: "nonce", Expires: time.Now().Add(time.Hour).Unix()})}
	for _, test := range []struct {
		csrf, origin string
		status       int
	}{{"nonce", "https://agbredo.example", 204}, {"wrong", "https://agbredo.example", 403}, {"nonce", "https://evil.example", 403}} {
		r := httptest.NewRequest("POST", "https://agbredo.example/approvals/1", strings.NewReader(url.Values{"csrf": {test.csrf}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", test.origin)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("csrf result=%d wanted=%d", w.Code, test.status)
		}
	}
	a.admins = map[string]bool{"other": true}
	r := httptest.NewRequest("POST", "https://agbredo.example/approvals/1", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("removed administrator retains access")
	}
}
func TestInvalidAndExpiredSessions(t *testing.T) {
	a := testAuth(t)
	handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unauthorized handler reached") }))
	for _, value := range []string{"", a.encode(session{Login: "Alice", CSRF: "nonce", Expires: time.Now().Add(-time.Minute).Unix()}), a.encode(session{Login: "mallory", CSRF: "nonce", Expires: time.Now().Add(time.Hour).Unix()}), a.encode(session{Login: "Alice", CSRF: "nonce", Expires: time.Now().Add(time.Hour).Unix()}) + "tamper"} {
		r := httptest.NewRequest("POST", "/approval", nil)
		r.AddCookie(&http.Cookie{Name: "agbredo_session", Value: value})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("invalid session status %d", w.Code)
		}
	}
}
func TestOAuthStatePKCEAndAllowlist(t *testing.T) {
	a := testAuth(t)
	login := httptest.NewRecorder()
	a.login(login, httptest.NewRequest("GET", "/auth/login", nil))
	redirect, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := redirect.Query()
	if query.Get("code_challenge_method") != "S256" || len(query.Get("code_challenge")) != 43 || query.Get("scope") != "" {
		t.Fatalf("unsafe OAuth request %s", redirect)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatal("OAuth cookie flags")
	}
	var state oauthState
	if !a.decode(cookies[0].Value, &state) {
		t.Fatal("OAuth cookie decode")
	}
	loginName := "Alice"
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenCalls++
			r.ParseForm()
			if r.PostForm.Get("code_verifier") != state.Verifier || r.PostForm.Get("client_secret") != "secret" {
				t.Error("invalid token exchange")
			}
			w.Write([]byte(`{"access_token":"ephemeral"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer ephemeral" {
			t.Error("missing OAuth user token")
		}
		w.Write([]byte(`{"id":123,"login":"` + loginName + `"}`))
	}))
	defer server.Close()
	a.tokenURL = server.URL + "/token"
	a.userURL = server.URL + "/user"
	request := func(s string) *http.Request {
		r := httptest.NewRequest("GET", "/auth/callback?code=one-use&state="+s, nil)
		r.AddCookie(cookies[0])
		return r
	}
	wrong := httptest.NewRecorder()
	a.callback(wrong, request("wrong"))
	if wrong.Code != 400 || tokenCalls != 0 {
		t.Fatal("invalid state reached token exchange")
	}
	ok := httptest.NewRecorder()
	a.callback(ok, request(state.State))
	if ok.Code != 303 || ok.Header().Get("Location") != "/" {
		t.Fatalf("login failed %d %s", ok.Code, ok.Body.String())
	}
	sessionFound := false
	for _, c := range ok.Result().Cookies() {
		if c.Name == "agbredo_session" {
			sessionFound = true
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 28800 {
				t.Fatal("session flags")
			}
		}
	}
	if !sessionFound {
		t.Fatal("session missing")
	}
	loginName = "Mallory"
	denied := httptest.NewRecorder()
	a.callback(denied, request(state.State))
	if denied.Code != 403 {
		t.Fatal("non-allowlisted account authenticated")
	}
}
