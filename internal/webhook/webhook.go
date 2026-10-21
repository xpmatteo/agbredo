// Package webhook receives GitHub webhooks. It stores the workflow_run
// webhooks of Pipeline runs, and the PRs and Issues opened by a Run, which
// we know from the Agbredo-Run marker on the last line of their body.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agbredo/internal/store"
)

type Inserter interface {
	InsertEvent(ctx context.Context, e store.Event) error
	InsertOpened(ctx context.Context, o store.Opened) error
}

type Handler struct {
	Secret []byte
	Store  Inserter
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		log.Printf("reject webhook %s: cannot read body: %v", r.Header.Get("X-GitHub-Delivery"), err)
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, h.Secret)
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want)) {
		log.Printf("reject webhook %s: bad signature", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "workflow_run":
		h.workflowRun(w, r, body)
	case "pull_request", "issues":
		h.opened(w, r, body)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) workflowRun(w http.ResponseWriter, r *http.Request, body []byte) {
	e, err := Parse(r.Header.Get("X-GitHub-Delivery"), body)
	if err != nil {
		log.Printf("reject webhook %s: bad workflow_run payload: %v", r.Header.Get("X-GitHub-Delivery"), err)
		http.Error(w, "bad workflow_run payload", http.StatusBadRequest)
		return
	}
	if len(e.Pipelines) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.Store.InsertEvent(r.Context(), e); err != nil {
		log.Printf("store webhook %s: %v", e.DeliveryID, err)
		http.Error(w, "cannot store event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Parse reads a workflow_run payload. The event has no Pipelines when the
// run is not a Pipeline run.
func Parse(deliveryID string, body []byte) (store.Event, error) {
	var p struct {
		Action      string `json:"action"`
		WorkflowRun struct {
			ID                  int64                  `json:"id"`
			RunAttempt          int                    `json:"run_attempt"`
			Name                string                 `json:"name"`
			Status              string                 `json:"status"`
			Conclusion          string                 `json:"conclusion"`
			HTMLURL             string                 `json:"html_url"`
			HeadBranch          string                 `json:"head_branch"`
			Event               string                 `json:"event"`
			RunStartedAt        time.Time              `json:"run_started_at"`
			UpdatedAt           time.Time              `json:"updated_at"`
			Actor               struct{ Login string } `json:"actor"`
			ReferencedWorkflows []struct {
				Path, SHA, Ref string
			} `json:"referenced_workflows"`
		} `json:"workflow_run"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return store.Event{}, err
	}
	run := p.WorkflowRun
	e := store.Event{
		DeliveryID: deliveryID, Repository: p.Repository.FullName, RunID: run.ID, RunAttempt: run.RunAttempt,
		Action: p.Action, Status: run.Status, Conclusion: run.Conclusion, WorkflowName: run.Name,
		HTMLURL: run.HTMLURL, HeadBranch: run.HeadBranch, EventName: run.Event, Actor: run.Actor.Login,
		RunStartedAt: run.RunStartedAt, UpdatedAt: run.UpdatedAt, Payload: body,
	}
	for _, ref := range run.ReferencedWorkflows {
		id, _, _ := strings.Cut(ref.Path, "@")
		version := ref.Ref
		if version == "" {
			version = ref.SHA
		}
		e.Pipelines = append(e.Pipelines, store.Pipeline{ID: id, Version: version})
	}
	return e, nil
}

func (h *Handler) opened(w http.ResponseWriter, r *http.Request, body []byte) {
	o, ok, err := ParseOpened(r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event"), body)
	if err != nil {
		log.Printf("reject webhook %s: bad %s payload: %v", o.DeliveryID, r.Header.Get("X-GitHub-Event"), err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.Store.InsertOpened(r.Context(), o); err != nil {
		log.Printf("store webhook %s: %v", o.DeliveryID, err)
		http.Error(w, "cannot store event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var marker = regexp.MustCompile(`^Agbredo-Run: (\d+)/(\d+)$`)

// ParseOpened reads a pull_request or issues payload. It is ok only when the
// action is "opened" and the last line of the body is an Agbredo-Run marker.
func ParseOpened(deliveryID, event string, body []byte) (store.Opened, bool, error) {
	type item struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		HTMLURL   string    `json:"html_url"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	}
	var p struct {
		Action      string `json:"action"`
		PullRequest item   `json:"pull_request"`
		Issue       item   `json:"issue"`
		Repository  struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	o := store.Opened{DeliveryID: deliveryID}
	if err := json.Unmarshal(body, &p); err != nil {
		return o, false, err
	}
	it, kind := p.Issue, "issue"
	if event == "pull_request" {
		it, kind = p.PullRequest, "pull_request"
	}
	text := strings.TrimSpace(it.Body)
	m := marker.FindStringSubmatch(strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:]))
	if p.Action != "opened" || m == nil {
		return o, false, nil
	}
	runID, err1 := strconv.ParseInt(m[1], 10, 64)
	attempt, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return o, false, nil
	}
	o.Repository, o.Kind, o.Number, o.Title, o.HTMLURL, o.OpenedAt = p.Repository.FullName, kind, it.Number, it.Title, it.HTMLURL, it.CreatedAt
	o.RunID, o.RunAttempt = runID, attempt
	return o, true, nil
}
