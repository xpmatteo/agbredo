package core

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Agbredo records the pull requests that agents open, from pull_request
// webhooks, and links each to the run that opened it, with a level that says
// how sure the link is. It records only the opening: what people do with the
// pull request later is not followed here.
//
// This file holds the rules, as pure functions. pullrequeststore.go applies
// them to the database.

// Link levels of a pull request.
const (
	// LinkDeclared: the pull request names its run in a valid trailer.
	LinkDeclared = "declared"
	// LinkInferred: no trailer, and exactly one run's time window holds the opening.
	LinkInferred = "inferred"
	// LinkUnlinked: no run, with a reason.
	LinkUnlinked = "unlinked"
)

// Reasons a pull request is unlinked.
const (
	ReasonNoRunInWindow        = "no_run_in_window"
	ReasonSeveralRunsInWindow  = "several_runs_in_window"
	ReasonTrailerRunUnknown    = "trailer_run_unknown"
	ReasonTrailerOutsideWindow = "trailer_outside_window"
	ReasonTrailerFromFork      = "trailer_from_fork"
	ReasonTrailerByHuman       = "trailer_by_human"
)

// Findings: rule breaks, kept on the pull request.
const (
	// FindingMissingTrailer: an inferred link; the pipeline opened a pull request without the trailer.
	FindingMissingTrailer = "missing_trailer"
	// FindingTrailerMismatch: a trailer is present, but the link is not declared.
	FindingTrailerMismatch = "trailer_mismatch"
	// FindingUnlinkedBotPR: a bot's pull request in a watched repository, linked to no run.
	FindingUnlinkedBotPR = "unlinked_bot_pr"
)

const (
	// linkGrace is a margin after GitHub finished a run, in which a pull
	// request may still count as opened by it. A run opens its pull request
	// before it finishes; the margin covers small differences between the
	// times GitHub gives the two.
	linkGrace = 60 * time.Second
	// relinkWindow: an unlinked pull request is tried again while it was
	// opened less than this long ago. Its run's webhook may come after it.
	relinkWindow = time.Hour
	// maxCandidates is the most candidate runs kept on a pull request.
	maxCandidates = 10
	maxPRURL      = 500
)

// PullRequestFacts is what a signed pull_request webhook says of a pull
// request. Only the trailer is kept of the body.
type PullRequestFacts struct {
	// RepositoryID is GitHub's ID of the base repository.
	RepositoryID int64
	// Repository is owner/repository of the base repository, as GitHub spells it.
	Repository string
	Number     int64
	Title      string
	URL        string // html_url
	Author     string // user.login
	// AuthorType is GitHub's user.type: "Bot" for an App or github-actions, "User" for a person.
	AuthorType string
	HeadRef    string
	// HeadRepo is owner/repository of the head branch; empty when GitHub
	// gives none (a deleted fork).
	HeadRepo  string
	BaseRef   string
	CreatedAt time.Time
	// Body may be empty: GitHub sends null for a pull request without one.
	Body string
}

// PullRequest is a pull request as Agbredo records it (pull_requests.document):
// the facts as at its opening, the trailer as found, and the link to its run.
// Later changes to the pull request (merged, closed) are not recorded.
type PullRequest struct {
	// ID is "<repository id>:<number>".
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"html_url,omitempty"`
	Author     string `json:"author"`
	AuthorType string `json:"author_type"`
	HeadRef    string `json:"head_ref"`
	HeadRepo   string `json:"head_repo,omitempty"`
	BaseRef    string `json:"base_ref"`
	// CreatedAt is GitHub's created_at: when the pull request was opened.
	CreatedAt time.Time `json:"created_at"`
	// Trailer is the last Agbredo-Run line of the body, as found; empty when
	// there is none. TrailerRunID and TrailerAttempt are its numbers, 0 when
	// they do not fit.
	Trailer        string `json:"trailer,omitempty"`
	TrailerRunID   int64  `json:"trailer_run_id,omitempty"`
	TrailerAttempt int64  `json:"trailer_run_attempt,omitempty"`
	// Link is declared, inferred or unlinked. RunID is the run it is linked
	// to; empty when unlinked. Also in the table's columns.
	Link  string `json:"link"`
	RunID string `json:"run_id,omitempty"`
	// Reason says why it is unlinked, or why a trailer did not count.
	Reason string `json:"reason,omitempty"`
	// CandidateRuns are the runs whose windows all hold the opening, when
	// there were several.
	CandidateRuns []string `json:"candidate_runs,omitempty"`
	Findings      []string `json:"findings,omitempty"`
	// Delivery is the webhook delivery that first recorded it. The event on
	// the linked run takes its ID from it.
	Delivery   string    `json:"delivery,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	LinkedAt   time.Time `json:"linked_at,omitzero"`
}

// Bot says whether GitHub says the author is a bot.
func (p PullRequest) Bot() bool { return p.AuthorType == "Bot" }

// recorded says whether Agbredo keeps a pull request of a watched
// repository: one opened by a bot, or one that carries a trailer. Other
// pull requests stay only in the raw archive of webhook deliveries.
func (p PullRequest) recorded() bool { return p.Bot() || p.Trailer != "" }

// trailerLine is the trailer the pipeline adds to the body of its pull
// requests: the GitHub run ID and run attempt that opened it.
var trailerLine = regexp.MustCompile(`(?m)^Agbredo-Run: ([0-9]+)/([0-9]+)\s*$`)

