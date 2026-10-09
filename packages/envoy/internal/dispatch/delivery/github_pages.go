package delivery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// githubPageAttempts bounds how many times one list/search page is asked for before the step
// gives up. A reconcile pass of a busy repository makes thousands of page requests, and GitHub
// times a slow query out often enough that a pass with no retry at all effectively never
// finished one: three attempts turns a single slow page from "this pass fails" into "this page
// costs a few more seconds".
const githubPageAttempts = 3

// githubPageTimeout is one attempt's own deadline. GitHub terminates a request it has spent
// about ten seconds on and answers its own error, so a deadline at ten seconds races GitHub's
// own and turns an answer that was about to arrive -- the real one, or GitHub's own timeout,
// which this file retries -- into a bare client timeout. Measured against the live API, a page
// of workflow runs answers in 1.2-1.8 s and a merged-PR search page in 1.3-3.5 s, so this is
// about an order of magnitude above a healthy page and still above GitHub's own bound plus the
// transfer of a response at the client's 1 MiB cap.
//
// A var rather than a const so a test can shrink it; nothing outside a test writes it.
var githubPageTimeout = githubapp.RequestTimeout

// githubPageRetryWait is how long a retry waits before asking again, multiplied by the attempt
// just made (1 s, then 2 s). A var for the same reason as githubPageTimeout.
var githubPageRetryWait = time.Second

// readGitHubPage performs one GET of a list page with its own deadline, retrying a timeout or
// one of GitHub's own timeout answers up to githubPageAttempts times. Returns the last attempt's
// answer exactly as githubapp.Client.Read would, so a caller's own status handling (a 404 that
// means "gone", a rate limit) is unchanged.
func readGitHubPage(ctx context.Context, client *githubapp.Client, token, path string) ([]byte, int, http.Header, error) {
	var body []byte
	var status int
	var header http.Header
	var err error
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, githubPageTimeout)
		body, status, header, err = client.Read(attemptCtx, token, path)
		cancel()
		if !retryGitHubPage(ctx, attempt, status, err) {
			return body, status, header, err
		}
		if waitErr := waitBeforeRetry(ctx, attempt); waitErr != nil {
			return body, status, header, err
		}
	}
}

// queryGitHubPage is readGitHubPage for one GraphQL search page: the same deadline and the same
// bounded retry. A GraphQL query is a read however GitHub spells it, so retrying one repeats
// nothing.
func queryGitHubPage(ctx context.Context, client *githubapp.Client, token, query string, variables map[string]any) ([]byte, int, http.Header, error) {
	var body []byte
	var status int
	var header http.Header
	var err error
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, githubPageTimeout)
		body, status, header, err = client.GraphQL(attemptCtx, token, query, variables)
		cancel()
		if !retryGitHubPage(ctx, attempt, status, err) {
			return body, status, header, err
		}
		if waitErr := waitBeforeRetry(ctx, attempt); waitErr != nil {
			return body, status, header, err
		}
	}
}

// retryGitHubPage reports whether an attempt's outcome is worth asking again for: this attempt's
// own deadline passing while the caller's context is still alive, any other network timeout, or
// one of GitHub's own gateway-timeout answers (it reports a query it gave up on as a 502 or a
// 504). Everything else -- a 404, a rate limit, a decode failure, a response past the size cap,
// the caller's own context ending -- is the answer, not a reason to ask again.
func retryGitHubPage(ctx context.Context, attempt, status int, err error) bool {
	if attempt >= githubPageAttempts {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return true
		}
		var netErr net.Error
		return errors.As(err, &netErr) && netErr.Timeout()
	}
	return status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}

// waitBeforeRetry sleeps between attempts, or returns the caller's context error when it ends
// first.
func waitBeforeRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * githubPageRetryWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// pageAttemptsNote names the retry budget in an error, so a failure that exhausted it reads as
// "GitHub was asked three times" rather than looking like a single unlucky call.
func pageAttemptsNote() string {
	return fmt.Sprintf("after %d attempts", githubPageAttempts)
}
