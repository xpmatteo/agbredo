package core

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// auditPR is a pull request that github-actions opened in acme/agent
// (repository ID 42) at the given time, with the given body.
func auditPR(number int64, opened time.Time, body string) PullRequestFacts {
	return PullRequestFacts{RepositoryID: 42, Repository: "acme/agent", Number: number, Title: "Audit fixes", URL: "https://github.com/acme/agent/pull/13",
		Author: "github-actions[bot]", AuthorType: "Bot", HeadRef: "audit/fixes", HeadRepo: "acme/agent", BaseRef: "main", CreatedAt: opened, Body: body}
}

// prWebhook delivers a pull_request webhook with the given action.
func prWebhook(s *Service, delivery, action string, f PullRequestFacts) error {
	return s.PullRequestWebhook(context.Background(), Delivery{ID: delivery, Event: "pull_request", Action: action, Repository: f.Repository, InstallationID: 123}, f)
}

// storedPR reads a pull request with its columns. ok is false when there is none.
func storedPR(t *testing.T, s *Service, id string) (p PullRequest, link string, runID *string, ok bool) {
	t.Helper()
	var doc []byte
	err := s.DB.QueryRow(context.Background(), "SELECT link, run_id, document FROM pull_requests WHERE id=$1", id).Scan(&link, &runID, &doc)
	if err != nil {
		return p, "", nil, false
	}
	p, err = readJSON[PullRequest](context.Background(), s.DB, "SELECT document FROM pull_requests WHERE id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	return p, link, runID, true
}

// prEvents are the pull request events of a run.
func prEvents(t *testing.T, s *Service, runID string) []Event {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var found []Event
	for _, e := range events {
		if e.Type == "github.pull_request" {
			found = append(found, e)
		}
	}
	return found
}

// recentRun is GitHub's description of an attempt of the deployment's
// workflow that started a minute ago; finished says whether it has finished.
func recentRun(id int64, finished bool) WorkflowRun {
	gh := auditRun(id, "in_progress", "")
	if finished {
		gh = auditRun(id, "completed", "success")
	}
	gh.StartedAt = time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	gh.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	return gh
}

// An opened pull request is recorded, linked to its run, and adds an event to
// it. Later deliveries of the same pull request change nothing.
func TestDatabasePullRequestOpenedIsLinkedAndAddsAnEvent(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "wr-1", "acme/agent", auditRun(777, "completed", "success")); err != nil {
		t.Fatal(err)
	}
	runID := attemptRunID(a.ID, 777, 1)
	opened := time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC)
	pr := auditPR(13, opened, "Audit fixes.\n\nAgbredo-Run: 777/1\n")
	if err = prWebhook(s, "pr-1", "opened", pr); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "pr-1"); outcome != DeliveryHandled {
		t.Fatal(outcome)
	}
	p, link, run, ok := storedPR(t, s, "42:13")
	if !ok || link != LinkDeclared || run == nil || *run != runID || p.Link != LinkDeclared || p.RunID != runID || p.Reason != "" || p.Findings != nil ||
		p.Trailer != "Agbredo-Run: 777/1" || p.Title != "Audit fixes" || p.Delivery != "pr-1" || p.LinkedAt.IsZero() || !p.CreatedAt.Equal(opened) {
		t.Fatalf("%v %s %v %#v", ok, link, run, p)
	}
	var owner, repository string
	var number int
	if err = s.DB.QueryRow(ctx, "SELECT owner, repository, number FROM pull_requests WHERE id='42:13'").Scan(&owner, &repository, &number); err != nil ||
		owner != "acme" || repository != "agent" || number != 13 {
		t.Fatal(owner, repository, number, err)
	}
	events := prEvents(t, s, runID)
	if len(events) != 1 || events[0].ID != "github:pull_request:opened:pr-1" || events[0].Action != "opened" || events[0].Resource != "acme/agent#13" ||
		!events[0].Timestamp.Equal(opened) || events[0].Status != "completed" {
		t.Fatalf("%+v", events)
	}
	prs, err := s.RunPullRequests(ctx, runID)
	if err != nil || len(prs) != 1 || prs[0].Number != 13 {
		t.Fatal(prs, err)
	}
	f, err := s.Fleet(ctx)
	if err != nil || len(f.PullRequests[runID]) != 1 || f.PullRequestsSince.IsZero() {
		t.Fatal(f.PullRequests, f.PullRequestsSince, err)
	}

	// The same delivery again: handled before, not applied again.
	if err = prWebhook(s, "pr-1", "opened", pr); err != nil {
		t.Fatal(err)
	}
	// Closed, reopened and other actions are ignored and change nothing.
	// An edit of a linked pull request changes nothing; neither does the
	// same opening sent with a new delivery ID.
	changed := pr
	changed.Title, changed.Body = "Renamed", "Agbredo-Run: 999/1"
	for delivery, action := range map[string]string{"pr-closed": "closed", "pr-reopened": "reopened", "pr-labeled": "labeled", "pr-edited": "edited", "pr-again": "opened"} {
		if err = prWebhook(s, delivery, action, changed); err != nil {
			t.Fatal(err)
		}
		if outcome, _ := deliveryOutcome(t, s, delivery); outcome != DeliveryIgnored {
			t.Fatal(action, outcome)
		}
	}
	if again, link, _, _ := storedPR(t, s, "42:13"); link != LinkDeclared || !reflect.DeepEqual(again, p) {
		t.Fatalf("changed: %s %#v", link, again)
	}
	if n := len(prEvents(t, s, runID)); n != 1 {
		t.Fatalf("%d pull request events", n)
	}
}

