package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestGetRunAttemptWorkflowIdentity(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/123/attempts/1") {
			w.Write([]byte(`{"id":123,"run_attempt":1,"workflow_id":4,"head_branch":"main","display_title":"unique","html_url":"https://github.com/o/r/actions/runs/123"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/workflows/4") {
			w.Write([]byte(`{"id":4,"path":".github/workflows/continue.yml"}`))
			return
		}
		t.Errorf("unexpected request %s", r.URL.Path)
		w.WriteHeader(404)
	})
	run, err := b.GetRunAttempt(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 123, 1)
	if err != nil || run.Workflow != "continue.yml" || run.HeadBranch != "main" {
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

// A suspended installation counts as not installed.
func TestSuspendedInstallationIsNotInstalled(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":42,"suspended_at":"2026-10-01T00:00:00Z"}`))
	})
	if _, err := b.Installation(context.Background(), "o", "r"); !errors.Is(err, core.ErrNotInstalled) {
		t.Fatalf("suspended installation: %v", err)
	}
}

// A 404 is remembered for five minutes per repository, ignoring case. Other
// answers are not remembered.
func TestNotInstalledIsRemembered(t *testing.T) {
	asked := map[string]int{}
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		asked[r.URL.Path]++
		switch r.URL.Path {
		case "/repos/o/r/installation":
			w.Write([]byte(`{"id":42}`))
		case "/repos/o/broken/installation":
			w.WriteHeader(502)
		default:
			w.WriteHeader(404)
		}
	})
	clock := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	ctx := context.Background()
	for _, name := range []string{"missing", "Missing", "MISSING"} {
		if _, err := b.Installation(ctx, "o", name); !errors.Is(err, core.ErrNotInstalled) {
			t.Fatal(name, err)
		}
	}
	if n := asked["/repos/o/missing/installation"]; n != 1 {
		t.Fatalf("asked GitHub %d times within five minutes", n)
	}
	clock = clock.Add(5*time.Minute - time.Second)
	b.Installation(ctx, "o", "missing")
	if n := asked["/repos/o/missing/installation"]; n != 1 {
		t.Fatalf("forgot the answer too soon: %d", n)
	}
	clock = clock.Add(time.Second)
	if _, err := b.Installation(ctx, "o", "missing"); !errors.Is(err, core.ErrNotInstalled) || asked["/repos/o/missing/installation"] != 2 {
		t.Fatalf("did not ask again after five minutes: %v, %d", err, asked["/repos/o/missing/installation"])
	}
	for range 2 {
		b.Installation(ctx, "o", "r")
		b.Installation(ctx, "o", "broken")
	}
	if asked["/repos/o/r/installation"] != 2 || asked["/repos/o/broken/installation"] != 2 {
		t.Fatalf("remembered an answer other than 404: %v", asked)
	}
}

func TestExternalFailureIsReturned(t *testing.T) {
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	a := core.Deployment{Owner: "o", Repository: "r"}
	if _, err := b.GetRunAttempt(context.Background(), a, 1, 1); err == nil {
		t.Fatal("attempt lookup failure masked")
	}
}

