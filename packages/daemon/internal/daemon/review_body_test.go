package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// reviewBody restores a truncated review-App review's body from GitHub, since only the review
// App's own reviews can carry the Legion footer the workflow reads (byReviewer): any other
// author's review passes through unchanged, with no GitHub call made.
func TestReviewBodyRestoresATruncatedReviewAppReview(t *testing.T) {
	var gotPath string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"body":"the full body\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->"}`))
	}))
	defer server.Close()

	pool := reviewPermissionPool(t)
	w := reviewPermissionRuntime(pool, server.URL, quietLogger())
	review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, Author: "legion-reviewer[bot]",
		Body: "a capped body", BodyTruncated: true}

	got, err := w.reviewBody(context.Background(), review)
	if err != nil {
		t.Fatalf("restore the review's body: %v", err)
	}
	if want := "the full body\n\n<!-- legion: {\"session\":\"ses-reviewer\",\"phase\":\"review\"} -->"; got != want {
		t.Fatalf("reviewBody = %q, want %q", got, want)
	}
	if calls != 1 {
		t.Fatalf("GitHub was asked %d times, want once", calls)
	}
	if want := "/repos/acme/widgets/pulls/42/reviews/7"; gotPath != want {
		t.Fatalf("GitHub was asked %s, want %s", gotPath, want)
	}
}

// Any author but the review App's own has nothing to restore: no footer but the review App's own
// session could ever decide a round, so its capped body is returned unchanged with no call made.
func TestReviewBodyLeavesAnotherAuthorsReviewUnchanged(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"body":"should never be read"}`))
	}))
	defer server.Close()

	pool := reviewPermissionPool(t)
	w := reviewPermissionRuntime(pool, server.URL, quietLogger())
	review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, Author: "a-human",
		Body: "a capped body", BodyTruncated: true}

	got, err := w.reviewBody(context.Background(), review)
	if err != nil {
		t.Fatalf("resolve another author's review body: %v", err)
	}
	if got != "a capped body" {
		t.Fatalf("reviewBody = %q, want the review's own body unchanged", got)
	}
	if calls != 0 {
		t.Fatalf("GitHub was asked %d times, want none", calls)
	}
}

// A review-App review on a pull request the daemon does not record is dropped unread, as the
// workflow drops it: its capped body is returned unchanged with no GitHub call made.
func TestReviewBodyLeavesAnUnrecordedPullRequestsReviewUnchanged(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"body":"should never be read"}`))
	}))
	defer server.Close()

	pool := reviewPermissionPool(t)
	w := reviewPermissionRuntime(pool, server.URL, quietLogger())
	review := intake.PullRequestReview{Repo: "acme/widgets", Number: 43, ID: 7, Author: "legion-reviewer[bot]",
		Body: "a capped body", BodyTruncated: true}

	got, err := w.reviewBody(context.Background(), review)
	if err != nil {
		t.Fatalf("resolve an unrecorded pull request's review body: %v", err)
	}
	if got != "a capped body" {
		t.Fatalf("reviewBody = %q, want the review's own body unchanged", got)
	}
	if calls != 0 {
		t.Fatalf("GitHub was asked %d times, want none", calls)
	}
}

// While GitHub's rate limit stands, a review's body is answered with the wait left and no call is
// made, as reviewerCanWrite's permission read is.
func TestReviewBodyWithRateLimitHeldMakesNoRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"body":"should never be read"}`))
	}))
	defer server.Close()

	pool := reviewPermissionPool(t)
	w := reviewPermissionRuntime(pool, server.URL, quietLogger())
	w.holdRateLimit(2 * time.Minute)
	review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, Author: "legion-reviewer[bot]",
		Body: "a capped body", BodyTruncated: true}

	_, err := w.reviewBody(context.Background(), review)
	var later *intake.RetryLater
	if !errors.As(err, &later) {
		t.Fatalf("a rate-limited read = %v; want an intake.RetryLater", err)
	}
	if calls != 0 {
		t.Fatalf("GitHub was asked %d times, want none", calls)
	}
}

// A rate-limited read is returned as an intake.RetryLater naming GitHub's wait, so intake waits it
// out before the review's delivery is tried again, the same as a rate-limited permission read.
func TestReviewBodyRateLimitedReadRetriesLater(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	pool := reviewPermissionPool(t)
	w := reviewPermissionRuntime(pool, server.URL, quietLogger())
	review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 7, Author: "legion-reviewer[bot]",
		Body: "a capped body", BodyTruncated: true}

	_, err := w.reviewBody(context.Background(), review)
	var later *intake.RetryLater
	if !errors.As(err, &later) {
		t.Fatalf("a rate-limited read = %v; want an intake.RetryLater", err)
	}
	if later.After != 120*time.Second {
		t.Fatalf("RetryLater.After = %s, want 120s", later.After)
	}
	if left := w.rateLimitLeft(); left <= 0 {
		t.Fatalf("rateLimitLeft() = %s after a rate-limited body read, want it held", left)
	}
	if calls != 1 {
		t.Fatalf("GitHub was asked %d times, want once", calls)
	}

	_, err = w.reviewBody(context.Background(), review)
	if !errors.As(err, &later) {
		t.Fatalf("a second read within the held limit = %v; want an intake.RetryLater", err)
	}
	if calls != 1 {
		t.Fatalf("GitHub was asked %d times across two reads within the held limit, want once", calls)
	}
}
