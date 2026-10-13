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

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v84/github"
	"rilian/internal/core"
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

func (b *Backend) Dispatch(ctx context.Context, a core.Agent, workflow string, inputs map[string]interface{}) error {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return err
	}
	_, _, err = c.Actions.CreateWorkflowDispatchEventByFileName(ctx, a.Owner, a.Repository, workflow, gh.CreateWorkflowDispatchEventRequest{Ref: a.Ref, Inputs: inputs})
	if err != nil {
		return fmt.Errorf("dispatch GitHub workflow %s: %w", workflow, err)
	}
	return nil
}

func convert(r *gh.WorkflowRun, workflow string) core.WorkflowRun {
	if r.GetPath() != "" {
		workflow = path.Base(strings.SplitN(r.GetPath(), "@", 2)[0])
	}
	return core.WorkflowRun{ID: r.GetID(), URL: r.GetHTMLURL(), Title: r.GetDisplayTitle(), Status: r.GetStatus(), Conclusion: r.GetConclusion(), Workflow: workflow, Ref: r.GetHeadBranch()}
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

// FindRuns uses the unique run-name, rather than assuming dispatch returns a run ID.
func (b *Backend) FindRuns(ctx context.Context, a core.Agent, correlation string, since time.Time) ([]core.WorkflowRun, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return nil, err
	}
	out := []core.WorkflowRun{}
	seen := map[int64]bool{}
	workflows := map[string]bool{}
	for _, workflow := range []string{a.Workflow, a.ContinuationWorkflow} {
		if workflow == "" || workflows[workflow] {
			continue
		}
		workflows[workflow] = true
		opts := &gh.ListWorkflowRunsOptions{Event: "workflow_dispatch", Created: ">=" + since.Add(-time.Minute).UTC().Format(time.RFC3339), ListOptions: gh.ListOptions{PerPage: 100, Page: 1}}
		for {
			runs, resp, err := c.Actions.ListWorkflowRunsByFileName(ctx, a.Owner, a.Repository, workflow, opts)
			if err != nil {
				return nil, fmt.Errorf("list GitHub workflow runs: %w", err)
			}
			for _, r := range runs.WorkflowRuns {
				if r.GetDisplayTitle() == correlation && !seen[r.GetID()] {
					seen[r.GetID()] = true
					out = append(out, convert(r, workflow))
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	return out, nil
}

func issueMarker(marker string) string { return "<!-- rilian-approval:" + marker + " -->" }

func (b *Backend) CreateIssue(ctx context.Context, a core.Agent, p core.Proposal, marker string) (string, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return "", err
	}
	body := p.Body + "\n\n" + issueMarker(marker)
	issue, _, err := c.Issues.Create(ctx, a.Owner, a.Repository, &gh.IssueRequest{Title: gh.Ptr(p.Title), Body: &body})
	if err != nil {
		return "", fmt.Errorf("create approved GitHub issue: %w", err)
	}
	if issue.GetHTMLURL() == "" {
		return "", errors.New("GitHub returned an issue without a URL")
	}
	return issue.GetHTMLURL(), nil
}

// FindIssue traverses the repository API, including closed issues. GitHub search
// indexing is eventually consistent and unsuitable for recovery after an uncertain POST.
func (b *Backend) FindIssue(ctx context.Context, a core.Agent, marker string) (string, error) {
	c, err := b.client(a.InstallationID)
	if err != nil {
		return "", err
	}
	opts := &gh.IssueListByRepoOptions{State: "all", Sort: "created", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100, Page: 1}}
	for {
		issues, resp, err := c.Issues.ListByRepo(ctx, a.Owner, a.Repository, opts)
		if err != nil {
			return "", fmt.Errorf("recover approved GitHub issue: %w", err)
		}
		for _, issue := range issues {
			if !issue.IsPullRequest() && strings.Contains(issue.GetBody(), issueMarker(marker)) {
				return issue.GetHTMLURL(), nil
			}
		}
		if resp.NextPage == 0 {
			return "", nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
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
