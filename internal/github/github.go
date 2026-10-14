// Package github provides the initial execution backend. Installation tokens are
// short lived and refreshed by ghinstallation; no personal access token is used.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"agbredo/internal/core"
	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v84/github"
)

type Backend struct {
	apps    *ghinstallation.AppsTransport
	mu      sync.Mutex
	clients map[int64]*gh.Client
	// clientFor is used by tests without requesting real installation credentials.
	clientFor func(int64) (*gh.Client, error)
}

func New(appID int64, privateKey []byte) (*Backend, error) {
	if appID <= 0 {
		return nil, errors.New("GitHub App ID must be positive")
	}
	apps, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub App private key: %w", err)
	}
	apps.Client = &http.Client{Timeout: 20 * time.Second}
	return &Backend{apps: apps, clients: make(map[int64]*gh.Client)}, nil
}

func (b *Backend) client(id int64) (*gh.Client, error) {
	if b.clientFor != nil {
		return b.clientFor(id)
	}
	if id <= 0 {
		return nil, errors.New("GitHub installation ID must be positive")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c := b.clients[id]; c != nil {
		return c, nil
	}
	tr := ghinstallation.NewFromAppsTransport(b.apps, id)
	tr.Client = &http.Client{Timeout: 20 * time.Second}
	c := gh.NewClient(&http.Client{Transport: tr, Timeout: 30 * time.Second})
	b.clients[id] = c
	return c, nil
}

func convert(r *gh.WorkflowRun, workflow string) core.WorkflowRun {
	if r.GetPath() != "" {
		workflow = path.Base(strings.SplitN(r.GetPath(), "@", 2)[0])
	}
	return core.WorkflowRun{Attempt: int64(r.GetRunAttempt()), ID: r.GetID(), URL: r.GetHTMLURL(), Title: r.GetDisplayTitle(), Status: r.GetStatus(), Conclusion: r.GetConclusion(), Workflow: workflow, Ref: r.GetHeadBranch(), Actor: r.GetActor().GetLogin(), TriggeringActor: r.GetTriggeringActor().GetLogin()}
}

func (b *Backend) GetRun(ctx context.Context, a core.Agent, id int64) (core.WorkflowRun, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return core.WorkflowRun{}, err
	}
	r, _, err := c.Actions.GetWorkflowRunByID(ctx, a.Owner, a.Repository, id)
	if err != nil {
		return core.WorkflowRun{}, fmt.Errorf("get GitHub workflow run: %w", err)
	}
	workflow := ""
	if r.GetPath() == "" {
		w, _, err := c.Actions.GetWorkflowByID(ctx, a.Owner, a.Repository, r.GetWorkflowID())
		if err != nil {
			return core.WorkflowRun{}, fmt.Errorf("get GitHub workflow identity: %w", err)
		}
		workflow = path.Base(w.GetPath())
	}
	return convert(r, workflow), nil
}

func (b *Backend) GetRunAttempt(ctx context.Context, a core.Agent, id int64, attempt int64) (core.WorkflowRun, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return core.WorkflowRun{}, err
	}
	r, _, err := c.Actions.GetWorkflowRunAttempt(ctx, a.Owner, a.Repository, id, int(attempt), nil)
	if err != nil {
		return core.WorkflowRun{}, fmt.Errorf("get GitHub workflow attempt: %w", err)
	}
	workflow := ""
	if r.GetPath() == "" {
		w, _, e := c.Actions.GetWorkflowByID(ctx, a.Owner, a.Repository, r.GetWorkflowID())
		if e != nil {
			return core.WorkflowRun{}, e
		}
		workflow = path.Base(w.GetPath())
	}
	return convert(r, workflow), nil
}

func VerifyWebhook(secret string, body []byte, signature string) bool {
	if secret == "" || !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
