package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// pollRequest is one request the conditional reads sent, as the stand-in GitHub saw it: sent
// says whether an If-None-Match header was there at all, which is not the same as an empty one.
type pollRequest struct {
	method      string
	path        string
	query       string
	ifNoneMatch string
	sent        bool
}

// pollGitHub is a GitHub API stand-in that records every request the conditional reads send and
// answers each from the handler the test gave it.
type pollGitHub struct {
	mu       sync.Mutex
	requests []pollRequest
	answer   http.HandlerFunc
}

func startPollGitHub(t *testing.T, answer http.HandlerFunc) (*Client, *pollGitHub) {
	t.Helper()
	fake := &pollGitHub{answer: answer}
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		values, sent := r.Header["If-None-Match"]
		fake.mu.Lock()
		fake.requests = append(fake.requests, pollRequest{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.RawQuery,
			ifNoneMatch: strings.Join(values, ","),
			sent:        sent,
		})
		fake.mu.Unlock()
		fake.answer(w, r)
	})
	return client, fake
}

// last is the request GitHub was sent most recently.
func (f *pollGitHub) last(t *testing.T) pollRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("GitHub was sent no request")
	}
	return f.requests[len(f.requests)-1]
}

// checkObjects is a page of check objects as GitHub answers it, under the key its endpoint
// carries them under.
func checkObjects(field string, objects []map[string]any) string {
	body, err := json.Marshal(map[string]any{"total_count": len(objects), field: objects})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// manyObjects is a page of exactly n check objects, enough of each for the page to be decoded.
func manyObjects(n int) []map[string]any {
	objects := make([]map[string]any, n)
	for i := range objects {
		objects[i] = map[string]any{"id": float64(i + 1), "name": "check-" + strconv.Itoa(i+1), "status": "completed"}
	}
	return objects
}

// A conditional read sends the caller's ETag as If-None-Match and hands GitHub's 304 back as
// that status, with no body and no error; the same read with no ETag sends no such header, so
// GitHub answers the whole thing.
func TestReadConditionalWithinSendsTheETagAndReadsA304(t *testing.T) {
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"full_name":"acme/web"}`)
	})
	ctx := context.Background()

	body, status, header, err := client.ReadConditionalWithin(ctx, "installation-token", "/repos/acme/web", `"v1"`, pollResponseLimit)
	if err != nil {
		t.Fatalf("conditional read with the ETag: %v", err)
	}
	if status != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("conditional read with the ETag: status=%d body=%q, want 304 and no body", status, body)
	}
	if got := header.Get("ETag"); got != `"v1"` {
		t.Fatalf("the 304's ETag = %q, want %q", got, `"v1"`)
	}
	if sent := fake.last(t); !sent.sent || sent.ifNoneMatch != `"v1"` {
		t.Fatalf("If-None-Match sent = %q (present=%v), want %q", sent.ifNoneMatch, sent.sent, `"v1"`)
	}

	body, status, _, err = client.ReadConditionalWithin(ctx, "installation-token", "/repos/acme/web", "", pollResponseLimit)
	if err != nil || status != http.StatusOK || string(body) != `{"full_name":"acme/web"}` {
		t.Fatalf("unconditional read: status=%d body=%q err=%v, want GitHub's whole 200", status, body, err)
	}
	if sent := fake.last(t); sent.sent {
		t.Fatalf("an empty ETag sent If-None-Match: %q", sent.ifNoneMatch)
	}
}

// CommitCheckRuns asks for every run of the commit, a hundred to a page, and answers the objects
// GitHub listed with the answer's own ETag. A page short of a hundred is not full.
func TestCommitCheckRunsReadsThePageWithItsETag(t *testing.T) {
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"runs-1"`)
		fmt.Fprint(w, checkObjects("check_runs", []map[string]any{
			{"id": float64(11), "name": "unit", "conclusion": "success"},
			{"id": float64(12), "name": "lint", "conclusion": "failure"},
		}))
	})

	page, err := client.CommitCheckRuns(context.Background(), "installation-token", "acme", "web", "c0ffee", 2, `"runs-0"`)
	if err != nil {
		t.Fatalf("CommitCheckRuns: %v", err)
	}
	sent := fake.last(t)
	if sent.path != "/repos/acme/web/commits/c0ffee/check-runs" || sent.query != "filter=all&per_page=100&page=2" {
		t.Fatalf("CommitCheckRuns asked for %s?%s, want the commit's check runs, filter=all, 100 a page, page 2", sent.path, sent.query)
	}
	if sent.ifNoneMatch != `"runs-0"` {
		t.Fatalf("If-None-Match sent = %q, want %q", sent.ifNoneMatch, `"runs-0"`)
	}
	if page.NotModified || page.Full || page.ETag != `"runs-1"` || len(page.Objects) != 2 {
		t.Fatalf("CommitCheckRuns = %+v, want two runs, ETag \"runs-1\", neither 304 nor full", page)
	}
	if page.Objects[1]["name"] != "lint" || page.Objects[1]["conclusion"] != "failure" {
		t.Fatalf("the second run decoded as %+v, want GitHub's own object", page.Objects[1])
	}
}