// Jobs come from the attempt's jobs endpoint, 100 per page. Steps keep
// GitHub's numbers, and missing times stay missing.
func TestListJobs(t *testing.T) {
	pages := 0
	b := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/actions/runs/123/attempts/2/jobs" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("wrong request %s", r.URL)
		}
		pages++
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2&per_page=100>; rel="next"`)
			w.Write([]byte(`{"total_count":2,"jobs":[{"id":501,"name":"audit / audit","status":"completed","conclusion":"success",
				"started_at":"2026-10-15T07:08:02Z","completed_at":"2026-10-15T07:08:14Z","runner_name":"GitHub Actions 7",
				"html_url":"https://github.com/o/r/actions/runs/123/job/501","steps":[
				{"number":1,"name":"Gate","status":"completed","conclusion":"success","started_at":"2026-10-15T07:08:03Z","completed_at":"2026-10-15T07:08:05Z"},
				{"number":2,"name":"Audit with Claude","status":"completed","conclusion":"skipped","started_at":null,"completed_at":null},
				{"number":28,"name":"Post Gate","status":"completed","conclusion":"success"}]}]}`))
			return
		}
		w.Write([]byte(`{"total_count":2,"jobs":[{"id":502,"name":"report","status":"completed","conclusion":"skipped"}]}`))
	})
	jobs, err := b.ListJobs(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 123, 2)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 15, 7, 8, 2, 0, time.UTC)
	want := []core.Job{{ID: 501, Name: "audit / audit", Status: "completed", Conclusion: "success", StartedAt: started, CompletedAt: started.Add(12 * time.Second),
		RunnerName: "GitHub Actions 7", URL: "https://github.com/o/r/actions/runs/123/job/501", Steps: []core.Step{
			{Number: 1, Name: "Gate", Status: "completed", Conclusion: "success", StartedAt: started.Add(time.Second), CompletedAt: started.Add(3 * time.Second)},
			{Number: 2, Name: "Audit with Claude", Status: "completed", Conclusion: "skipped"},
			{Number: 28, Name: "Post Gate", Status: "completed", Conclusion: "success"}}},
		{ID: 502, Name: "report", Status: "completed", Conclusion: "skipped"}}
	if pages != 2 || len(jobs) != 2 {
		t.Fatalf("%d pages, %#v", pages, jobs)
	}
	for i := range want {
		if !jobs[i].StartedAt.Equal(want[i].StartedAt) || !jobs[i].CompletedAt.Equal(want[i].CompletedAt) {
			t.Fatalf("job %d times %v %v", i, jobs[i].StartedAt, jobs[i].CompletedAt)
		}
		jobs[i].StartedAt, jobs[i].CompletedAt, want[i].StartedAt, want[i].CompletedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		for k := range want[i].Steps {
			if !jobs[i].Steps[k].StartedAt.Equal(want[i].Steps[k].StartedAt) || !jobs[i].Steps[k].CompletedAt.Equal(want[i].Steps[k].CompletedAt) {
				t.Fatalf("step %d times", k)
			}
			jobs[i].Steps[k].StartedAt, jobs[i].Steps[k].CompletedAt, want[i].Steps[k].StartedAt, want[i].Steps[k].CompletedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		}
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("got %#v", jobs)
	}
}

// An attempt without jobs is an empty list, not unknown. A failed page is an
// error with no jobs. Paging stops once there are enough jobs.
func TestListJobsEmptyFailedAndEnough(t *testing.T) {
	empty := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":0,"jobs":[]}`))
	})
	jobs, err := empty.ListJobs(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 1, 1)
	if err != nil || jobs == nil || len(jobs) != 0 {
		t.Fatalf("%#v %v", jobs, err)
	}
	failing := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2&per_page=100>; rel="next"`)
			w.Write([]byte(`{"total_count":2,"jobs":[{"id":1,"name":"a"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	if jobs, err = failing.ListJobs(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 1, 1); err == nil || jobs != nil {
		t.Fatalf("%#v %v", jobs, err)
	}
	pages := 0
	full := mockBackend(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=9&per_page=100>; rel="next"`)
		var b strings.Builder
		b.WriteString(`{"total_count":500,"jobs":[`)
		for i := range core.MaxJobs {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"name":"j"}`, i+1)
		}
		b.WriteString(`]}`)
		w.Write([]byte(b.String()))
	})
	if jobs, err = full.ListJobs(context.Background(), core.Deployment{Owner: "o", Repository: "r"}, 1, 1); err != nil || len(jobs) != core.MaxJobs || pages != 1 {
		t.Fatalf("%d jobs, %d pages, %v", len(jobs), pages, err)
	}
}
