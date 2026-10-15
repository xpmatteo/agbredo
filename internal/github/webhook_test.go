package github

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"agbredo/internal/core"
)

// workflowRunPayload is a workflow_run webhook as GitHub sends it, cut down.
// The referenced workflow's "ref" is the bogus one GitHub sent for a call to
// the tag v1.
const workflowRunPayload = `{
  "action": "completed",
  "workflow_run": {
    "id": 38696780798, "name": "Agent instructions audit", "head_branch": "main",
    "head_sha": "4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a", "path": ".github/workflows/agent-audit.yml",
    "display_title": "Agent instructions audit", "run_number": 12, "event": "schedule",
    "status": "completed", "conclusion": "success", "workflow_id": 1, "run_attempt": 1,
    "html_url": "https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798",
    "created_at": "2026-10-15T07:08:00Z", "updated_at": "2026-10-15T07:08:17Z", "run_started_at": "2026-10-15T07:08:00Z",
    "actor": {"login": "xpmatteo"}, "triggering_actor": {"login": "xpmatteo"},
    "referenced_workflows": [
      {"path": "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@v1", "sha": "0123456789abcdef0123456789abcdef01234567", "ref": "refs/heads/v1"}
    ],
    "repository": {"full_name": "xpmatteo/rpg-interpreter"},
    "head_repository": {"full_name": "xpmatteo/rpg-interpreter"},
    "pull_requests": []
  },
  "workflow": {"id": 1, "path": ".github/workflows/agent-audit.yml"},
  "repository": {"full_name": "xpmatteo/rpg-interpreter"},
  "installation": {"id": 42},
  "sender": {"login": "xpmatteo"}
}`

func TestParseWorkflowRunEvent(t *testing.T) {
	got, err := ParseWorkflowRunEvent([]byte(workflowRunPayload))
	if err != nil {
		t.Fatal(err)
	}
	want := core.WorkflowRun{ID: 38696780798, Attempt: 1, URL: "https://github.com/xpmatteo/rpg-interpreter/actions/runs/38696780798",
		Title: "Agent instructions audit", Status: "completed", Conclusion: "success", Workflow: "agent-audit.yml", Repository: "xpmatteo/rpg-interpreter",
		HeadBranch: "main", HeadSHA: "4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a", Event: "schedule",
		StartedAt: time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 15, 7, 8, 17, 0, time.UTC),
		Actor: "xpmatteo", TriggeringActor: "xpmatteo",
		Referenced: []core.ReferencedWorkflow{{Path: "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml@v1", SHA: "0123456789abcdef0123456789abcdef01234567"}}}
	got.StartedAt, got.UpdatedAt = got.StartedAt.UTC(), got.UpdatedAt.UTC()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

// The event header is not signed. A signed payload of another event, sent
// with the header workflow_run, does not have the shape and is refused.
func TestParseWorkflowRunEventChecksTheShape(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":          `not-json`,
		"empty":             `{}`,
		"no workflow_run":   `{"action":"completed","repository":{"full_name":"acme/agent"}}`,
		"pull_request":      `{"action":"opened","number":13,"pull_request":{"id":1},"repository":{"full_name":"acme/agent"}}`,
		"deployment action": strings.Replace(workflowRunPayload, `"action": "completed"`, `"action": "created"`, 1),
		"no ID":             strings.Replace(workflowRunPayload, `"id": 38696780798,`, ``, 1),
		"no attempt":        strings.Replace(workflowRunPayload, `"run_attempt": 1,`, ``, 1),
		"no path":           strings.Replace(workflowRunPayload, `"path": ".github/workflows/agent-audit.yml",`, ``, 1),
		"no status":         strings.Replace(workflowRunPayload, `"status": "completed",`, ``, 1),
		"other repository": strings.Replace(workflowRunPayload, `"repository": {"full_name": "xpmatteo/rpg-interpreter"},
  "installation"`, `"repository": {"full_name": "evil/repo"},
  "installation"`, 1),
		"no repository": strings.Replace(workflowRunPayload, `"repository": {"full_name": "xpmatteo/rpg-interpreter"},
  "installation"`, `"installation"`, 1),
	} {
		if body == workflowRunPayload {
			t.Fatalf("%s: the test payload did not change", name)
		}
		if _, err := ParseWorkflowRunEvent([]byte(body)); !errors.Is(err, ErrBadPayload) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	// Repository names ignore case.
	body := strings.Replace(workflowRunPayload, `"repository": {"full_name": "xpmatteo/rpg-interpreter"},
  "installation"`, `"repository": {"full_name": "XPMatteo/RPG-Interpreter"},
  "installation"`, 1)
	if got, err := ParseWorkflowRunEvent([]byte(body)); err != nil || got.Repository != "XPMatteo/RPG-Interpreter" {
		t.Fatal(got.Repository, err)
	}
}

