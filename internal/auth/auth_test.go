package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeGitHub answers the token exchange for the code "good" and returns the
// given login for the token it hands out.
func fakeGitHub(t *testing.T, login string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "good" || r.FormValue("client_id") != "cid" || r.FormValue("client_secret") != "csecret" {
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"login": login, "id": 1})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func newAuth(t *testing.T, baseURL, login string) (*Auth, *http.ServeMux) {
	gh := fakeGitHub(t, login)
	a := New(baseURL, "cid", "csecret", strings.Repeat("k", 32), " xpmatteo , other ")
	a.AuthorizeURL = gh.URL + "/authorize"
	a.TokenURL = gh.URL + "/token"
	a.UserURL = gh.URL + "/user"
	mux := http.NewServeMux()
	a.Register(mux)
	mux.Handle("GET /{$}", a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret page"))
	})))
	return a, mux
}

func do(mux http.Handler, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func cookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie %s in %v", name, w.Result().Cookies())
	return nil
}

func checkFlags(t *testing.T, c *http.Cookie, secure bool) {
	t.Helper()
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure != secure || c.Path != "/" {
		t.Fatalf("cookie flags: %+v", c)
	}
}

// login runs the whole flow and returns the callback response.
func login(t *testing.T, mux http.Handler, code string) *httptest.ResponseRecorder {
	t.Helper()
	w := do(mux, "GET", "/auth/login")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login status %d", w.Code)
	}
	state := cookie(t, w, stateCookie)
	q := url.Values{"code": {code}, "state": {state.Value}}
	return do(mux, "GET", "/auth/callback?"+q.Encode(), state)
}

func TestLoginRedirectsToGitHub(t *testing.T) {
	a, mux := newAuth(t, "https://www.agbredo.com", "xpmatteo")
	w := do(mux, "GET", "/auth/login")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	state := cookie(t, w, stateCookie)
	checkFlags(t, state, true)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), a.AuthorizeURL+"?") || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "https://www.agbredo.com/auth/callback" || q.Get("state") != state.Value || len(state.Value) < 32 {
		t.Fatalf("location %s, state %s", loc, state.Value)
	}
}

func TestCallbackLogsInAnAllowedUser(t *testing.T) {
	_, mux := newAuth(t, "https://www.agbredo.com", "XPMatteo")
	w := login(t, mux, "good")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("status %d, location %s: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	session := cookie(t, w, sessionCookie)
	checkFlags(t, session, true)
	if cookie(t, w, stateCookie).MaxAge >= 0 {
		t.Fatal("state cookie not cleared")
	}
	if w := do(mux, "GET", "/", session); w.Code != http.StatusOK || w.Body.String() != "secret page" {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestCallbackRefusesAUserNotInTheList(t *testing.T) {
	_, mux := newAuth(t, "https://www.agbredo.com", "stranger")
	w := login(t, mux, "good")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Fatal("session cookie set")
		}
	}
}

func TestCallbackChecksTheState(t *testing.T) {
	_, mux := newAuth(t, "https://www.agbredo.com", "xpmatteo")
	state := cookie(t, do(mux, "GET", "/auth/login"), stateCookie)
	if w := do(mux, "GET", "/auth/callback?code=good&state=other", state); w.Code != http.StatusBadRequest {
		t.Errorf("wrong state: status %d", w.Code)
	}
	if w := do(mux, "GET", "/auth/callback?code=good&state="+state.Value); w.Code != http.StatusBadRequest {
		t.Errorf("no state cookie: status %d", w.Code)
	}
}

func TestCallbackWithABadCode(t *testing.T) {
	_, mux := newAuth(t, "https://www.agbredo.com", "xpmatteo")
	if w := login(t, mux, "bad"); w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
}

func TestRequireRefusesBadSessions(t *testing.T) {
	a, mux := newAuth(t, "https://www.agbredo.com", "xpmatteo")
	future := time.Now().Add(time.Hour)
	valid := a.session("xpmatteo", future)
	sig := valid[strings.LastIndex(valid, ":"):]
	for name, value := range map[string]string{
		"tampered login": "other:" + valid[len("xpmatteo:"):],
		"tampered sig":   strings.TrimSuffix(valid, sig) + ":" + strings.Repeat("0", 64),
		"expired":        a.session("xpmatteo", time.Now().Add(-time.Second)),
		"not in list":    a.session("stranger", future),
		"garbage":        "garbage",
	} {
		w := do(mux, "GET", "/", &http.Cookie{Name: sessionCookie, Value: value})
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/login" {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if w := do(mux, "GET", "/"); w.Code != http.StatusSeeOther {
		t.Errorf("no cookie: status %d", w.Code)
	}
	if w := do(mux, "GET", "/", &http.Cookie{Name: sessionCookie, Value: valid}); w.Code != http.StatusOK {
		t.Errorf("valid: status %d", w.Code)
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	_, mux := newAuth(t, "http://localhost:8080", "xpmatteo")
	w := do(mux, "POST", "/auth/logout")
	c := cookie(t, w, sessionCookie)
	if w.Code != http.StatusSeeOther || c.MaxAge >= 0 {
		t.Fatalf("status %d, cookie %+v", w.Code, c)
	}
	checkFlags(t, c, false)
}
