package openobserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 6, 39, 0, 0, time.UTC)

func setup(t *testing.T, status int, response string) (*Client, *map[string]any, *http.Request) {
	t.Helper()
	var query map[string]any
	var got http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &query)
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL + "/api/my-org")
	return &Client{URL: base, Authorization: "Basic c2VhcmNo", HTTP: server.Client()}, &query, &got
}

func TestAgentSteps(t *testing.T) {
	c, query, req := setup(t, http.StatusOK, `{"took":3,"hits":[
		{"github_run_id":"39556711562","github_run_attempt":"1","github_step":"agent","first_at":1792564764858000,"last_at":1792564800000000},
		{"github_run_id":"bad","github_run_attempt":"1","github_step":"agent","first_at":1,"last_at":2}
	],"total":2}`)
	steps, err := c.AgentSteps(context.Background(), []int64{39556711562, 7}, t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentStep{{RunID: 39556711562, RunAttempt: 1, Step: "agent",
		First: time.UnixMicro(1792564764858000).UTC(), Last: time.UnixMicro(1792564800000000).UTC()}}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("got  %+v\nwant %+v", steps, want)
	}
	if req.URL.Path != "/api/my-org/_search" || req.URL.Query().Get("type") != "logs" || req.Header.Get("Authorization") != "Basic c2VhcmNo" {
		t.Fatalf("request %s %v", req.URL, req.Header)
	}
	q := (*query)["query"].(map[string]any)
	if sql := q["sql"].(string); !strings.Contains(sql, `FROM "default"`) || !strings.Contains(sql, `IN ('39556711562','7')`) {
		t.Errorf("sql %s", sql)
	}
	if q["start_time"].(float64) != float64(t0.UnixMicro()) {
		t.Errorf("start_time %v", q["start_time"])
	}
}

func TestAgentStepsWithoutRuns(t *testing.T) {
	c := &Client{}
	if steps, err := c.AgentSteps(context.Background(), nil, t0); err != nil || steps != nil {
		t.Fatal(steps, err)
	}
}

func TestAgentStepsErrors(t *testing.T) {
	for name, s := range map[string]struct {
		status   int
		response string
	}{
		"unauthorized": {http.StatusUnauthorized, `{"code":401}`},
		"not json":     {http.StatusOK, `oops`},
	} {
		c, _, _ := setup(t, s.status, s.response)
		if _, err := c.AgentSteps(context.Background(), []int64{1}, t0); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