// pullRequestPayload is a pull_request webhook as GitHub sends it, cut down:
// the audit pipeline's pull request, with Claude's text above the trailer.
const pullRequestPayload = `{
  "action": "opened",
  "number": 13,
  "pull_request": {
    "id": 1, "number": 13, "state": "open", "title": "Agent instructions audit",
    "html_url": "https://github.com/xpmatteo/rpg-interpreter/pull/13",
    "body": "Fixes.\n\nAgbredo-Run: 1/1\n\nAgbredo-Run: 38696780798/1\n",
    "created_at": "2026-10-15T07:08:10Z", "updated_at": "2026-10-15T07:08:10Z",
    "user": {"login": "github-actions[bot]", "type": "Bot"},
    "head": {"ref": "agent-audit/38696780798", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}},
    "base": {"ref": "main", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}}
  },
  "repository": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"},
  "installation": {"id": 42},
  "sender": {"login": "github-actions[bot]"}
}`

func TestParsePullRequestEvent(t *testing.T) {
	got, err := ParsePullRequestEvent([]byte(pullRequestPayload))
	if err != nil {
		t.Fatal(err)
	}
	want := core.PullRequestFacts{RepositoryID: 99, Repository: "xpmatteo/rpg-interpreter", Number: 13, Title: "Agent instructions audit",
		URL: "https://github.com/xpmatteo/rpg-interpreter/pull/13", Author: "github-actions[bot]", AuthorType: "Bot",
		HeadRef: "agent-audit/38696780798", HeadRepo: "xpmatteo/rpg-interpreter", BaseRef: "main",
		CreatedAt: time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC), Body: "Fixes.\n\nAgbredo-Run: 1/1\n\nAgbredo-Run: 38696780798/1\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
	// A null body, a deleted fork (no head repository) and a base without
	// its ID are accepted.
	edited := strings.NewReplacer(`"body": "Fixes.\n\nAgbredo-Run: 1/1\n\nAgbredo-Run: 38696780798/1\n"`, `"body": null`,
		`"head": {"ref": "agent-audit/38696780798", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}}`, `"head": {"ref": "agent-audit/38696780798", "repo": null}`,
		`"base": {"ref": "main", "repo": {"id": 99, `, `"base": {"ref": "main", "repo": {`).Replace(pullRequestPayload)
	if got, err = ParsePullRequestEvent([]byte(edited)); err != nil || got.Body != "" || got.HeadRepo != "" || got.Number != 13 {
		t.Fatalf("%#v %v", got, err)
	}
}

// The event header is not signed. A signed payload of another event, sent
// with the header pull_request, does not have the shape and is refused.
func TestParsePullRequestEventChecksTheShape(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":        `not-json`,
		"empty":           `{}`,
		"workflow_run":    workflowRunPayload,
		"closed":          strings.Replace(pullRequestPayload, `"action": "opened"`, `"action": "closed"`, 1),
		"edited":          strings.Replace(pullRequestPayload, `"action": "opened"`, `"action": "edited"`, 1),
		"no pull_request": `{"action":"opened","number":13,"repository":{"id":99,"full_name":"acme/agent"}}`,
		"no number":       strings.Replace(pullRequestPayload, `"id": 1, "number": 13,`, `"id": 1,`, 1),
		"another number": strings.Replace(pullRequestPayload, `"number": 13,
  "pull_request"`, `"number": 14,
  "pull_request"`, 1),
		"number too large":      strings.ReplaceAll(pullRequestPayload, `13`, `9999999999`),
		"no user":               strings.Replace(pullRequestPayload, `"user": {"login": "github-actions[bot]", "type": "Bot"},`, ``, 1),
		"no user type":          strings.Replace(pullRequestPayload, `, "type": "Bot"`, ``, 1),
		"no head":               strings.Replace(pullRequestPayload, `"head": {"ref": "agent-audit/38696780798", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}},`, ``, 1),
		"no base":               strings.Replace(pullRequestPayload, `"base": {"ref": "main", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}}`, `"base": null`, 1),
		"base in another repo":  strings.Replace(pullRequestPayload, `"base": {"ref": "main", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}}`, `"base": {"ref": "main", "repo": {"id": 99, "full_name": "evil/repo"}}`, 1),
		"base with another ID":  strings.Replace(pullRequestPayload, `"base": {"ref": "main", "repo": {"id": 99,`, `"base": {"ref": "main", "repo": {"id": 98,`, 1),
		"no created_at":         strings.Replace(pullRequestPayload, `"created_at": "2026-10-15T07:08:10Z", `, ``, 1),
		"no repository ID":      strings.Replace(pullRequestPayload, `"repository": {"id": 99, "full_name"`, `"repository": {"full_name"`, 1),
		"no repository":         strings.Replace(pullRequestPayload, `"repository": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"},`, ``, 1),
		"repository not a pair": strings.Replace(pullRequestPayload, `"repository": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"},`, `"repository": {"id": 99, "full_name": "xpmatteo"},`, 1),
	} {
		if body == pullRequestPayload {
			t.Fatalf("%s: the test payload did not change", name)
		}
		if _, err := ParsePullRequestEvent([]byte(body)); !errors.Is(err, ErrBadPullRequest) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	// Repository names ignore case.
	body := strings.Replace(pullRequestPayload, `"base": {"ref": "main", "repo": {"id": 99, "full_name": "xpmatteo/rpg-interpreter"}}`, `"base": {"ref": "main", "repo": {"id": 99, "full_name": "XPMatteo/RPG-Interpreter"}}`, 1)
	if got, err := ParsePullRequestEvent([]byte(body)); err != nil || got.Repository != "xpmatteo/rpg-interpreter" {
		t.Fatal(got.Repository, err)
	}
}
