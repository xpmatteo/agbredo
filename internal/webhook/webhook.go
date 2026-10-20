// Package webhook receives GitHub workflow_run webhooks and stores the
// ones of Pipeline runs.
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
	"strings"
	"time"

	"agbredo/internal/store"
)

type Inserter interface {
	InsertEvent(ctx context.Context, e store.Event) error
}

type Handler struct {
	Secret []byte
	Store  Inserter
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, h.Secret)
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want)) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "workflow_run" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	e, err := Parse(r.Header.Get("X-GitHub-Delivery"), body)
	if err != nil {
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
