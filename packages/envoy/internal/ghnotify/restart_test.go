package ghnotify

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/metrics"
)

// TestRestartGap stops a poller, lets 30 minutes of changes pass, and starts a new one with the
// default 1-hour lookback on the same GitHub and the same stream (A7, unit half). The new poller,
// knowing nothing, reads the last hour again and publishes everything in it; the stream's message
// id, DedupeKey:Topic, drops what the first poller already stored. Every comment, review and issue
// event, the gap's included, is stored once, and each poller's events_total counts only what the
// stream stored for it.
func TestRestartGap(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(t0)
	f := newFakeGitHub(t, clock)
	pub := newFakePublisher()

	f.addThread("5", "acme", "widgets", "PullRequest", 5, t0.Add(-5*time.Minute))
	f.addIssue("acme", "widgets", 5, true)
	f.addComment("acme", "widgets", 5, 9501, "maintainer", "First look.", t0.Add(-20*time.Minute))
	f.addReview("acme", "widgets", 5, 7501, "reviewer", "CHANGES_REQUESTED", t0.Add(-10*time.Minute))
	f.addEvent("acme", "widgets", 5, 15501, "closed", t0.Add(-5*time.Minute))

	regA := metrics.New()
	a := newTestPoller(t, f, pub, &syncBuffer{}, regA, nil)
	a.poll(ctx)
	if pub.storedCount() != 3 {
		t.Fatalf("poller A stored %v, want 3", pub.sourceIDs())
	}

	// Poller A stops. Thirty minutes of changes, on the thread it saw and on one it never did.
	clock.Advance(30 * time.Minute)
	f.addComment("acme", "widgets", 5, 9502, "contributor", "Fixed.", t0.Add(10*time.Minute))
	f.addEvent("acme", "widgets", 5, 15502, "reopened", t0.Add(20*time.Minute))
	f.addReviewComment("acme", "widgets", 5, 9601, "reviewer", "Still off by one.", t0.Add(25*time.Minute))
	f.bumpThread("5", t0.Add(25*time.Minute))
	f.addThread("6", "acme", "widgets", "Issue", 6, t0.Add(15*time.Minute))
	f.addIssue("acme", "widgets", 6, false)
	f.addComment("acme", "widgets", 6, 9602, "reporter", "Same here.", t0.Add(15*time.Minute))

	regB := metrics.New()
	b := newTestPoller(t, f, pub, &syncBuffer{}, regB, nil)
	b.poll(ctx)

	pr5 := "notifications.github.acme.widgets.pr.5"
	want := map[string]string{
		"notifications.issue_comment.9501":               pr5 + ".comment",
		"notifications.pull_request_review.7501":         pr5 + ".review",
		"notifications.pull_request.event.15501":         pr5,
		"notifications.issue_comment.9502":               pr5 + ".comment",
		"notifications.pull_request.event.15502":         pr5,
		"notifications.pull_request_review_comment.9601": pr5 + ".comment",
		"notifications.issue_comment.9602":               "notifications.github.acme.widgets.issue.6.comment",
	}
	if pub.storedCount() != len(want) {
		t.Fatalf("stored %v, want each of %d changes once", pub.sourceIDs(), len(want))
	}
	for sourceID, topic := range want {
		if _, ok := pub.get(sourceID, topic); !ok {
			t.Fatalf("no envelope for %s on %s; stored %v", sourceID, topic, pub.sourceIDs())
		}
	}
	if pub.duplicateCount() != 3 {
		t.Fatalf("the stream dropped %d repeats, want poller A's 3", pub.duplicateCount())
	}
	if got := metricSum(t, regA, "envoy_github_notifications_events_total"); got != 3 {
		t.Fatalf("poller A counted %d events, want 3", got)
	}
	if got := metricSum(t, regB, "envoy_github_notifications_events_total"); got != 4 {
		t.Fatalf("poller B counted %d events, want the gap's 4", got)
	}
}
