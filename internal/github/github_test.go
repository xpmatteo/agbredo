package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agbredo/internal/core"
	gh "github.com/google/go-github/v84/github"
)

func mockBackend(t *testing.T, h http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c := gh.NewClient(server.Client())
	c.BaseURL, _ = url.Parse(server.URL + "/")
	return &Backend{app: c, clientFor: func(int64) (*gh.Client, error) { return c, nil }}
}

func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"action":"completed"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifyWebhook("secret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	for _, test := range []struct {
		secret string
		body   []byte
		sig    string
	}{{"", body, sig}, {"wrong", body, sig}, {"secret", []byte("tampered"), sig}, {"secret", body, "sha1=abc"}, {"secret", body, "sha256=oops"}} {
		if VerifyWebhook(test.secret, test.body, test.sig) {
			t.Fatalf("accepted invalid signature: %q", test.sig)
		}
	}
}

func TestGetRunWorkflowIdentity(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/123") {
			w.Write([]byte(`{"id":123,"workflow_id":4,"head_branch":"main","display_title":"unique","html_url":"https://github.com/o/r/actions/runs/123"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/workflows/4") {
			w.Write([]byte(`{"id":4,"path":".github/workflows/continue.yml"}`))
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(404)
	})
	run, err := b.GetRun(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 123)
	if err != nil || run.Workflow != "continue.yml" || run.Ref != "main" {
		t.Fatalf("wrong workflow: %#v %v", run, err)
	}
}

func TestGetHistoricalWorkflowAttempt(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/123/attempts/2" {
			t.Errorf("wrong attempt endpoint: %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":123,"run_attempt":2,"path":".github/workflows/existing.yml","head_branch":"main","status":"completed","conclusion":"failure"}`))
	})
	run, err := b.GetRunAttempt(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 123, 2)
	if err != nil || run.Attempt != 2 || run.Workflow != "existing.yml" || run.Conclusion != "failure" {
		t.Fatalf("historical attempt not retained: %#v %v", run, err)
	}
}

// The installation is asked for as the App. A 404 means the App is not
// installed on the repository; other failures are not mistaken for that.
func TestInstallation(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/installation":
			w.Write([]byte(`{"id":42,"account":{"login":"o"}}`))
		case "/repos/o/missing/installation":
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"Not Found"}`))
		default:
			w.WriteHeader(500)
		}
	})
	ctx := context.Background()
	if id, err := b.Installation(ctx, "o", "r"); err != nil || id != 42 {
		t.Fatalf("installation %d, %v", id, err)
	}
	if _, err := b.Installation(ctx, "o", "missing"); !errors.Is(err, core.ErrNotInstalled) || !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("not installed: %v", err)
	}
	if _, err := b.Installation(ctx, "o", "broken"); err == nil || errors.Is(err, core.ErrNotInstalled) {
		t.Fatalf("server failure: %v", err)
	}
}

func TestExternalFailureIsReturned(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	a := core.Deployment{Owner: "o", Repository: "r"}
	if _, err := b.GetRun(context.Background(), a, 1); err == nil {
		t.Fatal("run lookup failure masked")
	}
	if _, err := b.GetRunAttempt(context.Background(), a, 1, 1); err == nil {
		t.Fatal("attempt lookup failure masked")
	}
}
