package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// gateSkip is the jobs of an audit run that its gate skipped: the gate step
// passed, the later steps were skipped. Post steps jump in number.
func gateSkip() []Job {
	started := time.Date(2026, 10, 15, 7, 8, 2, 0, time.UTC)
	return []Job{{ID: 501, Name: "audit / audit", Status: "completed", Conclusion: "success", StartedAt: started, CompletedAt: started.Add(12 * time.Second),
		RunnerName: "GitHub Actions 7", URL: "https://github.com/acme/agent/actions/runs/777/job/501",
		Steps: []Step{
			{Number: 1, Name: "Set up job", Status: "completed", Conclusion: "success", StartedAt: started, CompletedAt: started.Add(time.Second)},
			{Number: 2, Name: "Gate", Status: "completed", Conclusion: "success", StartedAt: started.Add(time.Second), CompletedAt: started.Add(3 * time.Second)},
			{Number: 3, Name: "Audit with Claude", Status: "completed", Conclusion: "skipped"},
			{Number: 4, Name: "Open a PR with the fixes", Status: "completed", Conclusion: "skipped"},
			{Number: 28, Name: "Post Gate", Status: "completed", Conclusion: "success", StartedAt: started.Add(11 * time.Second), CompletedAt: started.Add(12 * time.Second)},
		}}}
}

func TestLimitJobs(t *testing.T) {
	if got := limitJobs(nil); got == nil || len(got) != 0 {
		t.Fatalf("no jobs must be an empty list, not unknown: %#v", got)
	}
	if got := limitJobs(gateSkip()); !reflect.DeepEqual(got, gateSkip()) {
		t.Fatalf("changed jobs within the limits: %#v", got)
	}
	many := make([]Job, MaxJobs+3)
	for i := range many {
		many[i] = Job{ID: int64(i), Name: fmt.Sprint("job ", i)}
	}
	many[0].Steps = make([]Step, MaxSteps+7)
	for i := range many[0].Steps {
		many[0].Steps[i] = Step{Number: int64(i + 1)}
	}
	got := limitJobs(many)
	if len(got) != MaxJobs || got[MaxJobs-1].ID != MaxJobs-1 || len(got[0].Steps) != MaxSteps || got[0].Steps[MaxSteps-1].Number != MaxSteps {
		t.Fatalf("%d jobs, %d steps", len(got), len(got[0].Steps))
	}
	// Names are cut on a character; a cut never splits one.
	long := strings.Repeat("é", MaxJobText+10)
	j := limitJobs([]Job{{Name: long, RunnerName: long, URL: "javascript:alert(1)", Steps: []Step{{Name: "a" + long, Conclusion: "bad\xffbyte"}}}})[0]
	for _, s := range []string{j.Name, j.RunnerName, j.Steps[0].Name} {
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) != MaxJobText {
			t.Fatalf("cut %q (%d characters)", s, utf8.RuneCountInString(s))
		}
	}
	if j.Steps[0].Conclusion != "badbyte" || j.URL != "" {
		t.Fatalf("%q %q", j.Steps[0].Conclusion, j.URL)
	}
}

