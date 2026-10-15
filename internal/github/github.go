// Package github provides the initial execution backend. Installation tokens are
// short lived and refreshed by ghinstallation; no personal access token is used.
package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	run := core.WorkflowRun{Attempt: int64(r.GetRunAttempt()), ID: r.GetID(), URL: r.GetHTMLURL(), Title: r.GetDisplayTitle(), Status: r.GetStatus(), Conclusion: r.GetConclusion(), Workflow: workflow,
		Repository: r.GetRepository().GetFullName(), HeadBranch: r.GetHeadBranch(), HeadSHA: r.GetHeadSHA(), Event: r.GetEvent(),
		StartedAt: r.GetRunStartedAt().Time, UpdatedAt: r.GetUpdatedAt().Time, Actor: r.GetActor().GetLogin(), TriggeringActor: r.GetTriggeringActor().GetLogin()}
	for _, w := range r.ReferencedWorkflows {
		// The entry's ref is left out on purpose; see core.ReferencedWorkflow.
		run.Referenced = append(run.Referenced, core.ReferencedWorkflow{Path: w.GetPath(), SHA: w.GetSHA()})
	}
	return run
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

// maxJobPages is the most pages of jobs asked for one run attempt. With 100
// jobs per page, one page is enough for core.MaxJobs; the bound only guards
// against a GitHub that keeps sending small pages.
const maxJobPages = 5

// ListJobs lists the jobs of a run attempt with their steps
// (GET /repos/{owner}/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs),
// 100 per page, and stops once it has core.MaxJobs. If a page fails, it
// returns the error and no jobs. An attempt without jobs gives an empty list.
func (b *Backend) ListJobs(ctx context.Context, a core.Deployment, id, attempt int64) ([]core.Job, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return nil, err
	}
	jobs := []core.Job{}
	opts := &gh.ListOptions{PerPage: 100}
	for range maxJobPages {
		page, resp, err := c.Actions.ListWorkflowJobsAttempt(ctx, a.Owner, a.Repository, id, attempt, opts)
		if err != nil {
			return nil, fmt.Errorf("list GitHub workflow jobs: %w", err)
		}
		for _, j := range page.Jobs {
			if j == nil {
				continue
			}
			jobs = append(jobs, convertJob(j))
		}
		if resp.NextPage == 0 || len(jobs) >= core.MaxJobs {
			break
		}
		opts.Page = resp.NextPage
	}
	return jobs, nil
}

// convertJob keeps what Agbredo records of a job. Missing times stay zero.
func convertJob(j *gh.WorkflowJob) core.Job {
	job := core.Job{ID: j.GetID(), Name: j.GetName(), Status: j.GetStatus(), Conclusion: j.GetConclusion(),
		StartedAt: j.GetStartedAt().Time, CompletedAt: j.GetCompletedAt().Time, RunnerName: j.GetRunnerName(), URL: j.GetHTMLURL()}
	for _, st := range j.Steps {
		if st == nil {
			continue
		}
		job.Steps = append(job.Steps, core.Step{Number: st.GetNumber(), Name: st.GetName(), Status: st.GetStatus(), Conclusion: st.GetConclusion(),
			StartedAt: st.GetStartedAt().Time, CompletedAt: st.GetCompletedAt().Time})
	}
	return job
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

// ErrBadPayload says a webhook payload does not have the shape of its event.
var ErrBadPayload = errors.New("invalid workflow_run payload")

// ParseWorkflowRunEvent reads a workflow_run webhook payload. Its signature
// has been checked, so its facts come from GitHub. The X-GitHub-Event header
// is not signed, though, so the payload must have the shape of a workflow_run
// event: one of its actions, a workflow_run object with its ID, attempt,
// workflow path and status, and the same repository as the payload's.
// Otherwise it is ErrBadPayload.
func ParseWorkflowRunEvent(body []byte) (core.WorkflowRun, error) {
	var e gh.WorkflowRunEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return core.WorkflowRun{}, ErrBadPayload
	}
	switch e.GetAction() {
	case "requested", "in_progress", "completed":
	default:
		return core.WorkflowRun{}, ErrBadPayload
	}
	r := e.GetWorkflowRun()
	repository := e.GetRepo().GetFullName()
	owner, name, ok := strings.Cut(repository, "/")
	if r == nil || r.GetID() <= 0 || r.GetRunAttempt() <= 0 || r.GetPath() == "" || r.GetStatus() == "" ||
		!ok || owner == "" || name == "" || !strings.EqualFold(r.GetRepository().GetFullName(), repository) {
		return core.WorkflowRun{}, ErrBadPayload
	}
	run := convert(r, "")
	run.Repository = repository
	if run.Workflow == "" || run.Workflow == "." || run.Workflow == "/" {
		return core.WorkflowRun{}, ErrBadPayload
	}
	return run, nil
}
