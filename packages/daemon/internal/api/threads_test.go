package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewthreads"
)

// roleTokens leases each App a token of its own, so a test can tell which App's token reached
// GitHub.
type roleTokens struct{}

func (roleTokens) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	login := map[appauth.AppRole]string{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}[role]
	return appauth.Lease{Token: string(role) + "-token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: login}}, nil
}

// reviewThread is one review thread as GitHub's GraphQL answers it: its node id, and whether GitHub
// holds it resolved.
type reviewThread struct {
	id       string
	resolved bool
}

// threadsGitHub is GitHub's GraphQL holding threads on whichever pull request it is asked for. It
// records the bearer of every call, each pull request a threads query names, and the id of each
// thread a resolveReviewThread resolved; it refuses to resolve each thread refuse names.
type threadsGitHub struct {
	url      string
	mu       sync.Mutex
	refuse   map[string]bool
	bearers  []string
	queried  []string
	resolved []string
}

func newThreadsGitHub(t *testing.T, threads ...reviewThread) *threadsGitHub {
	t.Helper()
	g := &threadsGitHub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.bearers = append(g.bearers, r.Header.Get("Authorization"))
		if strings.Contains(request.Query, "resolveReviewThread") {
			id := request.Variables["threadId"].(string)
			if g.refuse[id] {
				_, _ = io.WriteString(w, `{"errors":[{"message":"Resource not accessible by integration"}]}`)
				return
			}
			g.resolved = append(g.resolved, id)
			_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`)
			return
		}
		g.queried = append(g.queried, fmt.Sprintf("%s/%s#%v", request.Variables["owner"], request.Variables["name"], request.Variables["number"]))
		nodes := []map[string]any{}
		for _, thread := range threads {
			nodes = append(nodes, map[string]any{"id": thread.id, "isResolved": thread.resolved})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}}}})
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// newThreadsHarness serves the daemon's routes over a real Postgres holding LEGION-208's pull
// request acme/widgets#42 and LEGION-209, which has no pull request recorded, with github as
// GitHub's GraphQL, and logs to log.
func newThreadsHarness(t *testing.T, github *threadsGitHub, log *bytes.Buffer) *harness {
	t.Helper()
	h := newHarness(t)
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, Controller: h.store,
		Pool: h.store.Pool(), Record: record.NewStore(), Tokens: roleTokens{}, GitHubOwner: "acme", GitHubGraphQL: github.url,
		Log: slog.New(slog.NewTextHandler(log, nil)),
	}).Handler
	records := record.NewStore()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		for _, key := range []string{"LEGION-208", "LEGION-209"} {
			if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key, Tree: key, Project: testProject, Title: key,
				Phase: phase.Reviewing, Status: "needs_review", Generation: 1}); err != nil {
				return err
			}
		}
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208",
			Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	}); err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	return h
}

// resolveRequest is a reviewer's request naming threads on its issue's recorded pull request.
func resolveRequest(grant string, threads ...string) ThreadsResolveRequest {
	return ThreadsResolveRequest{GrantID: grant, Threads: threads}
}

// The reviewer cannot resolve a thread on the implementer's pull request, so the daemon does it for
// the reviewer, as the implement App: exactly the threads the reviewer names by node id, in the
// order named, after one listing of the pull request's threads. A thread GitHub already holds
// resolved is answered as resolved with the reason "already resolved" and not written again, so a
// retry after a partial run is idempotent; a thread the reviewer did not name is left alone. Every
// resolution is logged with its thread, and the implement App's token goes to GitHub alone, never
// back to the reviewer.
func TestTheDaemonResolvesForTheReviewerExactlyTheThreadsItNames(t *testing.T) {
	github := newThreadsGitHub(t, reviewThread{"bot-finding", false}, reviewThread{"done-earlier", true}, reviewThread{"still-open", false}, reviewThread{"not-named", false})
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", resolveRequest(reviewer.grant(t), "still-open", "done-earlier", "bot-finding"), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("threads resolve = %d: %s", recorder.Code, recorder.Body)
	}
	var answer ThreadsResolveResponse
	decodeInto(t, recorder, &answer)
	want := []reviewthreads.Outcome{
		{Thread: "still-open", Resolved: true},
		{Thread: "done-earlier", Resolved: true, Reason: reviewthreads.AlreadyResolved},
		{Thread: "bot-finding", Resolved: true},
	}
	if !slices.Equal(answer.Threads, want) || answer.Refused != nil {
		t.Fatalf("answer %+v, want the three named threads in order, the resolved one answered as already resolved", answer)
	}
	if !slices.Equal(github.queried, []string{"acme/widgets#42"}) || !slices.Equal(github.resolved, []string{"still-open", "bot-finding"}) {
		t.Fatalf("GitHub was asked for %v and resolved %v, want #42's threads once and the two open named threads in order", github.queried, github.resolved)
	}
	for _, bearer := range github.bearers {
		if bearer != "Bearer implement-token" {
			t.Fatalf("GitHub was called with %q, want the implement App's token on every call", bearer)
		}
	}
	if strings.Contains(recorder.Body.String(), "implement-token") {
		t.Fatalf("the answer %s carries the implement App's token", recorder.Body)
	}
	if line := log.String(); !strings.Contains(line, "thread=still-open") || !strings.Contains(line, "thread=bot-finding") ||
		strings.Contains(line, "done-earlier") || strings.Contains(line, "not-named") {
		t.Fatalf("log %q, want the two resolutions with their threads and nothing of the others", line)
	}
}

// An id that is no review thread of the issue's pull request refuses the whole request, naming
// every such id, before any write: the named threads that are the pull request's stay as they were,
// so a reviewer who pasted a wrong id resolves nothing by accident, on this pull request or any
// other. A request naming no thread, or an empty id, is refused before GitHub is called.
func TestTheDaemonRefusesAThreadThatIsNotOnThePullRequestBeforeAnyWrite(t *testing.T) {
	github := newThreadsGitHub(t, reviewThread{"ours", false})
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", resolveRequest(reviewer.grant(t), "ours", "theirs", "nowhere"), nil)
	body := recorder.Body.String()
	assertFailure(t, recorder, http.StatusBadRequest, "THREAD_NOT_ON_PULL_REQUEST")
	if !strings.Contains(body, "theirs, nowhere") || !strings.Contains(body, "acme/widgets#42") {
		t.Fatalf("refusal %s, want it to name the foreign ids and the pull request", body)
	}
	if len(github.resolved) != 0 || !slices.Equal(github.queried, []string{"acme/widgets#42"}) {
		t.Fatalf("GitHub resolved %v after %v, want nothing written after the one listing", github.resolved, github.queried)
	}
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{"no threads", `{"grantId":%q}`, "MISSING_FIELD"},
		{"an empty list", `{"grantId":%q,"threads":[]}`, "MISSING_FIELD"},
		{"an empty id", `{"grantId":%q,"threads":["ours",""]}`, "INVALID_THREAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := len(github.bearers)
			assertFailure(t, h.request(http.MethodPost, "/legion/v1/threads/resolve", fmt.Sprintf(tc.body, reviewer.grant(t)), nil), http.StatusBadRequest, tc.code)
			if len(github.bearers) != calls {
				t.Fatalf("GitHub was called %d times, want none", len(github.bearers)-calls)
			}
		})
	}
}

// GitHub refusing to resolve a thread stops the run, and the answer names the refused thread beside
// the outcomes before it, the thread already resolved among them, so the reviewer sees what the
// daemon did; the threads after it are neither resolved nor named.
func TestADaemonResolveGitHubRefusesKeepsTheThreadsAlreadyResolved(t *testing.T) {
	github := newThreadsGitHub(t, reviewThread{"first", false}, reviewThread{"second", false}, reviewThread{"third", false})
	github.refuse = map[string]bool{"second": true}
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", resolveRequest(reviewer.grant(t), "first", "second", "third"), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("threads resolve = %d: %s", recorder.Code, recorder.Body)
	}
	var answer ThreadsResolveResponse
	decodeInto(t, recorder, &answer)
	want := ThreadsResolveResponse{
		Threads: []reviewthreads.Outcome{{Thread: "first", Resolved: true}},
		Refused: &ThreadRefusal{Thread: "second", Error: "GitHub: Resource not accessible by integration"},
	}
	if !slices.Equal(answer.Threads, want.Threads) || answer.Refused == nil || *answer.Refused != *want.Refused {
		t.Fatalf("answer %+v (refused %+v), want %+v (refused %+v)", answer, answer.Refused, want, want.Refused)
	}
	if !slices.Equal(github.resolved, []string{"first"}) {
		t.Fatalf("GitHub resolved %v, want the first thread alone", github.resolved)
	}
}

// Only the reviewer's grant has the daemon resolve, and only on its own issue's recorded pull
// request: any other role's grant is refused, and a reviewer whose issue has no pull request
// recorded has nothing to resolve threads on, both before GitHub is called, so no role borrows the
// implement App through the route.
func TestTheDaemonResolvesThreadsOnlyForTheReviewerOnItsOwnPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issue  string
		role   claim.Role
		status int
		code   string
	}{
		{"the implementer's grant", "LEGION-208", claim.RoleImplementer, http.StatusForbidden, "REVIEWER_REQUIRED"},
		{"the tester's grant, which acts as the review App too", "LEGION-208", claim.RoleTester, http.StatusForbidden, "REVIEWER_REQUIRED"},
		{"the reviewer of an issue with no pull request recorded", "LEGION-209", claim.RoleReviewer, http.StatusConflict, "NO_PULL_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newThreadsGitHub(t, reviewThread{"bot-finding", false})
			var log bytes.Buffer
			h := newThreadsHarness(t, github, &log)
			worker := newLiveClaim(t, h, tc.issue, tc.role)
			recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", resolveRequest(worker.grant(t), "bot-finding"), nil)
			body := recorder.Body.String()
			assertFailure(t, recorder, tc.status, tc.code)
			if tc.code == "NO_PULL_REQUEST" && !strings.Contains(body, "LEGION-209 has no pull request recorded; nothing to resolve threads on") {
				t.Fatalf("refusal %s, want it to name the issue and that there is nothing to resolve threads on", body)
			}
			if len(github.bearers) != 0 {
				t.Fatalf("GitHub was called %d times, want none", len(github.bearers))
			}
		})
	}
}
