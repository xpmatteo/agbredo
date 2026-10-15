package core

import (
	"context"
	"regexp"
	"strings"
)

// Pipeline is the pipeline a run used and its version (see
// docs/00-glossary.md). GitHub supplies both: in the job's OIDC token
// (job_workflow_ref, job_workflow_sha) at the run's first telemetry token, or,
// for a run that asked for no token, in the referenced_workflows of its
// workflow_run webhook. Either way the pipeline cannot fake them. A run of a
// pull request event (pull_request, pull_request_review,
// pull_request_review_comment; not pull_request_target) has none: it runs the
// pull request's workflow file, which a fork's author controls. They are only recorded and shown: the identity of
// a run is always its caller workflow.
type Pipeline struct {
	// Path of the reusable workflow, e.g. org/agents/.github/workflows/triage.yml.
	Path string `json:"path"`
	// Ref of the central repository it ran from: refs/tags/v1 from OIDC, or v1
	// as the caller workflow wrote it, from referenced_workflows.
	Ref string `json:"ref"`
	// SHA is the commit it ran from. Empty if GitHub did not give one.
	SHA string `json:"sha,omitempty"`
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// jobWorkflowRef splits job_workflow_ref, path@ref, after the workflow file.
var jobWorkflowRef = regexp.MustCompile(`^(.+?\.ya?ml)@(.+)$`)

// PipelineOf reads the pipeline from a job's OIDC identity. It is nil when the
// job ran no reusable workflow: then job_workflow_ref is the caller workflow
// itself, and there is no pipeline to record.
func PipelineOf(i Identity) *Pipeline {
	caller := i.Repository + "/.github/workflows/" + i.Workflow + "@" + i.Ref
	if i.JobWorkflowRef == "" || i.JobWorkflowRef == caller {
		return nil
	}
	// The ref may itself hold "@", so the path ends at the first ".yml@" or ".yaml@".
	m := jobWorkflowRef.FindStringSubmatch(i.JobWorkflowRef)
	if m == nil {
		return nil
	}
	p := &Pipeline{Path: m[1], Ref: m[2]}
	if commitSHA.MatchString(i.JobWorkflowSHA) {
		p.SHA = i.JobWorkflowSHA
	}
	return p
}

// PipelineOfRun reads the pipeline from the referenced workflows of a GitHub
// workflow run, for a run that has no OIDC identity. It takes the first entry,
// as a run keeps one pipeline. The ref is the text after "@" in the entry's
// path, as the caller workflow wrote it (e.g. v1, not refs/tags/v1). The
// entry's own ref field is not used: it is not reliable.
func PipelineOfRun(gh WorkflowRun) *Pipeline {
	if len(gh.Referenced) == 0 {
		return nil
	}
	first := gh.Referenced[0]
	m := jobWorkflowRef.FindStringSubmatch(first.Path)
	if m == nil {
		return nil
	}
	p := &Pipeline{Path: m[1], Ref: m[2]}
	if commitSHA.MatchString(first.SHA) {
		p.SHA = first.SHA
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
// used. The group of the newest run comes first. A ref from OIDC
// (refs/tags/v1) and the same ref as a caller workflow wrote it (v1) are one
// version, if the commit is the same too. The stored refs are not changed.
func PipelineVersions(runs []Run) []PipelineUse {
	var uses []PipelineUse
	index := map[Pipeline]int{}
	for _, r := range runs {
		var key Pipeline // the zero key groups the runs without a pipeline
		if r.Pipeline != nil {
			key = *r.Pipeline
			key.Ref = shortRef(key.Ref)
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

// shortRef is a ref without its refs/tags/ or refs/heads/ prefix.
func shortRef(ref string) string {
	for _, prefix := range []string{"refs/tags/", "refs/heads/"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			return rest
		}
	}
	return ref
}

// RecentRuns is the newest runs of a deployment, at most limit of them.
func (s *Service) RecentRuns(ctx context.Context, deploymentID string, limit int) ([]Run, error) {
	rows, err := s.DB.Query(ctx, "SELECT document FROM runs WHERE agent_id=$1 ORDER BY created_at DESC, id LIMIT $2", deploymentID, limit)
	if err != nil {
		return nil, err
	}
	return collect[Run](rows)
}
