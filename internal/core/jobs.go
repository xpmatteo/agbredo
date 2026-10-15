package core

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Limits on the jobs and steps kept on a run. GitHub may list more; the
// rest are dropped.
const (
	MaxJobs  = 50
	MaxSteps = 100
	// MaxJobText is the most characters kept of a job's or step's name, and
	// of the other texts of a job.
	MaxJobText = 200
	maxJobURL  = 500
)

// Fetching jobs is bounded, to spare the GitHub rate limit of each App
// installation, which the poll and the token path share.
const (
	// jobsWindow: a finished run is asked for its jobs only within this time
	// after it finished. A run whose jobs GitHub never gives (its
	// installation is gone, say) then stops being asked.
	jobsWindow = 24 * time.Hour
	// jobsPerPass is the most runs asked for their jobs in one pass.
	jobsPerPass = 5
	// After a failed call, a run waits jobsFirstDelay before the next one,
	// twice as long after each further failure, up to jobsMaxDelay.
	jobsFirstDelay = time.Minute
	jobsMaxDelay   = time.Hour
)

// jobsRetries remembers, in memory only, the runs whose jobs GitHub did not
// give. A restart forgets them; jobsWindow still ends their retries.
type jobsRetries struct {
	mu   sync.Mutex
	runs map[string]jobsRetry
}

type jobsRetry struct {
	failures int
	first    time.Time // the first failure
	next     time.Time // no call before this
}

// due says whether a run may be asked for its jobs now.
func (j *jobsRetries) due(runID string, at time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.runs[runID]
	return !ok || !at.Before(r.next)
}

func (j *jobsRetries) failed(runID string, at time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.runs == nil {
		j.runs = map[string]jobsRetry{}
	}
	// Forget runs that are past the window.
	for id, r := range j.runs {
		if at.Sub(r.first) > jobsWindow+time.Hour {
			delete(j.runs, id)
		}
	}
	r, ok := j.runs[runID]
	if !ok {
		r.first = at
	}
	r.failures++
	delay := jobsFirstDelay
	for i := 1; i < r.failures && delay < jobsMaxDelay; i++ {
		delay *= 2
	}
	r.next = at.Add(min(delay, jobsMaxDelay))
	j.runs[runID] = r
}

func (j *jobsRetries) succeeded(runID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.runs, runID)
}

// FetchJobs asks GitHub for the jobs and steps of finished runs that have
// none yet, and records them. A run is asked once: a run with jobs, even an
// empty list, is not asked again. The call to GitHub is made outside any
// database transaction. When it fails, the run keeps no jobs, and a later
// pass asks again, after a delay that grows with each failure, as long as
// the run finished less than jobsWindow ago. At most jobsPerPass runs are
// asked per pass, newest first.
//
// GitHub's answer is kept only once every job in it has completed. A list
// with a job still queued or in progress is a snapshot, not the final
// answer: it is not saved, and the run is asked again like after a failed
// call. Steps are not checked: a cancelled job's steps can stay pending.
//
// It returns an error when the database fails, or when ctx ends; a run is
// not put on hold for a call that ctx cut short.
func (s *Service) FetchJobs(ctx context.Context) error {
	// When GitHub finished the run; for runs without that time, when Agbredo
	// last changed it. Two clocks: updated_at is set by the database's now(),
	// the cutoff by the server's. They are close enough for a day's window.
	rows, err := s.DB.Query(ctx, `SELECT document FROM runs
		WHERE status IN ('completed','failed','cancelled') AND document->'jobs' IS NULL
			AND COALESCE((document->>'github_completed_at')::timestamptz, updated_at) > $1
		ORDER BY COALESCE((document->>'github_completed_at')::timestamptz, updated_at) DESC`, now().Add(-jobsWindow))
	if err != nil {
		return err
	}
	runs, err := collect[Run](rows)
	if err != nil {
		return err
	}
	asked := 0
	for _, r := range runs {
		if asked == jobsPerPass {
			break
		}
		if r.GitHubRunID <= 0 || r.RunAttempt <= 0 || !s.jobsRetries.due(r.ID, now()) {
			continue
		}
		asked++
		jobs, e := s.Backend.ListJobs(ctx, r.Deployment, r.GitHubRunID, r.RunAttempt)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.jobsRetries.failed(r.ID, now())
			slog.Warn("list GitHub jobs of run", "run", r.ID, "error", Redact(e.Error()))
			continue
		}
		if !allCompleted(jobs) {
			s.jobsRetries.failed(r.ID, now())
			slog.Warn("GitHub listed jobs of a finished run that have not completed; asking again later", "run", r.ID)
			continue
		}
		s.jobsRetries.succeeded(r.ID)
		if err = s.saveJobs(ctx, r.ID, jobs); err != nil {
			return err
		}
	}
	return nil
}

// allCompleted says whether every job of a list has completed.
func allCompleted(jobs []Job) bool {
	for _, j := range jobs {
		if j.Status != "completed" {
			return false
		}
	}
	return true
}

// saveJobs records the jobs of a run, in a short transaction, if the run has
// finished and has none yet.
func (s *Service) saveJobs(ctx context.Context, runID string, jobs []Job) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r, err := readJSON[Run](ctx, tx, "SELECT document FROM runs WHERE id=$1 FOR UPDATE", runID)
	if err != nil {
		return err
	}
	if r.Jobs != nil || !Terminal(r.Status) {
		return nil
	}
	r.Jobs = limitJobs(jobs)
	if err = saveRun(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// limitJobs keeps at most MaxJobs jobs and MaxSteps steps per job, in
// GitHub's order, and cuts their texts to MaxJobText characters. A job URL
// is kept only if it is a GitHub page. The result is never nil, so that
// "no jobs" stays apart from "unknown".
func limitJobs(jobs []Job) []Job {
	kept := make([]Job, 0, min(len(jobs), MaxJobs))
	for _, j := range jobs[:min(len(jobs), MaxJobs)] {
		j.Name, j.Status, j.Conclusion, j.RunnerName = cutText(j.Name), cutText(j.Status), cutText(j.Conclusion), cutText(j.RunnerName)
		if !strings.HasPrefix(j.URL, "https://github.com/") || len(j.URL) > maxJobURL {
			j.URL = ""
		}
		var steps []Step
		for _, st := range j.Steps[:min(len(j.Steps), MaxSteps)] {
			st.Name, st.Status, st.Conclusion = cutText(st.Name), cutText(st.Status), cutText(st.Conclusion)
			steps = append(steps, st)
		}
		j.Steps = steps
		kept = append(kept, j)
	}
	return kept
}

// cutText keeps at most MaxJobText characters, and drops bytes that are not UTF-8.
func cutText(s string) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= MaxJobText {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxJobText {
			return s[:i]
		}
		n++
	}
	return s
}
