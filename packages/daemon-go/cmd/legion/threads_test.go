package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var (
	stateField = regexp.MustCompile(`,"state":"[A-Z]+"`)
	stateWord  = regexp.MustCompile(`\bstate\b`)
)

// fakeThreadsGitHub serves page for every reviewThreads query and records each resolved thread id.
// Like GitHub, it answers only the fields a query selects: each newest comment's "state" is dropped
// unless the query's newest selection names it.
func fakeThreadsGitHub(t *testing.T, page string) (url string, resolved func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(request.Query, "resolveReviewThread") {
			ids = append(ids, request.Variables["threadId"].(string))
			_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`)
			return
		}
		served := page
		if !selectsNewestState(request.Query) {
			served = stateField.ReplaceAllString(page, "")
		}
		_, _ = io.WriteString(w, served)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ids...)
	}
}

func selectsNewestState(query string) bool {
	for _, line := range strings.Split(query, "\n") {
		if strings.Contains(line, "newest:") {
			return stateWord.MatchString(line)
		}
	}
	return false
}

// legionLogins is the daemon's gh-token answer naming both of Legion's role Apps by App role.
const legionLogins = `,"legionAppLogins":{"implement":"legion-implementer[bot]","review":"legion-reviewer[bot]"}`

// runThreadsResolve redeems a stub grant and runs `legion threads resolve` against githubURL.
func runThreadsResolve(t *testing.T, githubURL string) (code int, stdout, stderr string) {
	t.Helper()
	return runThreadsResolveNaming(t, githubURL, legionLogins)
}

// runThreadsResolveNaming is runThreadsResolve with the daemon's gh-token answer carrying logins,
// the JSON fields after appLogin ("" for none).
func runThreadsResolveNaming(t *testing.T, githubURL, logins string) (code int, stdout, stderr string) {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legion/v1/gh-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"token":"grant-token","appLogin":"legion-implementer[bot]"`+logins+`}`)
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("LEGION_DAEMON_URL", daemon.URL)
	t.Setenv("LEGION_GITHUB_GRAPHQL_URL", githubURL)
	t.Setenv("LEGION_GRANT", "one-command-grant")
	var out, errb bytes.Buffer
	code = run(context.Background(), []string{"legion", "threads", "resolve", "--repo", "owner/repo", "--pr", "7"}, &out, &errb)
	return code, out.String(), errb.String()
}

func TestThreadsResolveOnlyAcceptedRepliesByTheOpener(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"accepted","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"  Accepted: fixed","state":"SUBMITTED"}]}},{"id":"open","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/open","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"implementer"},"url":"https://github.test/thread/open","body":"fixed","state":"SUBMITTED"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "resolved https://github.test/thread/accepted") || !strings.Contains(stdout, "left open https://github.test/thread/open") {
		t.Fatalf("stdout = %q", stdout)
	}
	if got := resolved(); len(got) != 1 || got[0] != "accepted" {
		t.Fatalf("resolved = %v, want [accepted]", got)
	}
}

// A caller sees its own drafts in a pending review, and nobody else's, so a PENDING newest comment
// is never an acceptance: the TypeScript CLI's rule (review-threads.ts `acceptedByOpener`).
func TestThreadsResolveNeverCountsAPendingDraft(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"draft","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/draft","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/draft","body":"Accepted: drafted, not yet submitted","state":"PENDING"}]}},{"id":"accepted","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"Accepted: fixed","state":"SUBMITTED"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	want := "left open https://github.test/thread/draft — newest reply by reviewer is an unsubmitted draft in a pending review\nresolved https://github.test/thread/accepted — its opener's acceptance\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if got := resolved(); len(got) != 1 || got[0] != "accepted" {
		t.Fatalf("resolved = %v, want [accepted]", got)
	}
}

// GitHub always answers state when the query selects it; a newest comment without one means the
// query or the response changed shape, and reading it as submitted would resolve on a draft.
func TestThreadsResolveRefusesANewestCommentWithoutState(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"accepted","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/accepted","body":"Accepted: fixed"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 1 {
		t.Fatalf("threads resolve = %d, want 1; stdout %q", code, stdout)
	}
	if want := "legion threads resolve: review thread accepted: its newest comment carried state \"\", not \"PENDING\" or \"SUBMITTED\"\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if got := resolved(); len(got) != 0 {
		t.Fatalf("resolved = %v, want none", got)
	}
}