// FindTrailer finds the trailer in a pull request's body. Claude writes the
// body above the trailer and may write lines that look like one; the
// pipeline's own is the last line. So the last matching line counts. The
// numbers are 0 when they do not fit.
func FindTrailer(body string) (text string, runID, attempt int64, ok bool) {
	all := trailerLine.FindAllStringSubmatch(body, -1)
	if len(all) == 0 {
		return "", 0, 0, false
	}
	last := all[len(all)-1]
	runID, err := strconv.ParseInt(last[1], 10, 64)
	if err != nil {
		runID = 0
	}
	attempt, err = strconv.ParseInt(last[2], 10, 64)
	if err != nil {
		attempt = 0
	}
	return cutText(strings.TrimSpace(last[0])), runID, attempt, true
}

// newPullRequest is the record of a pull request from a webhook's facts,
// not yet linked. Texts are cut to MaxJobText characters, and a link that is
// not a GitHub page is dropped.
func newPullRequest(f PullRequestFacts, delivery string) PullRequest {
	owner, repository, _ := strings.Cut(f.Repository, "/")
	p := PullRequest{ID: fmt.Sprintf("%d:%d", f.RepositoryID, f.Number), Owner: owner, Repository: repository, Number: f.Number,
		Title: cutText(f.Title), Author: cutText(f.Author), AuthorType: cutText(f.AuthorType), HeadRef: cutText(f.HeadRef),
		HeadRepo: cutText(f.HeadRepo), BaseRef: cutText(f.BaseRef), CreatedAt: f.CreatedAt.UTC(), Link: LinkUnlinked,
		Delivery: delivery, RecordedAt: now()}
	if strings.HasPrefix(f.URL, "https://github.com/") && len(f.URL) <= maxPRURL {
		p.URL = f.URL
	}
	p.Trailer, p.TrailerRunID, p.TrailerAttempt, _ = FindTrailer(f.Body)
	return p
}

// Link is the outcome of linking a pull request to a run.
type Link struct {
	Level string
	// RunID is the linked run; empty when unlinked.
	RunID string
	// Reason is set when the pull request is unlinked.
	Reason string
	// Candidates are the runs whose windows hold the opening, when there are several.
	Candidates []string
	Findings   []string
}

// runWindow is the time in which a run may open a pull request: from when
// GitHub started the attempt (or, without that, when Agbredo first recorded
// the run) to linkGrace after GitHub finished it. A run that has not
// finished has an open end. A finished run without GitHub's completion time
// (recorded before Agbredo kept it) has no known window: ok is false.
func runWindow(r Run) (start, end time.Time, open, ok bool) {
	start = r.GitHubStartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	switch {
	case !r.GitHubCompletedAt.IsZero():
		return start, r.GitHubCompletedAt.Add(linkGrace), false, true
	case Terminal(r.Status):
		return start, time.Time{}, false, false
	default:
		return start, time.Time{}, true, true
	}
}

// inWindow says whether the run's window holds the time t.
func inWindow(r Run, t time.Time) bool {
	start, end, open, ok := runWindow(r)
	return ok && !t.Before(start) && (open || !t.After(end))
}

// LinkPullRequest links a pull request to one of runs. Only runs in the pull
// request's repository count (names ignore case); others are left out.
//
//   - declared: the body has a trailer, the author is a bot, the head branch
//     is in the same repository (not a fork), and the run the trailer names
//     (GitHub run ID and attempt) is in this repository and its window holds
//     the opening.
//   - inferred: no trailer, the author is a bot, and exactly one run's window
//     holds the opening. A run waiting for approval has run nothing, and is
//     not counted.
//   - unlinked: anything else, with a reason.
//
// A trailer that does not give a declared link leaves the pull request
// unlinked; it is never inferred instead. Its own run may simply not be
// recorded yet, and an inferred link, which never changes, could then name
// another run.
//
// A pull request by a person without a trailer is not recorded at all; for
// one, this returns unlinked with no reason and no findings.
func LinkPullRequest(p PullRequest, runs []Run) Link {
	repository := p.Owner + "/" + p.Repository
	var here []Run
	for _, r := range runs {
		if sameRepository(repository, r.Deployment.Owner+"/"+r.Deployment.Repository) {
			here = append(here, r)
		}
	}
	slices.SortFunc(here, func(a, b Run) int { return strings.Compare(a.ID, b.ID) })
	unlinked := func(reason string, findings ...string) Link {
		if p.Bot() {
			findings = append(findings, FindingUnlinkedBotPR)
		}
		return Link{Level: LinkUnlinked, Reason: reason, Findings: findings}
	}
	if p.Trailer != "" {
		switch {
		case !p.Bot():
			return unlinked(ReasonTrailerByHuman, FindingTrailerMismatch)
		case !sameRepository(p.HeadRepo, repository):
			return unlinked(ReasonTrailerFromFork, FindingTrailerMismatch)
		}
		named := false
		for _, r := range here {
			if p.TrailerRunID <= 0 || r.GitHubRunID != p.TrailerRunID || r.RunAttempt != p.TrailerAttempt {
				continue
			}
			named = true
			if inWindow(r, p.CreatedAt) {
				return Link{Level: LinkDeclared, RunID: r.ID}
			}
		}
		if !named {
			return unlinked(ReasonTrailerRunUnknown, FindingTrailerMismatch)
		}
		return unlinked(ReasonTrailerOutsideWindow, FindingTrailerMismatch)
	}
	if !p.Bot() {
		return Link{Level: LinkUnlinked}
	}
	var candidates []string
	for _, r := range here {
		if !r.AwaitingApproval && inWindow(r, p.CreatedAt) {
			candidates = append(candidates, r.ID)
		}
	}
	switch len(candidates) {
	case 0:
		return unlinked(ReasonNoRunInWindow)
	case 1:
		return Link{Level: LinkInferred, RunID: candidates[0], Findings: []string{FindingMissingTrailer}}
	default:
		l := unlinked(ReasonSeveralRunsInWindow)
		l.Candidates = candidates[:min(len(candidates), maxCandidates)]
		return l
	}
}
