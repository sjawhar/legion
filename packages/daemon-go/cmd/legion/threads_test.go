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

// legionLogins is the daemon's gh-token answer naming both of Legion's role Apps.
const legionLogins = `,"legionAppLogins":["legion-implementer[bot]","legion-reviewer[bot]"]`

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
	want := "left open https://github.test/thread/draft — newest reply by reviewer is an unsubmitted draft in a pending review\nresolved https://github.test/thread/accepted\n"
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
	want := "resolved https://github.test/thread/ascii\nleft open https://github.test/thread/nbsp — newest reply by reviewer is not an acceptance\n"
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

// botThread is one unresolved review thread as GitHub's GraphQL serves it: the opener's type and
// login (a Bot's is its bare slug), and the newest comment's author, body and state.
func botThread(id, openerType, opener, newest, body, state string) string {
	quote := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return `{"id":"` + id + `","isResolved":false,"opener":{"nodes":[{"author":{"__typename":"` + openerType + `","login":"` + opener + `"},"url":"https://github.test/thread/` + id + `"}]},"newest":{"nodes":[{"author":{"login":"` + newest + `"},"url":"https://github.test/thread/` + id + `","body":` + quote(body) + `,"state":"` + state + `"}]}}`
}

// botThreadsPage is a reviewThreads page of nodes on a pull request legion-implementer authored.
func botThreadsPage(nodes ...string) string {
	return `{"data":{"repository":{"pullRequest":{"author":{"login":"legion-implementer"},"reviewThreads":{"nodes":[` + strings.Join(nodes, ",") + `],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`
}

// The shared vector with review-threads.test.ts for a bot's threads. A bot never posts Accepted:,
// so a thread a bot opened closes when the pull request's author answers it, in its newest
// submitted comment, with a disposition as its first line: `Fixed in <commit>: …` or `Declined: …`
// (a disposition below another first line does not count). The bot must be none of Legion's own
// role Apps, which the daemon names: a thread legion-reviewer or legion-implementer opened closes
// only on its opener's Accepted:, whatever its review says, so the implementer's ordinary reply
// never closes a reviewer's finding. A human's thread is unchanged.
func TestThreadsResolveClosesABotsThreadOnTheAuthorsDisposition(t *testing.T) {
	nodes := []string{
		botThread("fixed", "Bot", "claude", "legion-implementer", "Fixed in 1a2b3c4: moved the guard", "SUBMITTED"),
		botThread("declined", "Bot", "claude", "legion-implementer", " \t\r\nDeclined: the loop is bounded", "SUBMITTED"),
		botThread("vague", "Bot", "claude", "legion-implementer", "Addressed it", "SUBMITTED"),
		botThread("second-line", "Bot", "claude", "legion-implementer", "Thanks for the catch.\nDeclined: the loop is bounded", "SUBMITTED"),
		botThread("multiline", "Bot", "claude", "legion-implementer", "Declined: the loop is bounded\r\nThe bound is the page size.", "SUBMITTED"),
		botThread("not-author", "Bot", "claude", "legion-reviewer", "Declined: not ours", "SUBMITTED"),
		botThread("draft", "Bot", "claude", "legion-implementer", "Fixed in 1a2b3c4: drafted", "PENDING"),
		botThread("reviewer", "Bot", "legion-reviewer", "legion-implementer", "Fixed in 1a2b3c4: moved the guard", "SUBMITTED"),
		botThread("implementer-app", "Bot", "legion-implementer", "legion-implementer", "Declined: my own note", "SUBMITTED"),
		botThread("human", "User", "octocat", "legion-implementer", "Fixed in 1a2b3c4: moved the guard", "SUBMITTED"),
	}
	github, resolved := fakeThreadsGitHub(t, botThreadsPage(nodes...))
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	bot := "not the opener's acceptance or the pull request author's disposition (Fixed in <commit>: … or Declined: …)"
	want := "resolved https://github.test/thread/fixed\n" +
		"resolved https://github.test/thread/declined\n" +
		"left open https://github.test/thread/vague — newest reply by legion-implementer is " + bot + "\n" +
		"left open https://github.test/thread/second-line — newest reply by legion-implementer is " + bot + "\n" +
		"resolved https://github.test/thread/multiline\n" +
		"left open https://github.test/thread/not-author — newest reply by legion-reviewer is " + bot + "\n" +
		"left open https://github.test/thread/draft — newest reply by legion-implementer is an unsubmitted draft in a pending review\n" +
		"left open https://github.test/thread/reviewer — newest reply by legion-implementer is not an acceptance\n" +
		"left open https://github.test/thread/implementer-app — newest reply by legion-implementer is not an acceptance\n" +
		"left open https://github.test/thread/human — newest reply by legion-implementer is not an acceptance\n"
	if stdout != want {
		t.Fatalf("stdout = %q\nwant     %q", stdout, want)
	}
	if got := strings.Join(resolved(), ","); got != "fixed,declined,multiline" {
		t.Fatalf("resolved = %s, want fixed,declined,multiline", got)
	}
}

// Which accounts are Legion's own is the daemon's to say. A daemon whose gh-token answer names no
// Legion App logins (one that predates them, or could not read one) leaves every bot's thread to
// the opener's-Accepted: rule: a CI bot's thread the author answered stays open rather than risk
// closing a Legion reviewer's.
func TestThreadsResolveAppliesNoBotRuleWithoutLegionsAppLogins(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, botThreadsPage(
		botThread("ci-bot", "Bot", "claude", "legion-implementer", "Fixed in 1a2b3c4: moved the guard", "SUBMITTED"),
		botThread("reviewer", "Bot", "legion-reviewer", "legion-implementer", "Fixed in 1a2b3c4: moved the guard", "SUBMITTED"),
	))
	code, stdout, stderr := runThreadsResolveNaming(t, github, "")
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	want := "left open https://github.test/thread/ci-bot — newest reply by legion-implementer is not an acceptance\n" +
		"left open https://github.test/thread/reviewer — newest reply by legion-implementer is not an acceptance\n"
	if stdout != want || len(resolved()) != 0 {
		t.Fatalf("stdout = %q, resolved %v; want both left open", stdout, resolved())
	}
}
