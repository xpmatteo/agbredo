package core

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindTrailer(t *testing.T) {
	for _, c := range []struct {
		name, body   string
		text         string
		run, attempt int64
		found        bool
	}{
		{"none", "Fixes the audit findings.", "", 0, 0, false},
		{"empty body (null)", "", "", 0, 0, false},
		{"pipeline's", "Audit fixes.\n\nAgbredo-Run: 38696780798/1\n", "Agbredo-Run: 38696780798/1", 38696780798, 1, true},
		{"last line, no newline", "Audit fixes.\nAgbredo-Run: 38696780798/2", "Agbredo-Run: 38696780798/2", 38696780798, 2, true},
		{"CRLF and spaces", "Audit fixes.\r\nAgbredo-Run: 5/1  \r\n", "Agbredo-Run: 5/1", 5, 1, true},
		{"the last one wins", "Agbredo-Run: 1/1\nClaude wrote this.\nAgbredo-Run: 2/1\n\nAgbredo-Run: 3/4\n", "Agbredo-Run: 3/4", 3, 4, true},
		{"not at the start of a line", "See Agbredo-Run: 1/1\n", "", 0, 0, false},
		{"text after the numbers", "Agbredo-Run: 1/1 please\n", "", 0, 0, false},
		{"a fake after the real one is not a trailer", "Agbredo-Run: 7/1\nAgbredo-Run: 8/1 fake\n", "Agbredo-Run: 7/1", 7, 1, true},
		{"numbers that do not fit", "Agbredo-Run: 99999999999999999999/1\n", "Agbredo-Run: 99999999999999999999/1", 0, 1, true},
		{"lower case", "agbredo-run: 1/1\n", "", 0, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			text, run, attempt, found := FindTrailer(c.body)
			if text != c.text || run != c.run || attempt != c.attempt || found != c.found {
				t.Fatalf("got %q %d %d %v", text, run, attempt, found)
			}
		})
	}
	// A trailer line is cut like other texts.
	long := "Agbredo-Run: " + strings.Repeat("9", 300) + "/1"
	if text, run, _, found := FindTrailer(long); !found || run != 0 || len(text) != MaxJobText {
		t.Fatal(len(text), run, found)
	}
}

func TestNewPullRequest(t *testing.T) {
	opened := time.Date(2026, 10, 15, 7, 8, 10, 0, time.FixedZone("CEST", 2*3600))
	f := PullRequestFacts{RepositoryID: 42, Repository: "acme/agent", Number: 13, Title: strings.Repeat("t", 300), URL: "https://github.com/acme/agent/pull/13",
		Author: "github-actions[bot]", AuthorType: "Bot", HeadRef: "audit/fixes", HeadRepo: "acme/agent", BaseRef: "main", CreatedAt: opened,
		Body: "Fixes.\n\nAgbredo-Run: 777/1\n"}
	p := newPullRequest(f, "delivery-1")
	if p.ID != "42:13" || p.Owner != "acme" || p.Repository != "agent" || p.Number != 13 || len(p.Title) != MaxJobText || p.URL != f.URL ||
		p.Author != "github-actions[bot]" || !p.Bot() || p.HeadRef != "audit/fixes" || p.HeadRepo != "acme/agent" || p.BaseRef != "main" ||
		!p.CreatedAt.Equal(opened) || p.CreatedAt.Location() != time.UTC || p.Trailer != "Agbredo-Run: 777/1" || p.TrailerRunID != 777 || p.TrailerAttempt != 1 ||
		p.Link != LinkUnlinked || p.Delivery != "delivery-1" || !p.recorded() {
		t.Fatalf("%#v", p)
	}
	f.URL = "javascript:alert(1)"
	if p = newPullRequest(f, ""); p.URL != "" {
		t.Fatal("kept a link that is not a GitHub page", p.URL)
	}
	// A person's pull request is recorded only with a trailer.
	f.AuthorType, f.Body = "User", ""
	if newPullRequest(f, "").recorded() {
		t.Fatal("recorded a person's pull request without a trailer")
	}
	f.Body = "Agbredo-Run: 777/1"
	if !newPullRequest(f, "").recorded() {
		t.Fatal("did not record a person's pull request with a trailer")
	}
}

