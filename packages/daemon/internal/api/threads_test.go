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

// reviewThread is one unresolved thread as GitHub's GraphQL answers it: who opened it, and who
// wrote its newest submitted comment and what it says. A login is GraphQL's, a Bot's bare slug.
type reviewThread struct {
	id, openerType, opener, newestType, newest, body string
}

// threadsGitHub is GitHub's GraphQL holding threads on whichever pull request it is asked for. It
// records the bearer of every call, each pull request a threads query names, and the id of each
// thread a resolveReviewThread resolved; it refuses to resolve each thread refuse names, and serves
// the newest comment of each thread pending names as a draft in a pending review.
type threadsGitHub struct {
	url      string
	mu       sync.Mutex
	refuse   map[string]bool
	pending  map[string]bool
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
			state := "SUBMITTED"
			if g.pending[thread.id] {
				state = "PENDING"
			}
			nodes = append(nodes, map[string]any{"id": thread.id, "isResolved": false,
				"opener": map[string]any{"nodes": []map[string]any{{"url": "https://github.com/acme/widgets/pull/42#" + thread.id,
					"author": map[string]any{"__typename": thread.openerType, "login": thread.opener}}}},
				"newest": map[string]any{"nodes": []map[string]any{{"url": "https://github.com/acme/widgets/pull/42#" + thread.id + "-newest", "body": thread.body, "state": state,
					"author": map[string]any{"__typename": thread.newestType, "login": thread.newest}}}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}}}}})
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

// newThreadsHarness serves the daemon's routes over a real Postgres holding LEGION-208's pull
// request acme/widgets#42 and LEGION-209's #43, with github as GitHub's GraphQL, and logs to log.
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
		for number, key := range map[int]string{42: "LEGION-208", 43: "LEGION-209"} {
			if err := records.PutIssue(context.Background(), tx, record.Issue{Key: key, Tree: key, Project: testProject, Title: key,
				Phase: phase.Reviewing, Status: "needs_review", Generation: 1}); err != nil {
				return err
			}
			if err := records.PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: key,
				Repo: "acme/widgets", Number: number, Branch: "legion/" + key, HeadSHA: "head"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the pull requests: %v", err)
	}
	return h
}

// The reviewer cannot resolve a thread on the implementer's pull request, so the daemon does it for
// the reviewer, as the implement App: exactly the threads a bot outside Legion's role Apps opened
// whose newest submitted comment is the review App's Accepted:. A thread a Legion App opened, even
// one its opener accepted, and one whose newest comment is anything else - the reviewer's Still
// open:, or the pull request author's own Accepted: - stay open, each named with why. Every
// resolution is logged with the thread and whose acceptance closed it, and the implement App's
// token goes to GitHub alone, never back to the reviewer.
func TestTheDaemonResolvesForTheReviewerOnlyTheBotThreadsItAccepted(t *testing.T) {
	github := newThreadsGitHub(t,
		reviewThread{"bot-accepted", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: not a defect — the gate runs after the last round"},
		reviewThread{"bot-still-open", "Bot", "claude", "Bot", "legion-reviewer", "Still open: the record is missing"},
		reviewThread{"bot-author-accepted", "Bot", "claude", "Bot", "legion-implementer", "Accepted: fixed in abc123"},
		reviewThread{"reviewer-opened", "Bot", "legion-reviewer", "Bot", "legion-reviewer", "Accepted: fixed in abc123"},
		reviewThread{"implementer-opened", "Bot", "legion-implementer", "Bot", "legion-reviewer", "Accepted: fine"},
		reviewThread{"person", "User", "octocat", "Bot", "legion-reviewer", "Accepted: not a defect"},
	)
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", ThreadsResolveRequest{GrantID: reviewer.grant(t), Repo: "acme/widgets", Number: 42}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("threads resolve = %d: %s", recorder.Code, recorder.Body)
	}
	var answer ThreadsResolveResponse
	decodeInto(t, recorder, &answer)
	var resolved []reviewthreads.Outcome
	for _, outcome := range answer.Threads {
		if outcome.Resolved != "" {
			resolved = append(resolved, outcome)
		} else if outcome.LeftOpen == "" {
			t.Errorf("outcome %+v neither resolved nor says why it was left open", outcome)
		}
	}
	if len(answer.Threads) != 6 || len(resolved) != 1 || resolved[0].URL != "https://github.com/acme/widgets/pull/42#bot-accepted" ||
		resolved[0].Resolved != reviewthreads.ReviewersAcceptanceOfABot {
		t.Fatalf("outcomes %+v, want six, the accepted bot thread alone resolved", answer.Threads)
	}
	if !slices.Equal(github.queried, []string{"acme/widgets#42"}) || !slices.Equal(github.resolved, []string{"bot-accepted"}) {
		t.Fatalf("GitHub was asked for %v and resolved %v, want #42's threads and the accepted bot thread alone", github.queried, github.resolved)
	}
	for _, bearer := range github.bearers {
		if bearer != "Bearer implement-token" {
			t.Fatalf("GitHub was called with %q, want the implement App's token on every call", bearer)
		}
	}
	if strings.Contains(recorder.Body.String(), "implement-token") {
		t.Fatalf("the answer %s carries the implement App's token", recorder.Body)
	}
	if line := log.String(); !strings.Contains(line, "thread=https://github.com/acme/widgets/pull/42#bot-accepted") ||
		!strings.Contains(line, `by="the Legion reviewer's acceptance of a bot's thread"`) || strings.Contains(line, "bot-still-open") {
		t.Fatalf("log %q, want the one resolution with its thread and whose acceptance closed it", line)
	}
}