// Stored runs keep "no jobs" apart from "unknown", and older runs load as unknown.
func TestJobsJSONKeepsEmptyApartFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		jobs []Job
		key  bool
	}{{nil, false}, {[]Job{}, true}, {gateSkip(), true}} {
		data, err := json.Marshal(Run{Jobs: tc.jobs})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"jobs"`) != tc.key {
			t.Fatalf("%s", data)
		}
		var back Run
		if err = json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if (back.Jobs == nil) != (tc.jobs == nil) || !reflect.DeepEqual(back.Jobs, tc.jobs) {
			t.Fatalf("%#v became %#v", tc.jobs, back.Jobs)
		}
	}
	// Missing times are not stored.
	data, _ := json.Marshal(gateSkip()[0].Steps[2])
	if strings.Contains(string(data), "started_at") || strings.Contains(string(data), "completed_at") {
		t.Fatalf("%s", data)
	}
}

func TestJobsRetriesBackOff(t *testing.T) {
	var j jobsRetries
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	if !j.due("r", at) {
		t.Fatal("a run never asked is due")
	}
	j.failed("r", at)
	if j.due("r", at.Add(59*time.Second)) || !j.due("r", at.Add(time.Minute)) {
		t.Fatal("first delay")
	}
	j.failed("r", at)
	if j.due("r", at.Add(119*time.Second)) || !j.due("r", at.Add(2*time.Minute)) {
		t.Fatal("second delay")
	}
	for range 20 {
		j.failed("r", at)
	}
	if !j.due("r", at.Add(jobsMaxDelay)) {
		t.Fatal("the delay grows past its cap")
	}
	j.succeeded("r")
	if !j.due("r", at) {
		t.Fatal("not forgotten")
	}
	// Runs past the window are forgotten.
	j.failed("old", at)
	j.failed("new", at.Add(jobsWindow+2*time.Hour))
	if _, ok := j.runs["old"]; ok {
		t.Fatal("kept a run past the window")
	}
}

// finishedNow is a workflow_run of a run attempt that GitHub has just finished.
func finishedNow(id int64, conclusion string) WorkflowRun {
	gh := auditRun(id, "completed", conclusion)
	gh.UpdatedAt = time.Now()
	return gh
}

// workPass is one pass of the worker (see Work): the poll, then the jobs.
func workPass(ctx context.Context, s *Service) error {
	if err := s.Reconcile(ctx); err != nil {
		return err
	}
	return s.FetchJobs(ctx)
}

// makeDue lets the next pass ask GitHub again for a run whose last call failed.
func makeDue(s *Service, runID string) {
	s.jobsRetries.mu.Lock()
	defer s.jobsRetries.mu.Unlock()
	r := s.jobsRetries.runs[runID]
	r.next = time.Time{}
	s.jobsRetries.runs[runID] = r
}

func (f *fakeBackend) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.jobCalls }
func (f *fakeBackend) putJobs(id int64, jobs []Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[id] = jobs
}

// A finished run gets its jobs once. A failed call leaves them unknown; a
// later pass asks again, after a delay.
func TestDatabaseFinishedRunGetsItsJobsOnce(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	id := attemptRunID(a.ID, 777, 1)
	if err = webhook(s, "requested", "acme/agent", auditRun(777, "queued", "")); err != nil {
		t.Fatal(err)
	}
	// A run that has not finished is not asked.
	if err = workPass(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 0 {
		t.Fatalf("%d calls for a queued run", f.calls())
	}
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "success")); err != nil {
		t.Fatal(err)
	}

	// GitHub does not answer: the jobs stay unknown.
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, id); r.Jobs != nil || f.calls() != 1 {
		t.Fatal(r.Jobs, f.calls())
	}
	// Not asked again at once.
	if err = workPass(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1 {
		t.Fatalf("asked again without waiting: %d calls", f.calls())
	}
	// After the delay, the worker's next pass asks again and records the jobs.
	f.putJobs(777, gateSkip())
	makeDue(s, id)
	if err = workPass(ctx, s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls() != 2 || !reflect.DeepEqual(r.Jobs, gateSkip()) || r.Status != "completed" {
		t.Fatalf("%d calls, %#v", f.calls(), r.Jobs)
	}
	// Once recorded, never asked again, even after a late delivery.
	if err = webhook(s, "late", "acme/agent", finishedNow(777, "success")); err != nil {
		t.Fatal(err)
	}
	if err = workPass(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 2 {
		t.Fatalf("asked again: %d calls", f.calls())
	}
	if again, _ := s.Run(ctx, id); !reflect.DeepEqual(again.Jobs, gateSkip()) {
		t.Fatal("lost the jobs", again.Jobs)
	}
}

// GitHub listing no jobs is an answer: it is kept, and not asked again.
func TestDatabaseNoJobsIsAnAnswer(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	f.putJobs(777, []Job{})
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "failure")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = workPass(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := s.Run(ctx, attemptRunID(a.ID, 777, 1))
	if r.Jobs == nil || len(r.Jobs) != 0 || f.calls() != 1 {
		t.Fatalf("%#v, %d calls", r.Jobs, f.calls())
	}
}

// Runs that finished more than a day ago, runs waiting for approval and runs
// still going are not asked; a pass asks at most jobsPerPass runs, newest first.
func TestDatabaseJobsAreAskedWithinBounds(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	old := auditRun(700, "completed", "success")
	old.UpdatedAt = time.Now().Add(-jobsWindow - time.Hour)
	approval := auditRun(701, "completed", "action_required")
	running := auditRun(702, "in_progress", "")
	for n, gh := range []WorkflowRun{old, approval, running} {
		f.putJobs(gh.ID, gateSkip())
		if err = webhook(s, fmt.Sprint("d", n), "acme/agent", gh); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 0 {
		t.Fatalf("%d calls", f.calls())
	}
	// A run whose deployment's installation is gone keeps failing, and is
	// left alone once the window has passed.
	broken := finishedNow(703, "success")
	if err = webhook(s, "broken", "acme/agent", broken); err != nil {
		t.Fatal(err)
	}
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1 {
		t.Fatalf("%d calls", f.calls())
	}
	brokenID := attemptRunID(a.ID, 703, 1)
	if _, err = s.DB.Exec(ctx, `UPDATE runs SET document = jsonb_set(document, '{github_completed_at}', to_jsonb($2::text)) WHERE id=$1`,
		brokenID, time.Now().Add(-jobsWindow-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	makeDue(s, brokenID)
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1 {
		t.Fatalf("asked past the window: %d calls", f.calls())
	}

	for n := range jobsPerPass + 2 {
		gh := finishedNow(int64(800+n), "success")
		gh.UpdatedAt = gh.UpdatedAt.Add(time.Duration(n) * time.Second)
		f.putJobs(gh.ID, gateSkip())
		if err = webhook(s, fmt.Sprint("new", n), "acme/agent", gh); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1+jobsPerPass {
		t.Fatalf("%d calls in one pass", f.calls())
	}
	// Newest first: the two oldest wait for the next pass.
	for n := range jobsPerPass + 2 {
		r, _ := s.Run(ctx, attemptRunID(a.ID, int64(800+n), 1))
		if (r.Jobs != nil) != (n >= 2) {
			t.Fatalf("run %d: %v", n, r.Jobs != nil)
		}
	}
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls() != 1+jobsPerPass+2 {
		t.Fatalf("%d calls", f.calls())
	}
}

// A list with a job not yet completed is a snapshot: it is not kept, and the
// run is asked again later. Steps are not checked: a cancelled job's steps
// can stay pending.
func TestDatabaseJobsNotYetCompletedAreAskedAgain(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	id := attemptRunID(a.ID, 777, 1)
	f.putJobs(777, []Job{{ID: 1, Name: "audit / audit", Status: "completed", Conclusion: "success"}, {ID: 2, Name: "report", Status: "in_progress"}})
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "cancelled")); err != nil {
		t.Fatal(err)
	}
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, id); r.Jobs != nil || f.calls() != 1 || s.jobsRetries.due(id, now()) {
		t.Fatal("kept a snapshot, or asks again at once", r.Jobs, f.calls())
	}
	final := []Job{{ID: 1, Name: "audit / audit", Status: "completed", Conclusion: "success"},
		{ID: 2, Name: "report", Status: "completed", Conclusion: "cancelled", Steps: []Step{{Number: 1, Name: "Report", Status: "pending"}}}}
	f.putJobs(777, final)
	makeDue(s, id)
	if err = s.FetchJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run(ctx, id); !reflect.DeepEqual(r.Jobs, final) || f.calls() != 2 {
		t.Fatal(r.Jobs, f.calls())
	}
}

// A call that the pass's context cut short does not put the run on hold.
func TestDatabaseJobsCutShortAreNotPutOnHold(t *testing.T) {
	s, f := testService(t)
	a, err := s.SaveDeployment(context.Background(), validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "success")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.beforeList = cancel // GitHub gives no jobs for 777, and the context ends meanwhile
	if err = s.FetchJobs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if id := attemptRunID(a.ID, 777, 1); !s.jobsRetries.due(id, now()) {
		t.Fatal("put on hold")
	}
}

// Runs without GitHub's completion time count from when Agbredo last changed them.
func TestDatabaseJobsWindowWithoutCompletionTime(t *testing.T) {
	s, f := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	id := attemptRunID(a.ID, 777, 1)
	f.putJobs(777, gateSkip())
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "success")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		age   string
		calls int
	}{{"25 hours", 0}, {"1 hour", 1}} {
		if _, err = s.DB.Exec(ctx, "UPDATE runs SET document = document - 'github_completed_at', updated_at = now() - $2::interval WHERE id=$1", id, tc.age); err != nil {
			t.Fatal(err)
		}
		if err = s.FetchJobs(ctx); err != nil {
			t.Fatal(err)
		}
		if f.calls() != tc.calls {
			t.Fatalf("changed %s ago: %d calls", tc.age, f.calls())
		}
	}
	if r, _ := s.Run(ctx, id); !reflect.DeepEqual(r.Jobs, gateSkip()) {
		t.Fatal(r.Jobs)
	}
}

// Two servers that ask at once keep one list: the first saved.
func TestDatabaseJobsOfTwoServersKeepOneList(t *testing.T) {
	s, f := testService(t)
	other := &Service{DB: s.DB, Backend: f}
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	f.putJobs(777, gateSkip())
	if err = webhook(s, "completed", "acme/agent", finishedNow(777, "success")); err != nil {
		t.Fatal(err)
	}
	// While s waits for GitHub, the other server gets and saves the jobs; then
	// GitHub gives s another list.
	var inner error
	f.beforeList = func() {
		f.beforeList = nil
		inner = other.FetchJobs(ctx)
		f.putJobs(777, []Job{{ID: 9, Name: "late", Status: "completed"}})
	}
	if err = s.FetchJobs(ctx); err != nil || inner != nil {
		t.Fatal(err, inner)
	}
	if r, _ := s.Run(ctx, attemptRunID(a.ID, 777, 1)); !reflect.DeepEqual(r.Jobs, gateSkip()) || f.calls() != 2 {
		t.Fatal(r.Jobs, f.calls())
	}
}