// The shared vector with review-threads.test.ts: only space, tab, CR and LF may precede Accepted:.
func TestThreadsResolveTrimsOnlySpaceTabCRLFBeforeAccepted(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"ascii","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/ascii","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/ascii","body":" \t\r\nAccepted: fixed","state":"SUBMITTED"}]}},{"id":"nbsp","isResolved":false,"opener":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/nbsp","body":"raise"}]},"newest":{"nodes":[{"author":{"login":"reviewer"},"url":"https://github.test/thread/nbsp","body":"\u00a0Accepted: fixed","state":"SUBMITTED"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	want := "resolved https://github.test/thread/ascii — its opener's acceptance\nleft open https://github.test/thread/nbsp — newest reply by reviewer is not an acceptance\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if got := resolved(); len(got) != 1 || got[0] != "ascii" {
		t.Fatalf("resolved = %v, want [ascii]", got)
	}
}

// An unresolved thread with no comment the caller can read is refused, as the TypeScript CLI does.
func TestThreadsResolveRefusesAThreadWithNoComments(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"empty","isResolved":false,"opener":{"nodes":[]},"newest":{"nodes":[]}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`)
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 1 {
		t.Fatalf("threads resolve = %d, want 1; stdout %q", code, stdout)
	}
	if want := "legion threads resolve: review thread empty has no comments\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if got := resolved(); len(got) != 0 {
		t.Fatalf("resolved = %v, want none", got)
	}
}

func TestThreadsResolveRejectsInvalidArgumentsBeforeGrantRedemption(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"resolve", "--repo", "not-a-repo", "--pr", "7"},
		// A dot segment names no repository: the configured repo and workspace-init refuse it too.
		{"resolve", "--repo", "owner/..", "--pr", "7"},
		{"resolve", "--repo", "owner/repo", "--pr", "zero"},
		{"other"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "threads"}, args...), &out, &errb)
		if code != 2 {
			t.Fatalf("threads %v = %d, want 2; stderr %q", args, code, errb.String())
		}
		if !strings.Contains(errb.String(), "legion threads") {
			t.Fatalf("threads %v stderr %q does not name the command", args, errb.String())
		}
	}
}

// threadVector is one unresolved review thread as GitHub's GraphQL serves it: the opener's type
// and login (a Bot's is its bare slug), and the newest comment's author, body and state.
type threadVector struct {
	id, openerType, opener, newest, body, state string
}

func (v threadVector) node() string {
	quote := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return `{"id":"` + v.id + `","isResolved":false,"opener":{"nodes":[{"author":{"__typename":"` + v.openerType + `","login":"` + v.opener + `"},"url":"https://github.test/thread/` + v.id + `"}]},"newest":{"nodes":[{"author":{"login":"` + v.newest + `"},"url":"https://github.test/thread/` + v.id + `","body":` + quote(v.body) + `,"state":"` + v.state + `"}]}}`
}

// threadsPage is a reviewThreads page of the vectors' threads.
func threadsPage(vectors ...threadVector) string {
	nodes := make([]string, 0, len(vectors))
	for _, vector := range vectors {
		nodes = append(nodes, vector.node())
	}
	return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` + strings.Join(nodes, ",") + `],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
}

// The shared vector with review-threads.test.ts for a bot's threads. The subject of a finding never
// closes it. A thread a Bot opened that is none of Legion's role Apps (a CI bot, or a person whose
// gh is routed to an App: GitHub cannot tell them apart) closes on its opener's Accepted:, or on
// Legion's review App's, the independent party, and the resolved line says which. The pull
// request author's reply (Fixed in, Declined) closes nothing. The review App's Accepted: counts
// only as the first line of a submitted comment, after space, tab, CR or LF alone, whatever the
// login's case. A thread either Legion App opened closes only on its opener's Accepted:, and a
// person's thread is unchanged.
func TestThreadsResolveClosesABotsThreadOnTheLegionReviewersAcceptance(t *testing.T) {
	vectors := []threadVector{
		{id: "reviewer-accepts", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Accepted: fixed in 1a2b3c4 — moved the guard", state: "SUBMITTED"},
		{id: "reviewer-accepts-any-case", openerType: "Bot", opener: "claude", newest: "Legion-Reviewer", body: " \t\r\nAccepted: not a defect — the loop is bounded", state: "SUBMITTED"},
		{id: "author-declined", openerType: "Bot", opener: "claude", newest: "legion-implementer", body: "Declined: the loop is bounded", state: "SUBMITTED"},
		{id: "author-fixed", openerType: "Bot", opener: "claude", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
		{id: "author-accepts", openerType: "Bot", opener: "claude", newest: "legion-implementer", body: "Accepted: my own fix", state: "SUBMITTED"},
		{id: "reviewer-still-open", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Still open: the loop is not bounded", state: "SUBMITTED"},
		{id: "reviewer-nbsp", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "\u00a0Accepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-second-line", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Thanks.\nAccepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-draft", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Accepted: drafted", state: "PENDING"},
		{id: "routed-person", openerType: "Bot", opener: "sjawhar-agent", newest: "legion-reviewer", body: "Accepted: fixed in 1a2b3c4 — moved the guard", state: "SUBMITTED"},
		{id: "routed-person-own", openerType: "Bot", opener: "sjawhar-agent", newest: "sjawhar-agent", body: "Accepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-thread", openerType: "Bot", opener: "legion-reviewer", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
		{id: "implementer-app-thread", openerType: "Bot", opener: "legion-implementer", newest: "legion-reviewer", body: "Accepted: fine", state: "SUBMITTED"},
		{id: "human", openerType: "User", opener: "octocat", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
	}
	github, resolved := fakeThreadsGitHub(t, threadsPage(vectors...))
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	byReviewer := " — the Legion reviewer's acceptance of a bot's thread\n"
	notEither := "not its opener's or the Legion reviewer's acceptance\n"
	want := "resolved https://github.test/thread/reviewer-accepts" + byReviewer +
		"resolved https://github.test/thread/reviewer-accepts-any-case" + byReviewer +
		"left open https://github.test/thread/author-declined — newest reply by legion-implementer is " + notEither +
		"left open https://github.test/thread/author-fixed — newest reply by legion-implementer is " + notEither +
		"left open https://github.test/thread/author-accepts — newest reply by legion-implementer is " + notEither +
		"left open https://github.test/thread/reviewer-still-open — newest reply by legion-reviewer is " + notEither +
		"left open https://github.test/thread/reviewer-nbsp — newest reply by legion-reviewer is " + notEither +
		"left open https://github.test/thread/reviewer-second-line — newest reply by legion-reviewer is " + notEither +
		"left open https://github.test/thread/reviewer-draft — newest reply by legion-reviewer is an unsubmitted draft in a pending review\n" +
		"resolved https://github.test/thread/routed-person" + byReviewer +
		"resolved https://github.test/thread/routed-person-own — its opener's acceptance\n" +
		"left open https://github.test/thread/reviewer-thread — newest reply by legion-implementer is not an acceptance\n" +
		"left open https://github.test/thread/implementer-app-thread — newest reply by legion-reviewer is not an acceptance\n" +
		"left open https://github.test/thread/human — newest reply by legion-reviewer is not an acceptance\n"
	if stdout != want {
		t.Fatalf("stdout = %q\nwant     %q", stdout, want)
	}
	if got := strings.Join(resolved(), ","); got != "reviewer-accepts,reviewer-accepts-any-case,routed-person,routed-person-own" {
		t.Fatalf("resolved = %s, want reviewer-accepts,reviewer-accepts-any-case,routed-person,routed-person-own", got)
	}
}

// Which accounts are Legion's own, and which is its review App, is the daemon's to say. A daemon
// whose gh-token answer names no Legion App logins (one that could not read one) leaves every
// bot's thread to its opener's Accepted:, and the left-open line says the session cannot tell,
// rather than that the reply was not an acceptance. A daemon names every App or none: an answer
// naming some is refused as invalid, as the contract refuses it.
func TestThreadsResolveAppliesNoBotRuleWithoutLegionsAppLogins(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, threadsPage(threadVector{id: "ci-bot", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"}))
	if code, stdout, stderr := runThreadsResolveNaming(t, github, `,"legionAppLogins":{"implement":"legion-implementer[bot]"}`); code != 1 || stdout != "" ||
		stderr != "legion threads resolve: daemon returned an invalid GitHub credential response\n" || len(resolved()) != 0 {
		t.Fatalf("a partial login answer = %d %q %q, resolved %v; want it refused as invalid", code, stdout, stderr, resolved())
	}
	github, resolved = fakeThreadsGitHub(t, threadsPage(
		threadVector{id: "ci-bot", openerType: "Bot", opener: "claude", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
		threadVector{id: "human", openerType: "User", opener: "octocat", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
	))
	code, stdout, stderr := runThreadsResolveNaming(t, github, "")
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	want := "left open https://github.test/thread/ci-bot — newest reply by legion-reviewer is not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:\n" +
		"left open https://github.test/thread/human — newest reply by legion-implementer is not an acceptance\n"
	if stdout != want || len(resolved()) != 0 {
		t.Fatalf("stdout = %q, resolved %v; want both left open", stdout, resolved())
	}
}