// GitHub refusing to resolve a thread stops the run, and the answer names the refused thread beside
// the outcomes before it, the thread already resolved among them, so the reviewer sees what the
// daemon did; the threads after it are neither resolved nor named.
func TestADaemonResolveGitHubRefusesKeepsTheThreadsAlreadyResolved(t *testing.T) {
	github := newThreadsGitHub(t,
		reviewThread{"first", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: not a defect"},
		reviewThread{"second", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: fixed in abc123"},
		reviewThread{"third", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: fine"},
	)
	github.refuse = map[string]bool{"second": true}
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", ThreadsResolveRequest{GrantID: reviewer.grant(t), Repo: "acme/widgets", Number: 42}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("threads resolve = %d: %s", recorder.Code, recorder.Body)
	}
	var answer ThreadsResolveResponse
	decodeInto(t, recorder, &answer)
	want := ThreadsResolveResponse{
		Threads: []reviewthreads.Outcome{{URL: "https://github.com/acme/widgets/pull/42#first", Resolved: reviewthreads.ReviewersAcceptanceOfABot, NewestBy: "legion-reviewer"}},
		Refused: &ThreadRefusal{URL: "https://github.com/acme/widgets/pull/42#second", Error: "GitHub: Resource not accessible by integration"},
	}
	if !slices.Equal(answer.Threads, want.Threads) || answer.Refused == nil || *answer.Refused != *want.Refused {
		t.Fatalf("answer %+v (refused %+v), want %+v (refused %+v)", answer, answer.Refused, want, want.Refused)
	}
	if !slices.Equal(github.resolved, []string{"first"}) {
		t.Fatalf("GitHub resolved %v, want the first thread alone", github.resolved)
	}
}

// GitHub shows a draft in a pending review only to its author, and the daemon reads the threads as
// the implement App, so it sees the implementer's own drafts. The reviewer's answer never names a
// thread whose newest comment is such a draft: not its URL, and not its author.
func TestADaemonResolveNamesNoThreadHoldingTheImplementersDraft(t *testing.T) {
	github := newThreadsGitHub(t,
		reviewThread{"bot-accepted", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: not a defect"},
		reviewThread{"implementer-draft", "Bot", "claude", "Bot", "legion-implementer", "Fixed in abc123: the guard moved"},
	)
	github.pending = map[string]bool{"implementer-draft": true}
	var log bytes.Buffer
	h := newThreadsHarness(t, github, &log)
	reviewer := newLiveClaim(t, h, "LEGION-208", claim.RoleReviewer)
	recorder := h.request(http.MethodPost, "/legion/v1/threads/resolve", ThreadsResolveRequest{GrantID: reviewer.grant(t), Repo: "acme/widgets", Number: 42}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("threads resolve = %d: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	var answer ThreadsResolveResponse
	decodeInto(t, recorder, &answer)
	if len(answer.Threads) != 1 || answer.Threads[0].URL != "https://github.com/acme/widgets/pull/42#bot-accepted" || strings.Contains(body, "implementer-draft") {
		t.Fatalf("answer %s, want the accepted bot thread alone and nothing of the draft's thread", body)
	}
}

// Only the reviewer's grant has the daemon resolve, and only on its own issue's pull request: any
// other role's grant, and a reviewer naming another issue's pull request, are refused before GitHub
// is called, so no role borrows the implement App through the route.
func TestTheDaemonResolvesThreadsOnlyForTheReviewerOnItsOwnPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   claim.Role
		number int
		code   string
	}{
		{"the implementer's grant", claim.RoleImplementer, 42, "REVIEWER_REQUIRED"},
		{"the tester's grant, which acts as the review App too", claim.RoleTester, 42, "REVIEWER_REQUIRED"},
		{"the reviewer naming another issue's pull request", claim.RoleReviewer, 43, "PULL_REQUEST_NOT_THE_ISSUES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newThreadsGitHub(t, reviewThread{"bot-accepted", "Bot", "claude", "Bot", "legion-reviewer", "Accepted: not a defect"})
			var log bytes.Buffer
			h := newThreadsHarness(t, github, &log)
			worker := newLiveClaim(t, h, "LEGION-208", tc.role)
			assertFailure(t, h.request(http.MethodPost, "/legion/v1/threads/resolve", ThreadsResolveRequest{GrantID: worker.grant(t), Repo: "acme/widgets", Number: tc.number}, nil),
				http.StatusForbidden, tc.code)
			if len(github.bearers) != 0 {
				t.Fatalf("GitHub was called %d times, want none", len(github.bearers))
			}
		})
	}
}
