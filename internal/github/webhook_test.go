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