// linkStart is when GitHub started the runs of the linking tests.
var linkStart = time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)

// linkRun is a run of acme/agent: GitHub run id, attempt 1, started at
// linkStart, finished after the given time (0: still running).
func linkRun(id string, githubID int64, finishedAfter time.Duration) Run {
	r := Run{ID: id, GitHubRunID: githubID, RunAttempt: 1, Status: "running", GitHubStartedAt: linkStart, CreatedAt: linkStart.Add(5 * time.Second),
		Deployment: Deployment{Owner: "acme", Repository: "agent", Workflow: "audit.yml"}}
	if finishedAfter > 0 {
		r.Status, r.GitHubCompletedAt = "completed", linkStart.Add(finishedAfter)
	}
	return r
}

// botPR is a pull request of acme/agent by github-actions, opened at the
// given time after linkStart, with the given body.
func botPR(openedAfter time.Duration, body string) PullRequest {
	return newPullRequest(PullRequestFacts{RepositoryID: 42, Repository: "acme/agent", Number: 13, Title: "Audit fixes", Author: "github-actions[bot]",
		AuthorType: "Bot", HeadRef: "audit/fixes", HeadRepo: "acme/agent", BaseRef: "main", CreatedAt: linkStart.Add(openedAfter), Body: body}, "d1")
}