// A page of exactly a hundred runs is full: another page may follow it.
func TestCommitCheckRunsAFullPageIsFull(t *testing.T) {
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, checkObjects("check_runs", manyObjects(objectsPerPage)))
	})

	page, err := client.CommitCheckRuns(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
	if err != nil {
		t.Fatalf("CommitCheckRuns: %v", err)
	}
	if !page.Full || len(page.Objects) != objectsPerPage {
		t.Fatalf("a page of %d runs: Full=%v, want a full page of %d", len(page.Objects), page.Full, objectsPerPage)
	}
}

// CommitCheckSuites reads the commit's suites a hundred to a page, and GitHub's 304 is the page
// NotModified, with the ETag GitHub repeated and no objects.
func TestCommitCheckSuitesReadsThePageAndA304(t *testing.T) {
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"suites-1"`)
		if r.Header.Get("If-None-Match") == `"suites-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, checkObjects("check_suites", []map[string]any{
			{"id": float64(21), "conclusion": "success", "app": map[string]any{"slug": "github-actions"}},
		}))
	})
	ctx := context.Background()

	page, err := client.CommitCheckSuites(ctx, "installation-token", "acme", "web", "c0ffee", 1, "")
	if err != nil {
		t.Fatalf("CommitCheckSuites: %v", err)
	}
	sent := fake.last(t)
	if sent.path != "/repos/acme/web/commits/c0ffee/check-suites" || sent.query != "per_page=100&page=1" {
		t.Fatalf("CommitCheckSuites asked for %s?%s, want the commit's check suites, 100 a page, page 1", sent.path, sent.query)
	}
	if len(page.Objects) != 1 || page.Objects[0]["id"] != float64(21) || page.ETag != `"suites-1"` || page.NotModified || page.Full {
		t.Fatalf("CommitCheckSuites = %+v, want one suite with ETag \"suites-1\"", page)
	}

	unchanged, err := client.CommitCheckSuites(ctx, "installation-token", "acme", "web", "c0ffee", 1, page.ETag)
	if err != nil {
		t.Fatalf("CommitCheckSuites again: %v", err)
	}
	if !unchanged.NotModified || len(unchanged.Objects) != 0 || unchanged.ETag != `"suites-1"` {
		t.Fatalf("the unchanged read = %+v, want 304 with no objects and the same ETag", unchanged)
	}
}

