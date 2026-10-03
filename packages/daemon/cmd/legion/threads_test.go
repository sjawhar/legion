package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// threadVector is one unresolved review thread as GitHub's GraphQL serves it: the opener's type and
// login (a Bot's is its bare slug), and the newest comment's author's type and login, body and
// state.
type threadVector struct {
	id, openerType, opener, newestType, newest, body, state string
}

func (v threadVector) node() string {
	quote := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return `{"id":"` + v.id + `","isResolved":false,"opener":{"nodes":[{"author":{"__typename":"` + v.openerType + `","login":"` + v.opener + `"},"url":"https://github.test/thread/` + v.id + `"}]},"newest":{"nodes":[{"author":{"__typename":"` + v.newestType + `","login":"` + v.newest + `"},"url":"https://github.test/thread/` + v.id + `","body":` + quote(v.body) + `,"state":"` + v.state + `"}]}}`
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
// person's thread is unchanged. An account is its type and its login together: a User who
// registered the review App's bare slug is not the review App, nor the Bot opener of that name.
func TestThreadsResolveClosesABotsThreadOnTheLegionReviewersAcceptance(t *testing.T) {
	vectors := []threadVector{
		{id: "reviewer-accepts", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed in 1a2b3c4 — moved the guard", state: "SUBMITTED"},
		{id: "reviewer-accepts-any-case", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "Legion-Reviewer", body: " \t\r\nAccepted: not a defect — the loop is bounded", state: "SUBMITTED"},
		{id: "author-declined", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-implementer", body: "Declined: the loop is bounded", state: "SUBMITTED"},
		{id: "author-fixed", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
		{id: "author-accepts", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-implementer", body: "Accepted: my own fix", state: "SUBMITTED"},
		{id: "reviewer-still-open", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Still open: the loop is not bounded", state: "SUBMITTED"},
		{id: "reviewer-nbsp", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "\u00a0Accepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-second-line", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Thanks.\nAccepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-draft", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: drafted", state: "PENDING"},
		{id: "routed-person", openerType: "Bot", opener: "sjawhar-agent", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed in 1a2b3c4 — moved the guard", state: "SUBMITTED"},
		{id: "routed-person-own", openerType: "Bot", opener: "sjawhar-agent", newestType: "Bot", newest: "sjawhar-agent", body: "Accepted: fixed", state: "SUBMITTED"},
		{id: "reviewer-thread", openerType: "Bot", opener: "legion-reviewer", newestType: "Bot", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
		{id: "implementer-app-thread", openerType: "Bot", opener: "legion-implementer", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fine", state: "SUBMITTED"},
		{id: "human", openerType: "User", opener: "octocat", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
		{id: "impostor-reviewer", openerType: "Bot", opener: "claude", newestType: "User", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
		{id: "impostor-opener", openerType: "Bot", opener: "legion-reviewer", newestType: "User", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
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
		"left open https://github.test/thread/human — newest reply by legion-reviewer is not an acceptance\n" +
		"left open https://github.test/thread/impostor-reviewer — newest reply by legion-reviewer is " + notEither +
		"left open https://github.test/thread/impostor-opener — newest reply by legion-reviewer is not an acceptance\n"
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
	github, resolved := fakeThreadsGitHub(t, threadsPage(threadVector{id: "ci-bot", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"}))
	if code, stdout, stderr := runThreadsResolveNaming(t, github, `,"legionAppLogins":{"implement":"legion-implementer[bot]"}`); code != 1 || stdout != "" ||
		!strings.HasPrefix(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response") || len(resolved()) != 0 {
		t.Fatalf("a partial login answer = %d %q %q, resolved %v; want it refused as invalid", code, stdout, stderr, resolved())
	}
	github, resolved = fakeThreadsGitHub(t, threadsPage(
		threadVector{id: "ci-bot", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
		threadVector{id: "human", openerType: "User", opener: "octocat", newestType: "Bot", newest: "legion-implementer", body: "Fixed in 1a2b3c4: moved the guard", state: "SUBMITTED"},
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

// standinGh is a gh first on PATH that records each call under its directory (its arguments, its
// GH_REPO and the request on its standard input) and answers as `gh api graphql` does: page for a
// reviewThreads query, a resolved thread for the mutation. Every call prints ghNotice on stderr, as
// a gh that picks its credential per call does when the call acts as someone else; once fail()
// runs, it exits 4 with GitHub's refusal instead. calls reads back what each call received.
type standinGh struct {
	t   *testing.T
	dir string
}

const ghNotice = "gh: this call acts as the fallback personal token\n"

func newStandinGh(t *testing.T, page string) standinGh {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$GH_STANDIN_DIR
n=$(($(cat "$dir/calls") + 1))
echo "$n" > "$dir/calls"
cat > "$dir/request-$n"
printf '%s\n' "$*" > "$dir/argv-$n"
printf '%s\n' "${GH_REPO-unset}" > "$dir/gh-repo-$n"
if [ -e "$dir/fail" ]; then
  echo 'HTTP 401: Bad credentials (https://api.github.com/graphql)' >&2
  exit 4
fi
printf %s '` + ghNotice + `' >&2
if grep -q resolveReviewThread "$dir/request-$n"; then
  echo '{"data":{"resolveReviewThread":{"thread":{"id":"resolved","isResolved":true}}}}'
else
  cat "$dir/page"
fi
`
	for name, contents := range map[string]string{"gh": script, "calls": "0\n", "page": page} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GH_STANDIN_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return standinGh{t: t, dir: dir}
}

func (g standinGh) fail() {
	if err := os.WriteFile(filepath.Join(g.dir, "fail"), nil, 0o600); err != nil {
		g.t.Fatal(err)
	}
}

// ghCall is what one gh call received.
type ghCall struct {
	argv, ghRepo string
	request      struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
}

func (g standinGh) calls() []ghCall {
	g.t.Helper()
	read := func(name string) string {
		contents, err := os.ReadFile(filepath.Join(g.dir, name))
		if err != nil {
			g.t.Fatal(err)
		}
		return strings.TrimSpace(string(contents))
	}
	var calls []ghCall
	for n := 1; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		if _, err := os.Stat(filepath.Join(g.dir, "argv"+suffix)); os.IsNotExist(err) {
			return calls
		}
		call := ghCall{argv: read("argv" + suffix), ghRepo: read("gh-repo" + suffix)}
		if err := json.Unmarshal([]byte(read("request"+suffix)), &call.request); err != nil {
			g.t.Fatalf("gh call %d's request is not JSON: %v", n, err)
		}
		calls = append(calls, call)
	}
}

// outsideAPane is a session no Legion pane started: no grant of any kind, and a daemon and a GitHub
// endpoint that fail the test if anything reaches them, run from a directory that is no checkout.
func outsideAPane(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LEGION_GRANT_FILE", "LEGION_GRANT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	untouched := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("%s %s was requested; --gh redeems no grant and calls GitHub only through gh", r.Method, r.URL)
	}))
	t.Cleanup(untouched.Close)
	t.Setenv("LEGION_DAEMON_URL", untouched.URL)
	t.Setenv("LEGION_GITHUB_GRAPHQL_URL", untouched.URL)
	t.Chdir(t.TempDir())
}

func runThreadsResolveWithGh(t *testing.T) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), []string{"legion", "threads", "resolve", "--repo", "owner/repo", "--pr", "7", "--gh"}, &out, &errb)
	return code, out.String(), errb.String()
}

// A session outside a Legion pane adds --gh: the same rule runs through its own gh, which gets
// GH_REPO so it authenticates for the repository from any directory, and whose stderr is shown on
// success too. With no grant there are no Legion App logins, so no thread counts as a bot's.
func TestThreadsResolveWithGhResolvesThroughTheSessionsOwnGh(t *testing.T) {
	gh := newStandinGh(t, threadsPage(
		threadVector{id: "accepted", openerType: "User", opener: "reviewer", newestType: "User", newest: "reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
		threadVector{id: "ci-bot", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed", state: "SUBMITTED"},
	))
	outsideAPane(t)
	code, stdout, stderr := runThreadsResolveWithGh(t)
	if code != 0 {
		t.Fatalf("threads resolve --gh = %d: %s", code, stderr)
	}
	want := "resolved https://github.test/thread/accepted — its opener's acceptance\n" +
		"left open https://github.test/thread/ci-bot — newest reply by legion-reviewer is not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:\n"
	if stdout != want {
		t.Fatalf("stdout = %q\nwant     %q", stdout, want)
	}
	if stderr != ghNotice+ghNotice {
		t.Fatalf("stderr = %q, want gh's stderr from both calls, %q each", stderr, ghNotice)
	}
	calls := gh.calls()
	if len(calls) != 2 {
		t.Fatalf("gh ran %d times, want the reviewThreads query and one resolve", len(calls))
	}
	for i, call := range calls {
		if call.argv != "api graphql --input -" || call.ghRepo != "owner/repo" {
			t.Errorf("gh call %d: argv %q, GH_REPO %q; want `api graphql --input -` with GH_REPO owner/repo", i+1, call.argv, call.ghRepo)
		}
	}
	if !strings.Contains(calls[0].request.Query, "reviewThreads") || calls[0].request.Variables["owner"] != "owner" || calls[0].request.Variables["name"] != "repo" || calls[0].request.Variables["number"] != float64(7) {
		t.Errorf("gh's first request = %+v, want the reviewThreads query for owner/repo#7", calls[0].request)
	}
	if !strings.Contains(calls[1].request.Query, "resolveReviewThread") || calls[1].request.Variables["threadId"] != "accepted" {
		t.Errorf("gh's second request = %+v, want resolveReviewThread for accepted", calls[1].request)
	}
}

// A gh that fails is named with its exit status and its own message, and nothing is resolved.
func TestThreadsResolveWithGhNamesAFailedGh(t *testing.T) {
	gh := newStandinGh(t, threadsPage())
	gh.fail()
	outsideAPane(t)
	code, stdout, stderr := runThreadsResolveWithGh(t)
	want := "legion threads resolve: gh api graphql failed (exit 4): HTTP 401: Bad credentials (https://api.github.com/graphql)\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("threads resolve --gh = %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if calls := gh.calls(); len(calls) != 1 {
		t.Fatalf("gh ran %d times, want only the failed query", len(calls))
	}
}

// In a Legion pane, which names its grant file, --gh is refused before anything runs: the pane's
// gh is `legion gh`, and the pane has its grant.
func TestThreadsResolveRefusesGhInALegionPane(t *testing.T) {
	gh := newStandinGh(t, threadsPage())
	outsideAPane(t)
	t.Setenv("LEGION_GRANT_FILE", filepath.Join(t.TempDir(), "grant"))
	code, stdout, stderr := runThreadsResolveWithGh(t)
	want := "legion threads resolve: --gh is for a session outside a Legion pane; this pane names a grant (LEGION_GRANT_FILE), so run legion threads resolve without --gh\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("threads resolve --gh in a pane = %d, stdout %q, stderr %q; want 1 and %q", code, stdout, stderr, want)
	}
	if calls := gh.calls(); len(calls) != 0 {
		t.Fatalf("gh ran %d times in a pane, want never", len(calls))
	}
}

// A session with no grant that leaves out --gh is told it can add it.
func TestThreadsResolveWithoutAGrantNamesGh(t *testing.T) {
	outsideAPane(t)
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "threads", "resolve", "--repo", "owner/repo", "--pr", "7"}, &out, &errb)
	want := "legion threads resolve: Unable to redeem LEGION_GRANT: LEGION_GRANT_FILE is missing (and LEGION_GRANT is unset); a session outside a Legion pane has no grant and adds --gh to resolve through its own gh\n"
	if code != 1 || errb.String() != want {
		t.Fatalf("threads resolve with no grant = %d, stderr %q; want 1 and %q", code, errb.String(), want)
	}
}
