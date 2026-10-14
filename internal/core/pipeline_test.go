package core

import (
	"context"
	"reflect"
	"testing"
)

const pipelineSHA = "0123456789abcdef0123456789abcdef01234567"

func TestPipelineOf(t *testing.T) {
	caller := Identity{Repository: "acme/agent", Workflow: "agent.yml", Ref: "refs/heads/main"}
	for _, tc := range []struct {
		name     string
		ref, sha string
		want     *Pipeline
	}{
		{"reusable", "org/agents/.github/workflows/triage.yml@refs/tags/v1", pipelineSHA,
			&Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1", SHA: pipelineSHA}},
		{"same repository", "acme/agent/.github/workflows/triage.yml@refs/heads/main", pipelineSHA,
			&Pipeline{Path: "acme/agent/.github/workflows/triage.yml", Ref: "refs/heads/main", SHA: pipelineSHA}},
		{"ref with @", "org/agents/.github/workflows/triage.yml@refs/heads/feature@x", pipelineSHA,
			&Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/heads/feature@x", SHA: pipelineSHA}},
		{"yaml file", "org/agents/.github/workflows/triage.yaml@refs/tags/v1", "",
			&Pipeline{Path: "org/agents/.github/workflows/triage.yaml", Ref: "refs/tags/v1"}},
		{"no sha", "org/agents/.github/workflows/triage.yml@refs/tags/v1", "",
			&Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1"}},
		{"odd sha is dropped", "org/agents/.github/workflows/triage.yml@refs/tags/v1", "nope",
			&Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1"}},
		// Not a reusable workflow: the caller is the pipeline itself.
		{"direct job", "acme/agent/.github/workflows/agent.yml@refs/heads/main", pipelineSHA, nil},
		{"no claim", "", "", nil},
		{"no ref", "org/agents/.github/workflows/triage.yml", pipelineSHA, nil},
		{"empty ref", "org/agents/.github/workflows/triage.yml@", pipelineSHA, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := caller
			i.JobWorkflowRef, i.JobWorkflowSHA = tc.ref, tc.sha
			if got := PipelineOf(i); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestPipelineDisplay(t *testing.T) {
	p := Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1", SHA: pipelineSHA}
	if p.ShortSHA() != "0123456" || p.URL() != "https://github.com/org/agents/blob/"+pipelineSHA+"/.github/workflows/triage.yml" {
		t.Fatalf("%s %s", p.ShortSHA(), p.URL())
	}
	p.SHA = ""
	if p.ShortSHA() != "" || p.URL() != "https://github.com/org/agents/blob/refs/tags/v1/.github/workflows/triage.yml" {
		t.Fatalf("without sha: %q %s", p.ShortSHA(), p.URL())
	}
}

func TestPipelineVersionsGroupsRunsNewestFirst(t *testing.T) {
	v2 := &Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v2", SHA: pipelineSHA}
	v1 := &Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1"}
	same := *v2
	runs := []Run{{ID: "e", Pipeline: v2}, {ID: "d"}, {ID: "c", Pipeline: &same}, {ID: "b", Pipeline: v1}, {ID: "a"}}
	uses := PipelineVersions(runs)
	if len(uses) != 3 {
		t.Fatalf("%#v", uses)
	}
	if uses[0].Pipeline != v2 || uses[0].Runs != 2 || uses[0].Latest.ID != "e" {
		t.Errorf("v2: %#v", uses[0])
	}
	if uses[1].Pipeline != nil || uses[1].Runs != 2 || uses[1].Latest.ID != "d" {
		t.Errorf("none: %#v", uses[1])
	}
	if uses[2].Pipeline != v1 || uses[2].Runs != 1 || uses[2].Latest.ID != "b" {
		t.Errorf("v1: %#v", uses[2])
	}
	if PipelineVersions(nil) != nil {
		t.Error("no runs, no versions")
	}
}

// The run records the pipeline from the identity of its first token request.
// A direct job records none. Later requests do not change it.
func TestRunRecordsPipelineAndRecentRunsListsThem(t *testing.T) {
	s, f, a, direct := tokenDeployment(t)
	ctx := context.Background()
	direct.JobWorkflowRef, direct.JobWorkflowSHA = "acme/agent/.github/workflows/"+a.Workflow+"@refs/heads/main", pipelineSHA
	if r := startRun(t, s, a.ID, "review", direct); r.Pipeline != nil {
		t.Fatalf("a direct job has no pipeline: %#v", r.Pipeline)
	}
	f.put(WorkflowRun{ID: 778, Attempt: 1, Workflow: a.Workflow, Ref: "main", Status: "in_progress"})
	reusable := direct
	reusable.RunID = 778
	reusable.JobWorkflowRef = "org/agents/.github/workflows/triage.yml@refs/tags/v1"
	want := &Pipeline{Path: "org/agents/.github/workflows/triage.yml", Ref: "refs/tags/v1", SHA: pipelineSHA}
	if r := startRun(t, s, a.ID, "review", reusable); !reflect.DeepEqual(r.Pipeline, want) {
		t.Fatalf("pipeline = %#v", r.Pipeline)
	}
	later := reusable
	later.JobWorkflowRef = "org/agents/.github/workflows/triage.yml@refs/tags/v2"
	if r := startRun(t, s, a.ID, "fix", later); !reflect.DeepEqual(r.Pipeline, want) {
		t.Fatalf("a later request changed the pipeline: %#v", r.Pipeline)
	}
	runs, err := s.RecentRuns(ctx, a.ID, 1)
	if err != nil || len(runs) != 1 || !reflect.DeepEqual(runs[0].Pipeline, want) {
		t.Fatalf("recent runs: %#v %v", runs, err)
	}
	if runs, err = s.RecentRuns(ctx, a.ID, 20); err != nil || len(runs) != 2 {
		t.Fatalf("recent runs: %d %v", len(runs), err)
	}
	if runs, err = s.RecentRuns(ctx, "other", 20); err != nil || len(runs) != 0 {
		t.Fatalf("runs of another deployment: %#v %v", runs, err)
	}
}