// OpenPullRequests reads the repository's open pull requests, a hundred to a page, and answers
// each one's number and head commit; GitHub's 304 is the page NotModified.
func TestOpenPullRequestsReadsTheNumbersAndHeads(t *testing.T) {
	pulls := []map[string]any{
		{"number": float64(7), "head": map[string]any{"sha": "aaa111"}},
		{"number": float64(9), "head": map[string]any{"sha": "bbb222"}},
	}
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"pulls-1"`)
		if r.Header.Get("If-None-Match") == `"pulls-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(pulls)
	})
	ctx := context.Background()

	page, err := client.OpenPullRequests(ctx, "installation-token", "acme", "web", 1, "")
	if err != nil {
		t.Fatalf("OpenPullRequests: %v", err)
	}
	sent := fake.last(t)
	if sent.path != "/repos/acme/web/pulls" || sent.query != "state=open&per_page=100&page=1" {
		t.Fatalf("OpenPullRequests asked for %s?%s, want the open pull requests, 100 a page, page 1", sent.path, sent.query)
	}
	want := []PullRequestHead{{Number: 7, HeadSHA: "aaa111"}, {Number: 9, HeadSHA: "bbb222"}}
	if page.ETag != `"pulls-1"` || page.NotModified || page.Full || len(page.PullRequests) != len(want) {
		t.Fatalf("OpenPullRequests = %+v, want two pull requests with ETag \"pulls-1\"", page)
	}
	for i, head := range want {
		if page.PullRequests[i] != head {
			t.Fatalf("pull request %d = %+v, want %+v", i, page.PullRequests[i], head)
		}
	}

	unchanged, err := client.OpenPullRequests(ctx, "installation-token", "acme", "web", 1, page.ETag)
	if err != nil {
		t.Fatalf("OpenPullRequests again: %v", err)
	}
	if !unchanged.NotModified || len(unchanged.PullRequests) != 0 || unchanged.ETag != `"pulls-1"` {
		t.Fatalf("the unchanged read = %+v, want 304 with no pull requests and the same ETag", unchanged)
	}
}

// A page of exactly a hundred pull requests is full.
func TestOpenPullRequestsAFullPageIsFull(t *testing.T) {
	pulls := make([]map[string]any, objectsPerPage)
	for i := range pulls {
		pulls[i] = map[string]any{"number": float64(i + 1), "head": map[string]any{"sha": fmt.Sprintf("%040d", i)}}
	}
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pulls)
	})

	page, err := client.OpenPullRequests(context.Background(), "installation-token", "acme", "web", 1, "")
	if err != nil {
		t.Fatalf("OpenPullRequests: %v", err)
	}
	if !page.Full || len(page.PullRequests) != objectsPerPage {
		t.Fatalf("a page of %d pull requests: Full=%v, want a full page of %d", len(page.PullRequests), page.Full, objectsPerPage)
	}
}

// RepositoryIdentity reads the repository itself for the name GitHub gives it and its id, and
// GitHub's 304 is that answer NotModified.
func TestRepositoryIdentityReadsTheNameAndID(t *testing.T) {
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"repo-1"`)
		if r.Header.Get("If-None-Match") == `"repo-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, `{"id":4242,"full_name":"Acme/Web","default_branch":"main"}`)
	})
	ctx := context.Background()

	answer, err := client.RepositoryIdentity(ctx, "installation-token", "acme", "web", "")
	if err != nil {
		t.Fatalf("RepositoryIdentity: %v", err)
	}
	if sent := fake.last(t); sent.path != "/repos/acme/web" || sent.query != "" {
		t.Fatalf("RepositoryIdentity asked for %s?%s, want the repository itself", sent.path, sent.query)
	}
	if answer.FullName != "Acme/Web" || answer.ID != 4242 || answer.ETag != `"repo-1"` || answer.NotModified {
		t.Fatalf("RepositoryIdentity = %+v, want Acme/Web, id 4242, ETag \"repo-1\"", answer)
	}

	unchanged, err := client.RepositoryIdentity(ctx, "installation-token", "acme", "web", answer.ETag)
	if err != nil {
		t.Fatalf("RepositoryIdentity again: %v", err)
	}
	if !unchanged.NotModified || unchanged.FullName != "" || unchanged.ID != 0 || unchanged.ETag != `"repo-1"` {
		t.Fatalf("the unchanged read = %+v, want 304 with no fields and the same ETag", unchanged)
	}
}