func TestLinkPullRequest(t *testing.T) {
	const trailer = "Audit fixes.\n\nAgbredo-Run: 777/1\n"
	audit := linkRun("run-audit", 777, 17*time.Second)
	other := linkRun("run-other", 888, 0)
	elsewhere := linkRun("run-elsewhere", 999, 0)
	elsewhere.Deployment.Repository = "other"
	waiting := linkRun("run-waiting", 555, 0)
	waiting.Status, waiting.AwaitingApproval = "queued", true
	old := linkRun("run-old", 444, 0)
	old.Status = "completed" // finished before Agbredo kept completion times
	fork := botPR(10*time.Second, trailer)
	fork.HeadRepo = "someone/agent"
	deletedFork := botPR(10*time.Second, trailer)
	deletedFork.HeadRepo = ""
	noTrailerFork := botPR(10*time.Second, "")
	noTrailerFork.HeadRepo = "someone/agent"
	noTrailerDeleted := botPR(10*time.Second, "")
	noTrailerDeleted.HeadRepo = ""
	human := botPR(10*time.Second, trailer)
	human.Author, human.AuthorType = "matt", "User"
	humanNoTrailer := human
	humanNoTrailer.Trailer = ""
	otherAttempt := botPR(10*time.Second, "Agbredo-Run: 777/2")
	upperCase := botPR(10*time.Second, trailer)
	upperCase.Owner, upperCase.Repository, upperCase.HeadRepo = "ACME", "Agent", "Acme/AGENT"
	before := linkRun("run-before", 666, 0)
	before.GitHubStartedAt = time.Time{} // only Agbredo's time: created 5 s after linkStart
	for _, c := range []struct {
		name string
		pr   PullRequest
		runs []Run
		want Link
	}{
		{"trailer ok", botPR(10*time.Second, trailer), []Run{audit, other}, Link{Level: LinkDeclared, RunID: "run-audit"}},
		{"trailer ok, names ignore case", upperCase, []Run{audit}, Link{Level: LinkDeclared, RunID: "run-audit"}},
		{"trailer ok, at the start", botPR(0, trailer), []Run{audit}, Link{Level: LinkDeclared, RunID: "run-audit"}},
		{"trailer ok, 60 s after the end", botPR(77*time.Second, trailer), []Run{audit}, Link{Level: LinkDeclared, RunID: "run-audit"}},
		{"trailer ok, run still running", botPR(time.Hour, "Agbredo-Run: 888/1"), []Run{audit, other}, Link{Level: LinkDeclared, RunID: "run-other"}},
		{"trailer from fork", fork, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerFromFork, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer from a deleted fork", deletedFork, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerFromFork, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer by human", human, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerByHuman, Findings: []string{FindingTrailerMismatch}}},
		{"trailer outside window, after", botPR(78*time.Second, trailer), []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerOutsideWindow, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer outside window, before", botPR(-time.Second, trailer), []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerOutsideWindow, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer of a run without completion time", botPR(10*time.Second, "Agbredo-Run: 444/1"), []Run{old}, Link{Level: LinkUnlinked, Reason: ReasonTrailerOutsideWindow, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer run unknown", botPR(10*time.Second, trailer), []Run{other}, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer run unknown, no runs", botPR(10*time.Second, trailer), nil, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer of another attempt", otherAttempt, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer of a run in another repository", botPR(10*time.Second, "Agbredo-Run: 999/1"), []Run{elsewhere}, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"trailer with numbers that do not fit", botPR(10*time.Second, "Agbredo-Run: 99999999999999999999/1"), []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		// A trailer that fails is never inferred instead, even with one run in the window.
		{"trailer that fails is not inferred", botPR(10*time.Second, "Agbredo-Run: 123/1"), []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonTrailerRunUnknown, Findings: []string{FindingTrailerMismatch, FindingUnlinkedBotPR}}},
		{"no trailer, one run", botPR(10*time.Second, ""), []Run{audit, elsewhere, waiting, old}, Link{Level: LinkInferred, RunID: "run-audit", Findings: []string{FindingMissingTrailer}}},
		{"no trailer, one run that started by Agbredo's time", botPR(6*time.Second, ""), []Run{before}, Link{Level: LinkInferred, RunID: "run-before", Findings: []string{FindingMissingTrailer}}},
		{"no trailer, before Agbredo's time", botPR(4*time.Second, ""), []Run{before}, Link{Level: LinkUnlinked, Reason: ReasonNoRunInWindow, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, two runs", botPR(10*time.Second, ""), []Run{other, audit}, Link{Level: LinkUnlinked, Reason: ReasonSeveralRunsInWindow, Candidates: []string{"run-audit", "run-other"}, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, from a fork", noTrailerFork, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonPRFromFork, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, from a deleted fork", noTrailerDeleted, []Run{audit}, Link{Level: LinkUnlinked, Reason: ReasonPRFromFork, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, from a fork, no run", noTrailerFork, nil, Link{Level: LinkUnlinked, Reason: ReasonPRFromFork, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, no run", botPR(10*time.Second, ""), nil, Link{Level: LinkUnlinked, Reason: ReasonNoRunInWindow, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, runs outside the window", botPR(78*time.Second, ""), []Run{audit, elsewhere, waiting, old}, Link{Level: LinkUnlinked, Reason: ReasonNoRunInWindow, Findings: []string{FindingUnlinkedBotPR}}},
		{"no trailer, only a run waiting for approval", botPR(10*time.Second, ""), []Run{waiting}, Link{Level: LinkUnlinked, Reason: ReasonNoRunInWindow, Findings: []string{FindingUnlinkedBotPR}}},
		{"human without trailer: not recorded", humanNoTrailer, []Run{audit}, Link{Level: LinkUnlinked}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LinkPullRequest(c.pr, c.runs); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %#v\nwant %#v", got, c.want)
			}
		})
	}
	// At most maxCandidates candidate runs are kept.
	var many []Run
	for n := range maxCandidates + 3 {
		many = append(many, linkRun(string(rune('a'+n)), int64(100+n), 0))
	}
	if got := LinkPullRequest(botPR(10*time.Second, ""), many); got.Reason != ReasonSeveralRunsInWindow || len(got.Candidates) != maxCandidates {
		t.Fatalf("%#v", got)
	}
}