// Agbredo records the pull requests of repositories with a live deployment
// (disabled or not), when a bot opened them or they carry a trailer.
func TestDatabasePullRequestsThatAreRecorded(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	disabled := validDeployment()
	disabled.Enabled = false
	if _, err := s.SaveDeployment(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	deleted := validDeployment()
	deleted.Repository = "gone"
	deleted, err := s.SaveDeployment(ctx, deleted)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDeployment(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	opened := time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC)
	byPerson := func(f PullRequestFacts) PullRequestFacts { f.Author, f.AuthorType = "matt", "User"; return f }
	fromFork := func(f PullRequestFacts) PullRequestFacts { f.HeadRepo = "someone/agent"; return f }
	inRepo := func(f PullRequestFacts, repository string) PullRequestFacts { f.Repository = repository; return f }
	for _, c := range []struct {
		name   string
		action string
		facts  PullRequestFacts
		stored bool
		reason string
	}{
		{"bot in a disabled deployment's repository", "opened", auditPR(1, opened, ""), true, ReasonNoRunInWindow},
		{"names ignore case", "opened", inRepo(auditPR(2, opened, ""), "ACME/Agent"), true, ReasonNoRunInWindow},
		{"person with a trailer", "opened", byPerson(auditPR(3, opened, "Agbredo-Run: 777/1")), true, ReasonTrailerByHuman},
		{"person without a trailer", "opened", byPerson(auditPR(4, opened, "")), false, ""},
		{"repository without a deployment", "opened", inRepo(auditPR(5, opened, ""), "acme/other"), false, ""},
		{"repository of a deleted deployment", "opened", inRepo(auditPR(6, opened, ""), "acme/gone"), false, ""},
		{"closed before Agbredo saw it open", "closed", auditPR(7, opened, ""), false, ""},
		{"edit of a pull request Agbredo did not record", "edited", auditPR(8, opened, ""), false, ""},
		{"bot from a fork, without a trailer", "opened", fromFork(auditPR(9, opened, "")), true, ReasonPRFromFork},
	} {
		t.Run(c.name, func(t *testing.T) {
			delivery := "pr-" + c.name
			if err := prWebhook(s, delivery, c.action, c.facts); err != nil {
				t.Fatal(err)
			}
			want := DeliveryIgnored
			if c.stored {
				want = DeliveryHandled
			}
			if outcome, _ := deliveryOutcome(t, s, delivery); outcome != want {
				t.Fatal(outcome)
			}
			p, link, _, ok := storedPR(t, s, "42:"+itoa(c.facts.Number))
			if ok != c.stored || (ok && (link != LinkUnlinked || p.Reason != c.reason)) {
				t.Fatalf("stored %v, link %s, reason %s", ok, link, p.Reason)
			}
		})
	}
	unlinked, err := s.UnlinkedPullRequests(ctx, "Acme", "AGENT", 20)
	if err != nil || len(unlinked) != 4 {
		t.Fatal(len(unlinked), err)
	}
	// The person's pull request has no finding of an unlinked bot pull request.
	for _, p := range unlinked {
		want := []string{FindingUnlinkedBotPR}
		if !p.Bot() {
			want = []string{FindingTrailerMismatch}
		}
		if !reflect.DeepEqual(p.Findings, want) {
			t.Fatal(p.Number, p.Findings)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A pull request may arrive before its run's webhook. Reconcile links it
// once the run is recorded, adds the event once, and leaves alone the
// unlinked pull requests opened more than an hour ago.
func TestDatabasePullRequestBeforeItsRunIsLinkedByReconcile(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	if err = prWebhook(s, "pr-declared", "opened", auditPR(13, opened, "Agbredo-Run: 777/1\n")); err != nil {
		t.Fatal(err)
	}
	if p, link, _, _ := storedPR(t, s, "42:13"); link != LinkUnlinked || p.Reason != ReasonTrailerRunUnknown ||
		!reflect.DeepEqual(p.Findings, []string{FindingTrailerMismatch, FindingUnlinkedBotPR}) {
		t.Fatalf("%s %#v", link, p)
	}
	// Another pull request, without a trailer: no run in its window yet.
	if err = prWebhook(s, "pr-inferred", "opened", auditPR(14, opened, "")); err != nil {
		t.Fatal(err)
	}
	// One opened long ago, unlinked, is not tried again.
	if err = prWebhook(s, "pr-old", "opened", auditPR(15, opened.Add(-2*time.Hour), "Agbredo-Run: 777/1")); err != nil {
		t.Fatal(err)
	}
	// Nothing to link yet: Reconcile writes nothing.
	before, _, _, _ := storedPR(t, s, "42:14")
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _, _, _ := storedPR(t, s, "42:14"); !reflect.DeepEqual(after, before) {
		t.Fatalf("rewritten: %#v", after)
	}

	if err = webhook(s, "wr-777", "acme/agent", recentRun(777, true)); err != nil {
		t.Fatal(err)
	}
	// Run 777 is recorded now. Reconcile links 13 by its trailer (declared)
	// and 14 by time (inferred): 777 is the only run in its window.
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	run777 := attemptRunID(a.ID, 777, 1)
	if p, link, run, _ := storedPR(t, s, "42:13"); link != LinkDeclared || *run != run777 || p.Reason != "" || p.Findings != nil {
		t.Fatalf("%s %#v", link, p)
	}
	if p, link, _, _ := storedPR(t, s, "42:14"); link != LinkInferred || p.RunID != run777 {
		t.Fatalf("one run in the window: %s %#v", link, p)
	}
	// The trailer of 15 names 777 too, but 15 was opened two hours ago: it
	// was tried once, when it arrived, and keeps the reason it got then.
	if p, link, _, _ := storedPR(t, s, "42:15"); link != LinkUnlinked || p.Reason != ReasonTrailerRunUnknown {
		t.Fatalf("an old pull request: %s %#v", link, p)
	}
	events := prEvents(t, s, run777)
	if len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("%+v", events)
	}
	if err = s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(prEvents(t, s, run777)); n != 2 {
		t.Fatalf("%d pull request events", n)
	}
}

// Link and findings are decided at the opening. Without a trailer and with
// two runs in the window, a pull request is unlinked, with both runs as
// candidates. An edit that adds the trailer is ignored and changes nothing.
func TestDatabaseEditedIsIgnored(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{777, 778} {
		if err = webhook(s, "wr-"+itoa(id), "acme/agent", auditRun(id, "in_progress", "")); err != nil {
			t.Fatal(err)
		}
	}
	opened := time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC)
	pr := auditPR(13, opened, "")
	if err = prWebhook(s, "pr-opened", "opened", pr); err != nil {
		t.Fatal(err)
	}
	run777, run778 := attemptRunID(a.ID, 777, 1), attemptRunID(a.ID, 778, 1)
	want := []string{run777, run778}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	before, link, _, _ := storedPR(t, s, "42:13")
	if link != LinkUnlinked || before.Reason != ReasonSeveralRunsInWindow || !reflect.DeepEqual(before.CandidateRuns, want) {
		t.Fatalf("%s %#v", link, before)
	}
	pr.Title, pr.Body = "Audit fixes, with the trailer", "Fixes.\n\nAgbredo-Run: 778/1"
	if err = prWebhook(s, "pr-edited", "edited", pr); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "pr-edited"); outcome != DeliveryIgnored {
		t.Fatal(outcome)
	}
	if after, link, _, _ := storedPR(t, s, "42:13"); link != LinkUnlinked || !reflect.DeepEqual(after, before) {
		t.Fatalf("changed: %s %#v", link, after)
	}
	if n := len(prEvents(t, s, run777)) + len(prEvents(t, s, run778)); n != 0 {
		t.Fatalf("%d pull request events", n)
	}
}

// Deliveries of the same opening and the worker at the same time link a
// pull request once, with one event.
func TestDatabasePullRequestLinkRace(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	a, err := s.SaveDeployment(ctx, validDeployment())
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	pr := auditPR(13, opened, "Agbredo-Run: 777/1")
	if err = prWebhook(s, "pr-opened", "opened", pr); err != nil {
		t.Fatal(err)
	}
	if err = webhook(s, "wr-777", "acme/agent", recentRun(777, false)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for n := range 4 {
		wg.Add(3)
		go func() { defer wg.Done(); errs <- s.Reconcile(ctx) }()
		go func() { defer wg.Done(); errs <- s.LinkPullRequests(ctx) }()
		go func() { defer wg.Done(); errs <- prWebhook(s, "pr-again-"+itoa(int64(n)), "opened", pr) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run777 := attemptRunID(a.ID, 777, 1)
	if _, link, run, _ := storedPR(t, s, "42:13"); link != LinkDeclared || *run != run777 {
		t.Fatal(link, run)
	}
	if n := len(prEvents(t, s, run777)); n != 1 {
		t.Fatalf("%d pull request events", n)
	}
}

// When the database fails, the delivery is indexed as failed; GitHub sends
// it again, and it is recorded.
func TestDatabasePullRequestFailureIsRetried(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	if _, err := s.SaveDeployment(ctx, validDeployment()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION broken_prs() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pull requests are broken'; END; $$;
		CREATE TRIGGER broken_prs BEFORE INSERT OR UPDATE ON pull_requests FOR EACH ROW EXECUTE FUNCTION broken_prs()`); err != nil {
		t.Fatal(err)
	}
	pr := auditPR(13, time.Date(2026, 10, 15, 7, 8, 10, 0, time.UTC), "")
	if err := prWebhook(s, "pr-1", "opened", pr); err == nil {
		t.Fatal("no error")
	}
	if outcome, problem := deliveryOutcome(t, s, "pr-1"); outcome != DeliveryFailed || !strings.Contains(problem, "pull requests are broken") {
		t.Fatal(outcome, problem)
	}
	if _, err := s.DB.Exec(ctx, "DROP TRIGGER broken_prs ON pull_requests; DROP FUNCTION broken_prs()"); err != nil {
		t.Fatal(err)
	}
	if err := prWebhook(s, "pr-1", "opened", pr); err != nil {
		t.Fatal(err)
	}
	if outcome, _ := deliveryOutcome(t, s, "pr-1"); outcome != DeliveryHandled {
		t.Fatal(outcome)
	}
	if _, _, _, ok := storedPR(t, s, "42:13"); !ok {
		t.Fatal("not recorded")
	}
}
