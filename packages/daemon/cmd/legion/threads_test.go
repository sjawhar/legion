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
// is never an acceptance: the command reads each newest comment's state to know it.
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

// An unresolved thread with no comment the caller can read is refused.
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

// The daemon's logins reach the rule (reviewthreads.Resolution, whose vectors live with it): a
// bot's thread closes on the Legion reviewer's Accepted:, the resolved line saying so, and the pull
// request author's own Accepted: closes nothing.
func TestThreadsResolveClosesABotsThreadOnTheLegionReviewersAcceptance(t *testing.T) {
	github, resolved := fakeThreadsGitHub(t, threadsPage(
		threadVector{id: "reviewer-accepts", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-reviewer", body: "Accepted: fixed in 1a2b3c4 — moved the guard", state: "SUBMITTED"},
		threadVector{id: "author-accepts", openerType: "Bot", opener: "claude", newestType: "Bot", newest: "legion-implementer", body: "Accepted: my own fix", state: "SUBMITTED"},
	))
	code, stdout, stderr := runThreadsResolve(t, github)
	if code != 0 {
		t.Fatalf("threads resolve = %d: %s", code, stderr)
	}
	want := "resolved https://github.test/thread/reviewer-accepts — the Legion reviewer's acceptance of a bot's thread\n" +
		"left open https://github.test/thread/author-accepts — newest reply by legion-implementer is not its opener's or the Legion reviewer's acceptance\n"
	if stdout != want {
		t.Fatalf("stdout = %q\nwant     %q", stdout, want)
	}
	if got := strings.Join(resolved(), ","); got != "reviewer-accepts" {
		t.Fatalf("resolved = %s, want reviewer-accepts", got)
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

// In the reviewer's pane the command asks the daemon to resolve, since GitHub refuses the review App
// a resolve on the implementer's pull request: it sends the pane's grant and the pull request it
// was given, prints each outcome the daemon answers as the command always prints it, and redeems
// no token of its own. It then prints how many threads the daemon left open unnamed because their
// newest comment is the implement App's pending draft, and any such thread fails the command, as a
// refusal does, since neither closes until someone else acts. A refusal from the daemon fails the
// command with the daemon's words, and a thread GitHub refused the daemon fails it after the
// outcomes and the count before it, its own message the one on stderr.
func TestThreadsResolveInTheReviewersPaneAsksTheDaemon(t *testing.T) {
	const withheldFails = "legion threads resolve: the implement App's pending review must be submitted or discarded before the threads holding its draft can close\n"
	for _, tc := range []struct {
		name   string
		status int
		answer string
		code   int
		stdout string
		stderr string
	}{
		{"the daemon resolves", http.StatusOK,
			`{"threads":[{"url":"https://github.test/thread/bot","resolved":"the Legion reviewer's acceptance of a bot's thread","newestBy":"legion-reviewer"},` +
				`{"url":"https://github.test/thread/own","leftOpen":"not an acceptance","newestBy":"legion-implementer"}],"withheld":0}`, 0,
			"resolved https://github.test/thread/bot — the Legion reviewer's acceptance of a bot's thread\nleft open https://github.test/thread/own — newest reply by legion-implementer is not an acceptance\n", ""},
		{"no thread is unresolved", http.StatusOK, `{"threads":[],"withheld":0}`, 0, "no unresolved threads\n", ""},
		{"the daemon resolves beside a thread it withheld", http.StatusOK,
			`{"threads":[{"url":"https://github.test/thread/bot","resolved":"the Legion reviewer's acceptance of a bot's thread","newestBy":"legion-reviewer"}],"withheld":1}`, 1,
			"resolved https://github.test/thread/bot — the Legion reviewer's acceptance of a bot's thread\n1 unresolved thread holds the implement App's pending draft and was left open\n", withheldFails},
		{"every unresolved thread is withheld", http.StatusOK, `{"threads":[],"withheld":2}`, 1,
			"2 unresolved threads hold the implement App's pending draft and were left open\n", withheldFails},
		{"GitHub refuses a thread after one resolved and one withheld", http.StatusOK,
			`{"threads":[{"url":"https://github.test/thread/bot","resolved":"the Legion reviewer's acceptance of a bot's thread","newestBy":"legion-reviewer"}],"withheld":1,` +
				`"refused":{"url":"https://github.test/thread/second","error":"GitHub: Resource not accessible by integration"}}`, 1,
			"resolved https://github.test/thread/bot — the Legion reviewer's acceptance of a bot's thread\n1 unresolved thread holds the implement App's pending draft and was left open\n",
			"legion threads resolve: resolveReviewThread failed for https://github.test/thread/second: GitHub: Resource not accessible by integration\n"},
		{"the daemon refuses", http.StatusForbidden, `{"code":"PULL_REQUEST_NOT_THE_ISSUES","error":"the grant is for LEGION-208, whose pull request is owner/repo#8, not owner/repo#7"}`, 1, "",
			"legion threads resolve: daemon returned 403: {\"code\":\"PULL_REQUEST_NOT_THE_ISSUES\",\"error\":\"the grant is for LEGION-208, whose pull request is owner/repo#8, not owner/repo#7\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				asked = append(asked, r.URL.Path+" "+string(body))
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.answer)
			}))
			t.Cleanup(daemon.Close)
			github, resolved := fakeThreadsGitHub(t, threadsPage())
			t.Setenv("LEGION_DAEMON_URL", daemon.URL)
			t.Setenv("LEGION_GITHUB_GRAPHQL_URL", github)
			t.Setenv("LEGION_GRANT", "one-command-grant")
			t.Setenv("LEGION_ROLE", "reviewer")
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "threads", "resolve", "--repo", "owner/repo", "--pr", "7"}, &out, &errb)
			if code != tc.code || out.String() != tc.stdout || errb.String() != tc.stderr {
				t.Fatalf("threads resolve in the reviewer's pane = %d, stdout %q, stderr %q; want %d, %q, %q", code, out.String(), errb.String(), tc.code, tc.stdout, tc.stderr)
			}
			want := `/legion/v1/threads/resolve {"grantId":"one-command-grant","repo":"owner/repo","number":7}`
			if len(asked) != 1 || asked[0] != want {
				t.Fatalf("the daemon was asked %q, want only %q", asked, want)
			}
			if got := resolved(); len(got) != 0 {
				t.Fatalf("the command resolved %v itself, want GitHub left to the daemon", got)
			}
		})
	}
}
