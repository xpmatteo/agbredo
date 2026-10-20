package store

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGBREDO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGBREDO_TEST_DATABASE_URL is not set")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE events"); err != nil {
		t.Fatal(err)
	}
	return s
}

var t0 = time.Date(2026, 10, 15, 7, 8, 0, 0, time.UTC)

func event(delivery string, runID int64, attempt int, action string, updated time.Time) Event {
	e := Event{
		DeliveryID: delivery, Repository: "xpmatteo/rpg-interpreter", RunID: runID, RunAttempt: attempt,
		Action: action, Status: action, WorkflowName: "Agent audit",
		HTMLURL:    "https://github.com/xpmatteo/rpg-interpreter/actions/runs/1",
		HeadBranch: "main", EventName: "schedule", Actor: "xpmatteo",
		RunStartedAt: t0, UpdatedAt: updated,
		Pipelines: []Pipeline{{ID: "xpmatteo/agbredo-pipelines/.github/workflows/audit.yml", Version: "refs/tags/v1"}},
		Payload:   []byte(`{"action":"` + action + `"}`),
	}
	if action == "completed" {
		e.Conclusion = "success"
	}
	return e
}

func insert(t *testing.T, s *Store, events ...Event) {
	t.Helper()
	for _, e := range events {
		if err := s.InsertEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLatestRunsUsesTheLatestUpdatedAt(t *testing.T) {
	s := openTestStore(t)
	// Events arrive out of order: completed first, requested last.
	insert(t, s,
		event("d3", 1, 1, "completed", t0.Add(30*time.Second)),
		event("d2", 1, 1, "in_progress", t0.Add(10*time.Second)),
		event("d1", 1, 1, "requested", t0),
	)
	runs, err := s.LatestRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	got := runs[0]
	if got.DeliveryID != "d3" || got.Status != "completed" || got.Conclusion != "success" || got.State() != "success" {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(got.Pipelines, event("", 0, 0, "", t0).Pipelines) || !got.RunStartedAt.Equal(t0) {
		t.Fatalf("got %+v", got)
	}
}

func TestLatestRunsBreaksTiesByArrival(t *testing.T) {
	s := openTestStore(t)
	insert(t, s, event("d1", 1, 1, "requested", t0), event("d2", 1, 1, "in_progress", t0))
	runs, err := s.LatestRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].DeliveryID != "d2" {
		t.Fatalf("got %+v", runs)
	}
}

func TestLatestRunsOneRowPerRunAttemptNewestFirst(t *testing.T) {
	s := openTestStore(t)
	older := event("a", 1, 1, "completed", t0)
	newer := event("b", 1, 2, "requested", t0.Add(time.Hour))
	newer.RunStartedAt = t0.Add(time.Hour)
	other := event("c", 2, 1, "requested", t0.Add(time.Minute))
	other.RunStartedAt = t0.Add(time.Minute)
	insert(t, s, older, newer, other)
	runs, err := s.LatestRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.DeliveryID)
	}
	if !reflect.DeepEqual(got, []string{"b", "c", "a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	s := openTestStore(t)
	first := event("d1", 1, 1, "requested", t0)
	again := event("d1", 1, 1, "completed", t0.Add(time.Minute))
	insert(t, s, first, again)
	events, err := s.RunEvents(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "requested" {
		t.Fatalf("got %+v", events)
	}
}

func TestRunEventsInOrder(t *testing.T) {
	s := openTestStore(t)
	insert(t, s,
		event("d3", 1, 1, "completed", t0.Add(30*time.Second)),
		event("d1", 1, 1, "requested", t0),
		event("other-attempt", 1, 2, "requested", t0),
		event("d2", 1, 1, "in_progress", t0.Add(10*time.Second)),
	)
	events, err := s.RunEvents(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Action)
		if e.ReceivedAt.IsZero() {
			t.Fatal("received_at not set")
		}
	}
	if !reflect.DeepEqual(got, []string{"requested", "in_progress", "completed"}) {
		t.Fatalf("got %v", got)
	}
	if events[0].Conclusion != "" {
		t.Fatalf("conclusion of a requested event: %q", events[0].Conclusion)
	}
	if events, err = s.RunEvents(context.Background(), 99, 1); err != nil || len(events) != 0 {
		t.Fatalf("unknown run: %v %v", events, err)
	}
}

func TestPing(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestState(t *testing.T) {
	for _, c := range []struct{ status, conclusion, want string }{
		{"queued", "", "queued"},
		{"in_progress", "", "in_progress"},
		{"completed", "failure", "failure"},
	} {
		if got := (Event{Status: c.status, Conclusion: c.conclusion}).State(); got != c.want {
			t.Errorf("%s/%s: got %s, want %s", c.status, c.conclusion, got, c.want)
		}
	}
}
