package ghnotify

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	issue7Comment = "notifications.github.acme.widgets.issue.7.comment"
	pr8           = "notifications.github.acme.widgets.pr.8"
)

// pollRequests runs one poll and returns every request it sent.
func pollRequests(ctx context.Context, h *harness) []recordedRequest {
	mark := h.f.mark()
	h.p.poll(ctx)
	return h.f.requestsSince(mark)
}

// listOf is the first page of the notification list among requests: its since and its
// If-Modified-Since are the poller's cursor.
func listOf(t *testing.T, requests []recordedRequest) recordedRequest {
	t.Helper()
	for _, request := range requests {
		if request.path == "/notifications" && request.query.Get("page") == "" {
			return request
		}
	}
	t.Fatalf("no notification list request among %d requests", len(requests))
	return recordedRequest{}
}

func TestPollerFakeGitHub(t *testing.T) {
	ctx := context.Background()

	t.Run("the fake GitHub answers 405 to every method but GET", func(t *testing.T) {
		h := newHarness(t, nil)
		resp, err := h.f.srv.Client().Post(h.f.srv.URL+"/notifications", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST answered %d, want 405", resp.StatusCode)
		}
		if others := h.f.takeOthers(); len(others) != 1 || others[0].method != http.MethodPost {
			t.Fatalf("recorded %v, want the one POST", others)
		}
	})

	t.Run("a 200 publishes each new comment, review and state change once", func(t *testing.T) {
		h := newHarness(t, nil)
		f := h.f
		f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-10*time.Minute))
		f.addIssue("acme", "widgets", 7, false)
		f.addComment("acme", "widgets", 7, 9000, "reporter", "Filed long ago.", t0.Add(-3*time.Hour))
		f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-30*time.Minute))
		f.addComment("acme", "widgets", 7, 9002, "reporter", "Thanks.", t0.Add(-20*time.Minute))
		f.addThread("2", "acme", "widgets", "PullRequest", 8, t0.Add(-5*time.Minute))
		f.addIssue("acme", "widgets", 8, true)
		f.addComment("acme", "widgets", 8, 9003, "maintainer", "CI is green.", t0.Add(-15*time.Minute))
		f.addReviewComment("acme", "widgets", 8, 9101, "reviewer", "Check the bound.", t0.Add(-14*time.Minute))
		f.addReview("acme", "widgets", 8, 7101, "reviewer", "APPROVED", t0.Add(-13*time.Minute))
		f.addReview("acme", "widgets", 8, 7102, "reviewer", "PENDING", time.Time{})
		f.addEvent("acme", "widgets", 8, 15000, "labeled", t0.Add(-12*time.Minute))
		f.addEvent("acme", "widgets", 8, 15001, "closed", t0.Add(-6*time.Minute))
		f.setMerged("acme", "widgets", 8)

		if next := h.p.poll(ctx); next != time.Minute {
			t.Fatalf("next interval %s, want 1m", next)
		}
		lastModified := f.lastModified()
		want := map[string]string{
			"notifications.issue_comment.9001":               issue7Comment,
			"notifications.issue_comment.9002":               issue7Comment,
			"notifications.issue_comment.9003":               pr8 + ".comment",
			"notifications.pull_request_review_comment.9101": pr8 + ".comment",
			"notifications.pull_request_review.7101":         pr8 + ".review",
			"notifications.pull_request.event.15001":         pr8,
		}
		if h.pub.storedCount() != len(want) {
			t.Fatalf("stored %v, want %d envelopes", h.pub.sourceIDs(), len(want))
		}
		for sourceID, topic := range want {
			if _, ok := h.pub.get(sourceID, topic); !ok {
				t.Fatalf("no envelope for %s on %s; stored %v", sourceID, topic, h.pub.sourceIDs())
			}
		}
		closed, _ := h.pub.get("notifications.pull_request.event.15001", pr8)
		var payload map[string]string
		if err := json.Unmarshal([]byte(closed.Payload), &payload); err != nil || payload["merged"] != "true" || payload["action"] != "closed" {
			t.Fatalf("the close carries %v (%v), want action closed and merged true", payload, err)
		}
		review, _ := h.pub.get("notifications.pull_request_review.7101", pr8+".review")
		if !strings.Contains(review.Payload, `"state":"approved"`) {
			t.Fatalf("review payload %s, want the state in lower case", review.Payload)
		}
		// A thread's changes are published in time order.
		var prOrder []string
		for _, id := range h.pub.sourceIDs() {
			if id != "notifications.issue_comment.9001" && id != "notifications.issue_comment.9002" {
				prOrder = append(prOrder, id)
			}
		}
		if strings.Join(prOrder, " ") != "notifications.issue_comment.9003 notifications.pull_request_review_comment.9101 notifications.pull_request_review.7101 notifications.pull_request.event.15001" {
			t.Fatalf("the pull request's changes were published as %v", prOrder)
		}

		requests := f.requestsSince(0)
		list := requests[0]
		if list.path != "/notifications" {
			t.Fatalf("first request %s, want the notification list", list.path)
		}
		for name, value := range map[string]string{"participating": "true", "all": "true", "per_page": "50", "since": stamp(t0.Add(-time.Hour))} {
			if list.query.Get(name) != value {
				t.Fatalf("list query %s=%q, want %q", name, list.query.Get(name), value)
			}
		}
		if list.header.Get("If-Modified-Since") != "" {
			t.Fatal("the first list request sent If-Modified-Since")
		}
		for _, request := range requests {
			if !strings.HasSuffix(request.header.Get("Authorization"), " ghp_test") {
				t.Fatalf("%s carried Authorization %q", request.path, request.header.Get("Authorization"))
			}
		}
		for series, value := range map[string]int64{
			`envoy_github_notifications_polls_total{result="ok"}`:                         1,
			`envoy_github_notifications_events_total{kind="issue_comment"}`:               3,
			`envoy_github_notifications_events_total{kind="pull_request_review_comment"}`: 1,
			`envoy_github_notifications_events_total{kind="pull_request_review"}`:         1,
			`envoy_github_notifications_events_total{kind="pull_request"}`:                1,
			"envoy_github_notifications_poll_interval_seconds":                            60,
			"envoy_github_notifications_rate_remaining":                                   4500,
			"envoy_github_notifications_last_success_timestamp_seconds":                   t0.Unix(),
		} {
			if got := metricValue(t, h.reg, series); got != value {
				t.Fatalf("%s = %d, want %d", series, got, value)
			}
		}
		if health := h.p.Health(); !health.LastSuccess.Equal(t0) || health.Degraded || health.Unreadable24h != 0 {
			t.Fatalf("health %+v", health)
		}
		lines := h.logs.lines(t, "INFO", msgPoll)
		if len(lines) != 1 || lines[0]["published"] != float64(6) || lines[0]["status"] != float64(200) ||
			lines[0]["threads"] != float64(2) || lines[0]["next_interval_seconds"] != float64(60) || lines[0]["rate_remaining"] != float64(4500) {
			t.Fatalf("poll lines %v", lines)
		}

		// The next poll starts where this one's answer left the cursor: its Date less 5 minutes,
		// conditional on its Last-Modified.
		h.clock.Advance(time.Minute)
		next := listOf(t, pollRequests(ctx, h))
		if got := next.query.Get("since"); got != stamp(t0.Add(-5*time.Minute)) {
			t.Fatalf("next poll since %s, want the answer's Date less 5 minutes", got)
		}
		if got := next.header.Get("If-Modified-Since"); got != lastModified {
			t.Fatalf("next poll If-Modified-Since %q, want the answer's Last-Modified %q", got, lastModified)
		}
	})

	t.Run("a 200 of two pages processes the threads of both", func(t *testing.T) {
		h := newHarness(t, nil)
		for n := 1; n <= 51; n++ {
			h.f.addThread(strconv.Itoa(n), "acme", "widgets", "Issue", n, t0.Add(-time.Duration(n)*time.Second))
			h.f.addIssue("acme", "widgets", n, false)
		}
		// Thread 51 is the oldest, so the second page holds it.
		h.f.addComment("acme", "widgets", 51, 9051, "maintainer", "On the second page.", t0.Add(-2*time.Minute))
		h.p.poll(ctx)
		requests := h.f.requestsSince(0)
		if countPath(requests, "/notifications") != 2 {
			t.Fatalf("%d list requests, want 2 pages", countPath(requests, "/notifications"))
		}
		// The whole list is read before any thread is fetched: its second page is the second request.
		if requests[1].path != "/notifications" || requests[1].query.Get("page") != "2" {
			t.Fatalf("second request %s %v, want the list's second page", requests[1].path, requests[1].query)
		}
		for n := 1; n <= 51; n++ {
			if got := countPath(requests, "/repos/acme/widgets/issues/"+strconv.Itoa(n)); got != 1 {
				t.Fatalf("issue %d fetched %d times", n, got)
			}
		}
		if _, ok := h.pub.get("notifications.issue_comment.9051", "notifications.github.acme.widgets.issue.51.comment"); !ok {
			t.Fatalf("the second page's comment was not published: %v", h.pub.sourceIDs())
		}
	})

	t.Run("a 304 keeps the cursor and makes no detail call", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.p.poll(ctx)
		lastModified := h.f.lastModified()
		for poll := 2; poll <= 3; poll++ {
			h.clock.Advance(time.Minute)
			requests := pollRequests(ctx, h)
			if len(requests) != 1 || requests[0].path != "/notifications" {
				t.Fatalf("poll %d sent %v, want the list request alone", poll, requests)
			}
			if got := requests[0].header.Get("If-Modified-Since"); got != lastModified {
				t.Fatalf("poll %d If-Modified-Since %q, want the first answer's Last-Modified %q", poll, got, lastModified)
			}
			if got := requests[0].query.Get("since"); got != stamp(t0.Add(-5*time.Minute)) {
				t.Fatalf("poll %d since %s; a 304 moves no cursor", poll, got)
			}
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="not_modified"}`); got != 2 {
			t.Fatalf("not_modified polls %d", got)
		}
		if health := h.p.Health(); !health.LastSuccess.Equal(t0.Add(2 * time.Minute)) {
			t.Fatalf("a 304 is a successful poll; last success %s", health.LastSuccess)
		}
		if lines := h.logs.lines(t, "INFO", msgPoll); len(lines) != 1 {
			t.Fatalf("a quiet poll wrote a line: %v", lines)
		}
	})

	t.Run("a 200 without Last-Modified keeps the last one for If-Modified-Since", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.p.poll(ctx)
		lastModified := h.f.lastModified()
		h.clock.Advance(time.Minute)
		h.f.touch()
		h.f.omitLastModified(true)
		h.p.poll(ctx)
		h.clock.Advance(time.Minute)
		h.f.omitLastModified(false)
		if got := listOf(t, pollRequests(ctx, h)).header.Get("If-Modified-Since"); got != lastModified {
			t.Fatalf("If-Modified-Since %q after an answer without Last-Modified, want the last one, %q", got, lastModified)
		}
	})

	t.Run("margin ids stop a repeat and a late comment is still published", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-2*time.Minute))
		h.p.poll(ctx)
		if h.pub.callCount() != 1 {
			t.Fatalf("first poll published %d", h.pub.callCount())
		}
		// GitHub shows a comment written before the last one only now.
		h.clock.Advance(time.Minute)
		h.f.addComment("acme", "widgets", 7, 9000, "reporter", "Written first, shown late.", t0.Add(-3*time.Minute))
		h.f.bumpThread("1", t0.Add(30*time.Second))
		h.p.poll(ctx)
		if h.pub.callCount() != 2 || h.pub.storedCount() != 2 {
			t.Fatalf("second poll: %d publishes, %d stored; want the late comment alone published", h.pub.callCount(), h.pub.storedCount())
		}
		if _, ok := h.pub.get("notifications.issue_comment.9000", issue7Comment); !ok {
			t.Fatal("the late comment was not published")
		}
		// Listed again inside the overlap with both comments inside the margin: nothing is
		// published a second time.
		h.clock.Advance(time.Minute)
		h.f.touch()
		if requests := pollRequests(ctx, h); countPath(requests, "/repos/acme/widgets/issues/7/comments") != 1 {
			t.Fatalf("the third poll did not read the thread again: %v", requests)
		}
		if h.pub.callCount() != 2 {
			t.Fatalf("the third poll published again: %d publishes", h.pub.callCount())
		}
	})

	t.Run("an unmapped subject type gives one INFO line and no envelope", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Release", 3, t0.Add(-2*time.Minute))
		h.p.poll(ctx)
		if details := h.f.detailsSince(0); len(details) != 0 {
			t.Fatalf("an unmapped thread was fetched: %v", details)
		}
		if h.pub.callCount() != 0 {
			t.Fatal("an unmapped thread was published")
		}
		check := func() {
			lines := h.logs.lines(t, "INFO", msgDropped)
			if len(lines) != 1 {
				t.Fatalf("drop lines %v, want one", lines)
			}
			for name, value := range map[string]string{"repository": "acme/widgets", "subject_type": "Release", "reason": "unmapped", "thread_id": "1"} {
				if lines[0][name] != value {
					t.Fatalf("drop line %s=%v, want %q", name, lines[0][name], value)
				}
			}
			if got := metricValue(t, h.reg, `envoy_github_notifications_threads_dropped_total{reason="unmapped"}`); got != 1 {
				t.Fatalf("unmapped drops %d", got)
			}
		}
		check()
		// The next poll's overlap lists the thread again, unchanged: no second line.
		h.clock.Advance(time.Minute)
		h.f.touch()
		h.p.poll(ctx)
		check()
	})

	t.Run("a skipped owner's thread makes no detail call", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.SkipOwners = []string{" Covered-Org "} })
		h.f.addThread("1", "covered-org", "tools", "Issue", 4, t0.Add(-2*time.Minute))
		h.f.addIssue("covered-org", "tools", 4, false)
		h.f.addComment("covered-org", "tools", 4, 9400, "maintainer", "Covered by the App.", t0.Add(-3*time.Minute))
		h.f.addThread("2", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.p.poll(ctx)
		for _, request := range h.f.detailsSince(0) {
			if strings.HasPrefix(request.path, "/repos/covered-org/") {
				t.Fatalf("a skipped owner's thread was fetched: %s", request.path)
			}
		}
		if h.pub.storedCount() != 1 {
			t.Fatalf("stored %v, want the other owner's comment alone", h.pub.sourceIDs())
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_threads_dropped_total{reason="skipped_owner"}`); got != 1 {
			t.Fatalf("skipped_owner drops %d", got)
		}
	})

	t.Run("New takes each skip owner as one owner, a comma and all", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.SkipOwners = []string{"acme,covered-org"} })
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.p.poll(ctx)
		if h.pub.storedCount() != 1 {
			t.Fatalf("stored %v; the entry \"acme,covered-org\" names no owner acme", h.pub.sourceIDs())
		}
	})

	t.Run("a 403 with Retry-After waits as GitHub says and keeps the state", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.f.answer("/notifications", http.StatusForbidden, http.Header{"Retry-After": {"300"}}, 1)
		if next := h.p.poll(ctx); next != 5*time.Minute {
			t.Fatalf("next interval %s, want Retry-After's 5m", next)
		}
		if len(h.f.detailsSince(0)) != 0 || h.pub.callCount() != 0 {
			t.Fatal("a rate-limited list fetched a thread or published")
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="rate_limited"}`); got != 1 {
			t.Fatalf("rate_limited polls %d", got)
		}
		h.clock.Advance(5 * time.Minute)
		if got := listOf(t, pollRequests(ctx, h)).query.Get("since"); got != stamp(t0.Add(-time.Hour)) {
			t.Fatalf("after the wait the list started at %s; a rate-limited list moves no cursor", got)
		}
		if h.pub.storedCount() != 1 {
			t.Fatalf("after the wait, stored %v", h.pub.sourceIDs())
		}

		// A detail answer with X-RateLimit-Remaining 0 waits until the reset, keeping the thread
		// state and the cursor.
		h.clock.Advance(time.Minute)
		h.f.addComment("acme", "widgets", 7, 9002, "reporter", "Thanks.", h.clock.Now().Add(-30*time.Second))
		h.f.bumpThread("1", h.clock.Now().Add(-30*time.Second))
		reset := h.clock.Now().Add(10 * time.Minute)
		h.f.answer("/repos/acme/widgets/issues/7", http.StatusForbidden, http.Header{
			"X-RateLimit-Remaining": {"0"}, "X-RateLimit-Reset": {strconv.FormatInt(reset.Unix(), 10)},
		}, 1)
		if next := h.p.poll(ctx); next != 10*time.Minute {
			t.Fatalf("next interval %s, want the 10m to the reset", next)
		}
		if h.pub.storedCount() != 1 {
			t.Fatal("a rate-limited thread published")
		}
		h.clock.Advance(10 * time.Minute)
		if got := listOf(t, pollRequests(ctx, h)).query.Get("since"); got != stamp(t0) {
			t.Fatalf("after the reset the list started at %s; a rate-limited thread moves no cursor", got)
		}
		if _, ok := h.pub.get("notifications.issue_comment.9002", issue7Comment); !ok {
			t.Fatalf("after the reset, stored %v", h.pub.sourceIDs())
		}
	})

	t.Run("a 403 with X-GitHub-SSO retries the thread after one hour", func(t *testing.T) {
		h := newHarness(t, nil)
		ssoIssue := "/repos/sso-org/private-tools/issues/3"
		h.f.addThread("1", "sso-org", "private-tools", "Issue", 3, t0.Add(-2*time.Minute))
		h.f.addIssue("sso-org", "private-tools", 3, false)
		h.f.addComment("sso-org", "private-tools", 3, 9301, "maintainer", "Behind single sign-on.", t0.Add(-3*time.Minute))
		h.f.addThread("2", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.f.answer(ssoIssue, http.StatusForbidden, http.Header{
			"X-GitHub-SSO": {"required; url=https://github.com/orgs/sso-org/sso?authorization_request=example"},
		}, -1)
		h.p.poll(ctx)
		if h.pub.storedCount() != 1 {
			t.Fatalf("stored %v, want the other owner's comment alone", h.pub.sourceIDs())
		}
		warn := func(want int) {
			t.Helper()
			lines := h.logs.lines(t, "WARN", msgSSO)
			if len(lines) != want {
				t.Fatalf("SSO lines %v, want %d", lines, want)
			}
			if lines[0]["owner"] != "sso-org" || lines[0]["repository"] != "sso-org/private-tools" {
				t.Fatalf("SSO line %v", lines[0])
			}
		}
		warn(1)
		h.clock.Advance(time.Minute)
		h.f.touch()
		requests := pollRequests(ctx, h)
		if got := listOf(t, requests).query.Get("since"); got != stamp(t0.Add(-5*time.Minute)) {
			t.Fatalf("second poll since %s; a thread behind single sign-on holds the cursor back", got)
		}
		if got := countPath(requests, ssoIssue); got != 0 {
			t.Fatalf("the thread was fetched %d times within the hour", got)
		}
		warn(1)
		h.clock.Advance(time.Hour)
		h.f.clearAnswer(ssoIssue)
		if got := countPath(pollRequests(ctx, h), ssoIssue); got != 1 {
			t.Fatalf("after the hour the thread was fetched %d times, want 1", got)
		}
		if _, ok := h.pub.get("notifications.issue_comment.9301", "notifications.github.sso-org.private-tools.issue.3.comment"); !ok {
			t.Fatalf("after the hour, stored %v", h.pub.sourceIDs())
		}
		warn(1)
	})

	t.Run("another 403 or a 404 marks the thread unreadable once for each update", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.answer("/repos/acme/widgets/issues/7", http.StatusNotFound, nil, -1)
		h.f.addThread("2", "acme", "gadgets", "Issue", 9, t0.Add(-3*time.Minute))
		h.f.answer("/repos/acme/gadgets/issues/9", http.StatusForbidden, nil, -1)
		h.p.poll(ctx)
		check := func(wantLines int, wantStatus map[string]float64) {
			t.Helper()
			lines := h.logs.lines(t, "WARN", msgUnreadable)
			statuses := map[string]float64{}
			for _, line := range lines {
				statuses[line["repository"].(string)] = line["status"].(float64)
			}
			if len(lines) != wantLines || statuses["acme/widgets"] != wantStatus["acme/widgets"] || statuses["acme/gadgets"] != wantStatus["acme/gadgets"] {
				t.Fatalf("unreadable lines %v, want %d", lines, wantLines)
			}
			if got := metricValue(t, h.reg, `envoy_github_notifications_threads_dropped_total{reason="unreadable"}`); got != int64(wantLines) {
				t.Fatalf("unreadable drops %d, want %d", got, wantLines)
			}
		}
		statuses := map[string]float64{"acme/widgets": 404, "acme/gadgets": 403}
		check(2, statuses)
		if got := h.p.Health().Unreadable24h; got != 2 {
			t.Fatalf("unreadable in 24h %d", got)
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="ok"}`); got != 1 {
			t.Fatalf("ok polls %d", got)
		}

		// Listed again inside the overlap, unchanged: neither thread is fetched or logged again,
		// and an unreadable thread holds the cursor back no more than a published one.
		h.clock.Advance(time.Minute)
		h.f.touch()
		requests := pollRequests(ctx, h)
		if got := listOf(t, requests).query.Get("since"); got != stamp(t0.Add(-5*time.Minute)) {
			t.Fatalf("second poll since %s", got)
		}
		if len(requests) != 1 {
			t.Fatalf("an unchanged unreadable thread was fetched again: %v", requests)
		}
		check(2, statuses)

		// A change to the thread reads it again.
		h.f.bumpThread("1", h.clock.Now())
		h.clock.Advance(time.Minute)
		if got := countPath(pollRequests(ctx, h), "/repos/acme/widgets/issues/7"); got != 1 {
			t.Fatalf("the changed thread was fetched %d times, want 1", got)
		}
		lines := h.logs.lines(t, "WARN", msgUnreadable)
		if len(lines) != 3 || lines[2]["repository"] != "acme/widgets" {
			t.Fatalf("unreadable lines %v, want a third for the changed thread", lines)
		}
		if got := h.p.Health().Unreadable24h; got != 2 {
			t.Fatalf("unreadable in 24h %d; it counts threads", got)
		}
		// A day later the count is spent.
		h.clock.Advance(25 * time.Hour)
		if got := h.p.Health().Unreadable24h; got != 0 {
			t.Fatalf("unreadable in 24h %d a day later", got)
		}
	})

	t.Run("a thread failing transiently for 5 polls in a row is written off, and the cursor moves on", func(t *testing.T) {
		h := newHarness(t, nil)
		comments7 := "/repos/acme/widgets/issues/7/comments"
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0)
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.answer(comments7, http.StatusBadGateway, nil, -1)
		h.f.addThread("2", "acme", "gadgets", "Issue", 9, t0.Add(-3*time.Minute))
		h.f.addIssue("acme", "gadgets", 9, false)
		h.f.addComment("acme", "gadgets", 9, 9901, "maintainer", "Fine here.", t0.Add(-4*time.Minute))
		for poll := 1; poll <= 5; poll++ {
			if poll > 1 {
				h.clock.Advance(time.Minute)
			}
			if got := listOf(t, pollRequests(ctx, h)).query.Get("since"); got != stamp(t0.Add(-time.Hour)) {
				t.Fatalf("poll %d listed since %s; a failing thread holds the cursor", poll, got)
			}
			if poll == 5 {
				break
			}
			if lines := h.logs.lines(t, "WARN", msgUnreadable); len(lines) != 0 {
				t.Fatalf("poll %d wrote the thread off: %v", poll, lines)
			}
			if !h.p.Health().LastSuccess.IsZero() {
				t.Fatalf("poll %d counted as a success", poll)
			}
		}
		lines := h.logs.lines(t, "WARN", msgUnreadable)
		if len(lines) != 1 || lines[0]["repository"] != "acme/widgets" || lines[0]["status"] != float64(502) || lines[0]["failures"] != float64(5) {
			t.Fatalf("unreadable lines %v, want one for the failing thread after 5 polls", lines)
		}
		if health := h.p.Health(); !health.LastSuccess.Equal(t0.Add(4*time.Minute)) || health.Unreadable24h != 1 {
			t.Fatalf("health %+v after the write-off", health)
		}
		for series, value := range map[string]int64{
			`envoy_github_notifications_polls_total{result="error"}`:                4,
			`envoy_github_notifications_polls_total{result="ok"}`:                   1,
			`envoy_github_notifications_threads_dropped_total{reason="unreadable"}`: 1,
		} {
			if got := metricValue(t, h.reg, series); got != value {
				t.Fatalf("%s = %d, want %d", series, got, value)
			}
		}

		// The cursor moved, and the written-off thread, listed again unchanged, is not fetched.
		h.clock.Advance(time.Minute)
		requests := pollRequests(ctx, h)
		if got := listOf(t, requests).query.Get("since"); got != stamp(t0.Add(-time.Minute)) {
			t.Fatalf("poll 6 since %s, want poll 5's Date less 5 minutes", got)
		}
		if countPath(requests, "/repos/acme/widgets/issues/7") != 0 || countPath(requests, comments7) != 0 {
			t.Fatalf("the written-off thread was fetched again: %v", requests)
		}
		if lines := h.logs.lines(t, "WARN", msgUnreadable); len(lines) != 1 {
			t.Fatalf("unreadable lines %v after poll 6", lines)
		}
		if h.pub.storedCount() != 1 || h.pub.callCount() != 1 {
			t.Fatalf("the other thread: %d stored, %d publishes; want its comment once", h.pub.storedCount(), h.pub.callCount())
		}
	})

	t.Run("transient failures that stop short of 5 polls in a row write nothing off", func(t *testing.T) {
		h := newHarness(t, nil)
		comments7 := "/repos/acme/widgets/issues/7/comments"
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0)
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-time.Minute))
		h.f.answer(comments7, http.StatusBadGateway, nil, 4)
		for range 5 {
			h.p.poll(ctx)
			h.clock.Advance(time.Minute)
		}
		if _, ok := h.pub.get("notifications.issue_comment.9001", issue7Comment); !ok {
			t.Fatalf("after 4 failures and a success, stored %v", h.pub.sourceIDs())
		}
		h.f.addComment("acme", "widgets", 7, 9002, "reporter", "Thanks.", h.clock.Now())
		h.f.bumpThread("1", h.clock.Now())
		h.f.answer(comments7, http.StatusBadGateway, nil, 4)
		for range 5 {
			h.p.poll(ctx)
			h.clock.Advance(time.Minute)
		}
		if _, ok := h.pub.get("notifications.issue_comment.9002", issue7Comment); !ok {
			t.Fatalf("after 4 more failures and a success, stored %v", h.pub.sourceIDs())
		}
		if lines := h.logs.lines(t, "WARN", msgUnreadable); len(lines) != 0 {
			t.Fatalf("a success between failures still wrote the thread off: %v", lines)
		}
	})

	t.Run("a publish that fails does not count toward writing a thread off", func(t *testing.T) {
		h := newHarness(t, nil)
		comments7 := "/repos/acme/widgets/issues/7/comments"
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0)
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-time.Minute))
		// Four polls whose fetch failed, then four whose read succeeded and whose publish failed:
		// the read that succeeded cleared the count, so the next fetch failure starts again at one.
		h.f.answer(comments7, http.StatusBadGateway, nil, 4)
		h.pub.failPublishes(4)
		for poll := 1; poll <= 8; poll++ {
			h.p.poll(ctx)
			if h.pub.storedCount() != 0 {
				t.Fatalf("poll %d stored through a failing stream", poll)
			}
			h.clock.Advance(time.Minute)
		}
		if lines := h.logs.lines(t, "WARN", msgUnreadable); len(lines) != 0 {
			t.Fatalf("a failing stream wrote the thread off: %v", lines)
		}
		h.f.answer(comments7, http.StatusBadGateway, nil, 4)
		for poll := 9; poll <= 12; poll++ {
			h.p.poll(ctx)
			h.clock.Advance(time.Minute)
		}
		if lines := h.logs.lines(t, "WARN", msgUnreadable); len(lines) != 0 {
			t.Fatalf("the fetch failures counted on past the read that succeeded: %v", lines)
		}
		h.p.poll(ctx)
		if _, ok := h.pub.get("notifications.issue_comment.9001", issue7Comment); !ok {
			t.Fatalf("once both answer, stored %v", h.pub.sourceIDs())
		}
	})

	t.Run("a 5xx keeps the thread state and the cursor", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		h.f.answer("/repos/acme/widgets/issues/7/comments", http.StatusBadGateway, nil, 1)
		h.p.poll(ctx)
		if h.pub.callCount() != 0 {
			t.Fatal("a 5xx published")
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="error"}`); got != 1 {
			t.Fatalf("error polls %d", got)
		}
		if lines := h.logs.lines(t, "INFO", msgPoll); len(lines) != 1 || lines[0]["result"] != "error" {
			t.Fatalf("a failed poll's lines %v", lines)
		}
		if !h.p.Health().LastSuccess.IsZero() {
			t.Fatal("a failed poll counted as a success")
		}
		h.clock.Advance(time.Minute)
		if got := listOf(t, pollRequests(ctx, h)).query.Get("since"); got != stamp(t0.Add(-time.Hour)) {
			t.Fatalf("the retry listed since %s; a 5xx moves no cursor", got)
		}
		if h.pub.storedCount() != 1 {
			t.Fatalf("the retry stored %v", h.pub.sourceIDs())
		}
		h.clock.Advance(time.Minute)
		if got := listOf(t, pollRequests(ctx, h)).query.Get("since"); got != stamp(t0.Add(-4*time.Minute)) {
			t.Fatalf("after the retry the list started at %s, want its Date less 5 minutes", got)
		}
	})

	t.Run("a 401 logs one ERROR an hour and degrades after the bound", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.answer("/notifications", http.StatusUnauthorized, nil, -1)
		errors := func(want int) {
			t.Helper()
			if lines := h.logs.lines(t, "ERROR", msgUnauthorized); len(lines) != want {
				t.Fatalf("401 lines %v, want %d", lines, want)
			}
		}
		h.p.poll(ctx)
		errors(1)
		if h.p.Health().Degraded {
			t.Fatal("degraded at the first poll")
		}
		h.clock.Advance(time.Minute)
		h.p.poll(ctx)
		errors(1)
		h.clock.Advance(10 * time.Minute)
		if !h.p.Health().Degraded {
			t.Fatal("not degraded after 11 minutes without a successful poll")
		}
		h.p.poll(ctx)
		errors(1)
		h.clock.Advance(50 * time.Minute)
		h.p.poll(ctx)
		errors(2)
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="unauthorized"}`); got != 4 {
			t.Fatalf("unauthorized polls %d", got)
		}
		if details := h.f.detailsSince(0); len(details) != 0 {
			t.Fatalf("a refused token fetched %v", details)
		}
	})

	t.Run("X-Poll-Interval sets the next interval, never under 60 s", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.setPollInterval("120")
		if next := h.p.poll(ctx); next != 2*time.Minute {
			t.Fatalf("next interval %s, want 2m", next)
		}
		if got := metricValue(t, h.reg, "envoy_github_notifications_poll_interval_seconds"); got != 120 {
			t.Fatalf("interval gauge %d", got)
		}
		h.f.setPollInterval("30")
		h.clock.Advance(2 * time.Minute)
		if next := h.p.poll(ctx); next != time.Minute {
			t.Fatalf("next interval %s, want the 1m floor", next)
		}
		if got := metricValue(t, h.reg, "envoy_github_notifications_poll_interval_seconds"); got != 60 {
			t.Fatalf("interval gauge %d", got)
		}
	})

	t.Run("a rate floor of 500 skips detail fetches until the reset, and is no failed poll", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-2*time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		h.f.addComment("acme", "widgets", 7, 9001, "maintainer", "Reproduced.", t0.Add(-3*time.Minute))
		reset := t0.Add(15 * time.Minute)
		h.f.setRate(499, reset)
		// Longer than the 10-minute degraded bound: the floor is the poller keeping to GitHub's
		// budget, not a poller that fails.
		for minute := range 15 {
			if minute > 0 {
				h.clock.Advance(time.Minute)
			}
			requests := pollRequests(ctx, h)
			if got := listOf(t, requests).query.Get("since"); got != stamp(t0.Add(-time.Hour)) {
				t.Fatalf("minute %d listed since %s; the thread the floor held back holds the cursor", minute, got)
			}
			if len(requests) != 1 {
				t.Fatalf("minute %d sent %v under the rate floor", minute, requests)
			}
			if health := h.p.Health(); health.Degraded || !health.LastSuccess.Equal(h.clock.Now()) {
				t.Fatalf("minute %d health %+v; a poll the floor held back is still a success", minute, health)
			}
		}
		if h.pub.callCount() != 0 {
			t.Fatal("a poll under the rate floor published")
		}
		if got := metricValue(t, h.reg, `envoy_github_notifications_polls_total{result="rate_floor"}`); got != 15 {
			t.Fatalf("rate_floor polls %d", got)
		}
		if got := metricValue(t, h.reg, "envoy_github_notifications_rate_remaining"); got != 499 {
			t.Fatalf("rate gauge %d", got)
		}
		if got := metricValue(t, h.reg, "envoy_github_notifications_last_success_timestamp_seconds"); got != h.clock.Now().Unix() {
			t.Fatalf("last success gauge %d", got)
		}
		if lines := h.logs.lines(t, "INFO", msgPoll); len(lines) != 0 {
			t.Fatalf("a poll the floor held back wrote a line: %v", lines)
		}
		h.clock.Advance(time.Minute)
		h.f.setRate(4999, reset.Add(time.Hour))
		h.p.poll(ctx)
		if h.pub.storedCount() != 1 {
			t.Fatalf("after the reset, stored %v", h.pub.sourceIDs())
		}
	})

	t.Run("a token expiring in 10 days warns once a day", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.setHeader("GitHub-Authentication-Token-Expiration", t0.Add(10*24*time.Hour).Format("2006-01-02 15:04:05 MST"))
		warns := func(want int) {
			t.Helper()
			lines := h.logs.lines(t, "WARN", msgExpiry)
			if len(lines) != want {
				t.Fatalf("expiry lines %v, want %d", lines, want)
			}
			if lines[0]["expires_at"] != stamp(t0.Add(10*24*time.Hour)) {
				t.Fatalf("expiry line %v", lines[0])
			}
		}
		h.p.poll(ctx)
		warns(1)
		h.clock.Advance(time.Minute)
		h.f.touch()
		h.p.poll(ctx)
		warns(1)
		h.clock.Advance(24 * time.Hour)
		h.p.poll(ctx)
		warns(2)
	})

	t.Run("a token expiring in 20 days does not warn", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.setHeader("GitHub-Authentication-Token-Expiration", t0.Add(20*24*time.Hour).Format("2006-01-02 15:04:05 MST"))
		h.p.poll(ctx)
		if lines := h.logs.lines(t, "WARN", msgExpiry); len(lines) != 0 {
			t.Fatalf("expiry lines %v", lines)
		}
	})

	t.Run("a thread with 150 new comments publishes every page once", func(t *testing.T) {
		h := newHarness(t, nil)
		h.f.addThread("1", "acme", "widgets", "Issue", 7, t0.Add(-time.Minute))
		h.f.addIssue("acme", "widgets", 7, false)
		for i := range 150 {
			h.f.addComment("acme", "widgets", 7, 10000+i, "maintainer", "Comment "+strconv.Itoa(i), t0.Add(-30*time.Minute+time.Duration(i)*time.Second))
		}
		h.p.poll(ctx)
		if h.pub.storedCount() != 150 || h.pub.callCount() != 150 {
			t.Fatalf("stored %d after %d publishes, want 150 of 150", h.pub.storedCount(), h.pub.callCount())
		}
		requests := h.f.requestsSince(0)
		if got := countPath(requests, "/repos/acme/widgets/issues/7/comments"); got != 2 {
			t.Fatalf("%d comment pages read, want 2", got)
		}
		var pages []string
		for _, request := range requests {
			if request.path == "/repos/acme/widgets/issues/7/comments" {
				if request.query.Get("per_page") != "100" {
					t.Fatalf("comment page query %v, want 100 a page", request.query)
				}
				pages = append(pages, request.query.Get("page"))
			}
		}
		if pages[1] != "2" {
			t.Fatalf("comment pages %q, want the second read as page 2", pages)
		}
	})

	t.Run("a list GitHub cannot filter by time is read in 10 pages: its first and its newest 9", func(t *testing.T) {
		h := newHarness(t, nil)
		reviews := "/repos/acme/widgets/pulls/8/reviews"
		h.f.addThread("1", "acme", "widgets", "PullRequest", 8, t0.Add(-time.Minute))
		h.f.addIssue("acme", "widgets", 8, true)
		for i := range 1199 {
			h.f.addReview("acme", "widgets", 8, 20000+i, "reviewer", "COMMENTED", t0.Add(-3*time.Hour))
		}
		h.f.addReview("acme", "widgets", 8, 21199, "reviewer", "APPROVED", t0.Add(-2*time.Minute))
		h.p.poll(ctx)
		var pages []string
		for _, request := range h.f.requestsSince(0) {
			if request.path == reviews {
				pages = append(pages, request.query.Get("page"))
			}
		}
		want := []string{"", "4", "5", "6", "7", "8", "9", "10", "11", "12"}
		if !slices.Equal(pages, want) {
			t.Fatalf("review pages read %q, want %q: the first, then the newest 9 of 12", pages, want)
		}
		if h.pub.storedCount() != 1 {
			t.Fatalf("stored %v, want the newest review alone", h.pub.sourceIDs())
		}
		if _, ok := h.pub.get("notifications.pull_request_review.21199", pr8+".review"); !ok {
			t.Fatalf("the newest review was not published: %v", h.pub.sourceIDs())
		}
	})
}

func TestRunReturnsWhenItsContextEnds(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.p.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.f.mark() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run sent no request at its start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once its context ended")
	}
}