// A repository the App cannot see answers 404, which every read here reads as unreadable, never
// as an empty answer.
func TestRepositoryIdentityMissingIsUnreadable(t *testing.T) {
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})

	_, err := client.RepositoryIdentity(context.Background(), "installation-token", "acme", "web", "")
	if !errors.Is(err, ErrRepositoryUnreadable) || !strings.Contains(err.Error(), "acme/web") {
		t.Fatalf("RepositoryIdentity of a repository GitHub does not show: %v, want ErrRepositoryUnreadable naming acme/web", err)
	}
}

// The polled reads are bounded by pollResponseLimit, not by responseLimit: an answer past it is
// the typed error, never a partial page.
func TestAPolledReadPastTheBoundIsTooLarge(t *testing.T) {
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", pollResponseLimit+1))
	})

	_, err := client.CommitCheckRuns(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
	var tooLarge *ResponseTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != pollResponseLimit {
		t.Fatalf("a check-runs answer past the bound: %v, want a *ResponseTooLargeError at %d", err, int64(pollResponseLimit))
	}
}

// An answer of a megabyte, which responseLimit would refuse, is read whole: a head with many
// check runs is what this bound exists for.
func TestAPolledReadReadsPastTheGenericLimit(t *testing.T) {
	objects := manyObjects(10)
	for _, object := range objects {
		object["output"] = map[string]any{"text": strings.Repeat("y", 1<<17)}
	}
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, checkObjects("check_runs", objects))
	})

	page, err := client.CommitCheckRuns(context.Background(), "installation-token", "acme", "web", "c0ffee", 1, "")
	if err != nil {
		t.Fatalf("CommitCheckRuns of a commit with large outputs: %v", err)
	}
	if len(page.Objects) != len(objects) {
		t.Fatalf("read %d runs, want %d", len(page.Objects), len(objects))
	}
}

// A rate limit GitHub answers one of these reads with is the typed *RateLimitError, and is
// recorded under the token's installation in the limits every process shares, so no process asks
// GitHub again inside it.
func TestAPolledReadsRateLimitIsRecorded(t *testing.T) {
	fake := startLimitGitHub(t)
	database := storetest.Open(t)
	limits := ratelimit.New(database.Pool)
	client := fake.newClient(t, WithLimits(limits))
	ctx := context.Background()
	token, err := client.RepositoryToken(ctx, "acme", "web")
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}
	fake.limit("/repos/acme/web/commits/c0ffee/check-runs")

	_, err = client.CommitCheckRuns(ctx, token, "acme", "web", "c0ffee", 1, "")
	var limited *RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("a rate-limited check-runs read: %v, want a *RateLimitError", err)
	}
	if limited.Held || limited.Status != http.StatusForbidden {
		t.Fatalf("the limit = %+v, want GitHub's own 403, not one held before the request", limited)
	}
	wait, _, err := limits.Wait(ctx, InstallationScope(7))
	if err != nil {
		t.Fatalf("read the recorded limit: %v", err)
	}
	if wait <= 0 {
		t.Fatalf("the recorded limit has %s left, want GitHub's own wait", wait)
	}
}

// A call's shared rate-limit scope is its installation's when this client minted the token, and
// none when it did not: nothing then says whose budget the call spends.
func TestInstallationScopeNamesTheInstallationATokenWasMintedFor(t *testing.T) {
	if got := InstallationScope(7); got != "installation:7" {
		t.Fatalf("InstallationScope(7) = %q, want %q", got, "installation:7")
	}
	fake := startLimitGitHub(t)
	client := fake.newClient(t)
	token, err := client.RepositoryToken(context.Background(), "acme", "web")
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}
	if got := client.TokenScope(token); got != InstallationScope(7) {
		t.Fatalf("TokenScope of a minted token = %q, want %q", got, InstallationScope(7))
	}
	if got := client.TokenScope("ghs_someone_elses"); got != "" {
		t.Fatalf("TokenScope of a token this client never minted = %q, want none", got)
	}
}
