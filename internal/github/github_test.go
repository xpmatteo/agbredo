package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v84/github"
	"rilian/internal/core"
)

func mockBackend(t *testing.T, h http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c := gh.NewClient(server.Client())
	c.BaseURL, _ = url.Parse(server.URL + "/")
	return &Backend{clientFor: func(int64) (*gh.Client, error) { return c, nil }}
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

func TestDispatchAndCorrelation(t *testing.T) {
	dispatch := false
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/dispatches"):
			if r.Method != "POST" {
				t.Error("dispatch must POST")
			}
			var data struct {
				Ref    string
				Inputs map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Error(err)
			}
			if data.Ref != "main" || data.Inputs["rilian_correlation_id"] != "unique" {
				t.Errorf("wrong dispatch: %#v", data)
			}
			dispatch = true
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			if r.URL.Query().Get("event") != "workflow_dispatch" || r.URL.Query().Get("created") == "" {
				t.Error("missing dispatch and time filters")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"workflow_runs":[{"id":1,"display_title":"rilian:unique:initial","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://github.com/o/r/actions/runs/1"},{"id":2,"display_title":"rilian:other:initial"}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	a := core.Agent{Owner: "o", Repository: "r", Workflow: "agent.yml", ContinuationWorkflow: "agent.yml", Ref: "main"}
	if err := b.Dispatch(context.Background(), a, a.Workflow, map[string]interface{}{"rilian_correlation_id": "unique"}); err != nil {
		t.Fatal(err)
	}
	if !dispatch {
		t.Fatal("dispatch absent")
	}
	runs, err := b.FindRuns(context.Background(), a, "rilian:unique:initial", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 1 || runs[0].Workflow != "agent.yml" {
		t.Fatalf("wrong correlation: %#v", runs)
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
	run, err := b.GetRun(context.Background(), core.Agent{Owner: "o", Repository: "r"}, 123)
	if err != nil || run.Workflow != "continue.yml" || run.Ref != "main" {
		t.Fatalf("wrong workflow: %#v %v", run, err)
	}
}

func TestIssueRecoveryPaginatesClosedIssues(t *testing.T) {
	calls := 0
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var issue gh.IssueRequest
			if err := json.NewDecoder(r.Body).Decode(&issue); err != nil {
				t.Error(err)
			}
			if issue.GetTitle() != "Approved" || !strings.Contains(issue.GetBody(), issueMarker("approval1")) {
				t.Errorf("missing approved payload: %#v", issue)
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"html_url":"https://github.com/o/r/issues/9"}`))
			return
		}
		calls++
		if r.URL.Query().Get("state") != "all" {
			t.Error("recovery must include closed issues")
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
			w.Write([]byte(`[{"body":"unrelated"},{"body":"<!-- rilian-approval:approval1 -->","pull_request":{"url":"pr"},"html_url":"wrong"}]`))
			return
		}
		w.Write([]byte(`[{"body":"<!-- rilian-approval:approval1 -->","state":"closed","html_url":"https://github.com/o/r/issues/9"}]`))
	})
	a := core.Agent{Owner: "o", Repository: "r"}
	result, err := b.CreateIssue(context.Background(), a, core.Proposal{Title: "Approved", Body: "Immutable"}, "approval1")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := b.FindIssue(context.Background(), a, "approval1")
	if err != nil || result != recovered || calls != 2 {
		t.Fatalf("recovery %s calls=%d err=%v", recovered, calls, err)
	}
}

func TestExternalFailureIsReturned(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	a := core.Agent{Owner: "o", Repository: "r"}
	if err := b.Dispatch(context.Background(), a, "agent.yml", nil); err == nil {
		t.Fatal("dispatch failure masked")
	}
	if _, err := b.CreateIssue(context.Background(), a, core.Proposal{}, "id"); err == nil {
		t.Fatal("issue failure masked")
	}
	if _, err := b.FindIssue(context.Background(), a, "id"); err == nil {
		t.Fatal("recovery failure masked")
	}
}
