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
	// app authenticates as the GitHub App itself (a JWT), not as an
	// installation. Tests replace it.
	app *gh.Client
	// notInstalled holds, per lower-case "owner/repository", until when a 404
	// from the installation lookup is believed.
	notInstalledMu sync.Mutex
	notInstalled   map[string]time.Time
	// now is the clock; tests replace it. Nil means time.Now.
	now func() time.Time
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
	app := gh.NewClient(&http.Client{Transport: apps, Timeout: 30 * time.Second})
	return &Backend{apps: apps, app: app, clients: make(map[int64]*gh.Client)}, nil
}

// notInstalledFor is how long a "not installed" answer is remembered, so that
// runs in a repository without the App do not ask GitHub every time.
const notInstalledFor = 5 * time.Minute

// Installation asks GitHub, as the App, for the App's installation on a
// repository (GET /repos/{owner}/{repo}/installation). A 404 means the App is
// not installed there, and a suspended installation counts as not installed:
// both are core.ErrNotInstalled. A 404 is remembered for notInstalledFor,
// per repository, ignoring case.
func (b *Backend) Installation(ctx context.Context, owner, repository string) (int64, error) {
	key := strings.ToLower(owner + "/" + repository)
	if b.recentlyNotInstalled(key) {
		return 0, core.ErrNotInstalled
	}
	inst, resp, err := b.app.Apps.FindRepositoryInstallation(ctx, owner, repository)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		b.rememberNotInstalled(key)
		return 0, core.ErrNotInstalled
	}
	if err != nil {
		return 0, fmt.Errorf("find GitHub App installation: %w", err)
	}
	if inst.SuspendedAt != nil {
		return 0, core.ErrNotInstalled
	}
	if inst.GetID() <= 0 {
		return 0, errors.New("GitHub returned no installation ID")
	}
	return inst.GetID(), nil
}

func (b *Backend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *Backend) recentlyNotInstalled(key string) bool {
	b.notInstalledMu.Lock()
	defer b.notInstalledMu.Unlock()
	until, ok := b.notInstalled[key]
	if ok && !b.clock().Before(until) {
		delete(b.notInstalled, key)
		return false
	}
	return ok
}

func (b *Backend) rememberNotInstalled(key string) {
	b.notInstalledMu.Lock()
	defer b.notInstalledMu.Unlock()
	if b.notInstalled == nil {
		b.notInstalled = map[string]time.Time{}
	}
	b.notInstalled[key] = b.clock().Add(notInstalledFor)
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

func (b *Backend) GetRun(ctx context.Context, a core.Deployment, id int64) (core.WorkflowRun, error) {
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

func (b *Backend) GetRunAttempt(ctx context.Context, a core.Deployment, id int64, attempt int64) (core.WorkflowRun, error) {
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
