package core

import (
	"context"
	"regexp"
	"strings"
)

// Pipeline is the pipeline a run used and its version (see
// docs/00-glossary.md). GitHub supplies both in the job's OIDC token, so the
// pipeline cannot fake them. They are only recorded and shown: the identity
// of a run is always its caller workflow.
type Pipeline struct {
	// Path of the reusable workflow, e.g. org/agents/.github/workflows/triage.yml.
	Path string `json:"path"`
	// Ref of the central repository it ran from, e.g. refs/tags/v1.
	Ref string `json:"ref"`
	// SHA is the commit it ran from. Empty if GitHub did not give one.
	SHA string `json:"sha,omitempty"`
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// PipelineOf reads the pipeline from a job's OIDC identity. It is nil when the
// job ran no reusable workflow: then job_workflow_ref is the caller workflow
// itself, and there is no pipeline to record.
func PipelineOf(i Identity) *Pipeline {
	caller := i.Repository + "/.github/workflows/" + i.Workflow + "@" + i.Ref
	if i.JobWorkflowRef == "" || i.JobWorkflowRef == caller {
		return nil
	}
	at := strings.LastIndex(i.JobWorkflowRef, "@")
	if at <= 0 || at == len(i.JobWorkflowRef)-1 {
		return nil
	}
	p := &Pipeline{Path: i.JobWorkflowRef[:at], Ref: i.JobWorkflowRef[at+1:]}
	if commitSHA.MatchString(i.JobWorkflowSHA) {
		p.SHA = i.JobWorkflowSHA
	}
	return p
}

// ShortSHA is the first 7 characters of the commit, as GitHub shows it.
func (p Pipeline) ShortSHA() string {
	if len(p.SHA) < 7 {
		return p.SHA
	}
	return p.SHA[:7]
}

// URL is the pipeline's file on GitHub at the commit it ran from, or at its
// ref when the commit is unknown.
func (p Pipeline) URL() string {
	owner, rest, ok := strings.Cut(p.Path, "/")
	if !ok {
		return ""
	}
	repository, file, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	at := p.SHA
	if at == "" {
		at = p.Ref
	}
	return "https://github.com/" + owner + "/" + repository + "/blob/" + at + "/" + file
}

// PipelineUse is one pipeline version seen among some runs of a deployment.
type PipelineUse struct {
	// Pipeline is nil for the runs that recorded none.
	Pipeline *Pipeline
	Runs     int
	// Latest is the most recent of these runs.
	Latest Run
}

// PipelineVersions groups runs, newest first, by the pipeline version they
// used. The group of the newest run comes first.
func PipelineVersions(runs []Run) []PipelineUse {
	var uses []PipelineUse
	index := map[Pipeline]int{}
	for _, r := range runs {
		var key Pipeline // the zero key groups the runs without a pipeline
		if r.Pipeline != nil {
			key = *r.Pipeline
		}
		i, ok := index[key]
		if !ok {
			i = len(uses)
			index[key] = i
			uses = append(uses, PipelineUse{Pipeline: r.Pipeline, Latest: r})
		}
		uses[i].Runs++
	}
	return uses
}

// RecentRuns is the newest runs of a deployment, at most limit of them.
func (s *Service) RecentRuns(ctx context.Context, deploymentID string, limit int) ([]Run, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE agent_id=$1 ORDER BY created_at DESC, id LIMIT $2", deploymentID, limit)
	if err != nil {
		return nil, err
	}
	return collect[Run](rows)
}
