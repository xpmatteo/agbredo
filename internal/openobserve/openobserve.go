// Package openobserve asks OpenObserve which Agents ran in a Run.
//
// Each Agent step sets github.step in OTEL_RESOURCE_ATTRIBUTES, so its Claude
// Code log events carry github_step. An Agent ran when it sent at least one
// event.
package openobserve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stream is where OpenObserve keeps OTLP logs when the sender names no stream.
const Stream = "default"

// AgentStep is one Agent in a Run, with the time of its first and last event.
type AgentStep struct {
	RunID      int64
	RunAttempt int
	Step       string
	First      time.Time
	Last       time.Time
}

type Client struct {
	// URL is the API base of the organization, e.g. https://api.openobserve.ai/api/my-org
	URL *url.URL
	// Authorization is the Authorization header of a user who may search, e.g. "Basic ...".
	Authorization string
	HTTP          *http.Client
}

// AgentSteps returns the Agents of the Runs with these run IDs, all attempts.
// Their events come after since.
func (c *Client) AgentSteps(ctx context.Context, runIDs []int64, since time.Time) ([]AgentStep, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, len(runIDs))
	for i, id := range runIDs {
		ids[i] = "'" + strconv.FormatInt(id, 10) + "'"
	}
	sql := `SELECT github_run_id, github_run_attempt, github_step, min(_timestamp) AS first_at, max(_timestamp) AS last_at
		FROM "` + Stream + `" WHERE github_run_id IN (` + strings.Join(ids, ",") + `) AND github_step IS NOT NULL
		GROUP BY github_run_id, github_run_attempt, github_step ORDER BY first_at`
	q, _ := json.Marshal(map[string]any{"query": map[string]any{
		"sql": sql, "start_time": since.UnixMicro(), "end_time": time.Now().UnixMicro(), "from": 0, "size": 1000,
	}})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(c.URL.String(), "/")+"/_search?type=logs", bytes.NewReader(q))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.Authorization)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openobserve search: status %d", resp.StatusCode)
	}
	var r struct {
		Hits []struct {
			RunID   string `json:"github_run_id"`
			Attempt string `json:"github_run_attempt"`
			Step    string `json:"github_step"`
			First   int64  `json:"first_at"`
			Last    int64  `json:"last_at"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("openobserve search: %w", err)
	}
	var steps []AgentStep
	for _, h := range r.Hits {
		runID, err1 := strconv.ParseInt(h.RunID, 10, 64)
		attempt, err2 := strconv.Atoi(h.Attempt)
		if err1 != nil || err2 != nil {
			continue
		}
		steps = append(steps, AgentStep{RunID: runID, RunAttempt: attempt, Step: h.Step,
			First: time.UnixMicro(h.First).UTC(), Last: time.UnixMicro(h.Last).UTC()})
	}
	return steps, nil
}
