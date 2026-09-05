package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-github/v66/github"

	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// TestRequestIDQueryMatchesMarker locks the idempotency invariant: the token
// SearchByRequestID looks for in:body must actually appear in the body that
// BuildMetaMarker writes. If these drift, dedupe silently breaks and retries
// create duplicate threads — this is the regression guard for the original
// requestId (marker) vs request_id (search) mismatch.
func TestRequestIDQueryMatchesMarker(t *testing.T) {
	id := ComputeRequestID("sjawhar/legion", "641", "Subject", "Context", "Question", UrgencyMed, nil)
	marker, err := BuildMetaMarker(MetaMarker{Urgency: UrgencyMed, RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	query := githubapi.BuildRequestIDQuery("sjawhar", "legion", id, dispatchLabel)

	if !strings.Contains(query, "\""+id+"\"") {
		t.Fatalf("query %q does not search for quoted request id %q", query, id)
	}
	if !strings.Contains(marker, id) {
		t.Fatalf("marker %q does not contain request id %q the search looks for", marker, id)
	}
	if !strings.Contains(query, "label:"+dispatchLabel) {
		t.Errorf("query %q not scoped to dispatch label %q", query, dispatchLabel)
	}
}

// TestComputeRequestIDIncludesAsk ensures two dispatches that differ only in
// their structured ask produce different request ids, so they do not collapse
// onto a single thread.
func TestComputeRequestIDIncludesAsk(t *testing.T) {
	ask := []QuestionInfo{{Question: "Color?", Options: []QuestionOption{{Label: "red"}}}}
	base := ComputeRequestID("o/r", "641", "S", "C", "Q", UrgencyMed, nil)
	withAsk := ComputeRequestID("o/r", "641", "S", "C", "Q", UrgencyMed, ask)
	if base == withAsk {
		t.Fatalf("request id ignored ask: both %q", base)
	}
	again := ComputeRequestID("o/r", "641", "S", "C", "Q", UrgencyMed, ask)
	if withAsk != again {
		t.Fatalf("request id not deterministic for same ask: %q vs %q", withAsk, again)
	}
}

// TestComputeRequestIDStableForSameInputs guards the retry short-circuit: the
// same logical dispatch must hash to the same id across invocations.
func TestComputeRequestIDStableForSameInputs(t *testing.T) {
	a := ComputeRequestID("o/r", "641", "S", "C", "Q", UrgencyHigh, nil)
	b := ComputeRequestID("o/r", "641", "S", "C", "Q", UrgencyHigh, nil)
	if a != b {
		t.Fatalf("request id not stable: %q vs %q", a, b)
	}
}

// TestComputeRequestIDEmptyAskMatchesNil guards dedupe across callers that
// send `ask: []` versus omitting ask entirely: both mean "no structured
// questions" and must hash to the same thread.
func TestComputeRequestIDEmptyAskMatchesNil(t *testing.T) {
	a := ComputeRequestID("o/r", "", "S", "C", "Q", UrgencyMed, nil)
	b := ComputeRequestID("o/r", "", "S", "C", "Q", UrgencyMed, []QuestionInfo{})
	if a != b {
		t.Fatalf("nil ask %q and empty ask %q must hash identically", a, b)
	}
}

// TestComputeRequestIDChangesWithRepo ensures the same subject/context/
// question dispatched at two different repos never collapses onto one
// thread — repo is part of the hash, not just the search scope.
func TestComputeRequestIDChangesWithRepo(t *testing.T) {
	a := ComputeRequestID("owner/one", "", "S", "C", "Q", UrgencyMed, nil)
	b := ComputeRequestID("owner/two", "", "S", "C", "Q", UrgencyMed, nil)
	if a == b {
		t.Fatalf("request id ignored repo: both %q", a)
	}
}

type fakeIssue struct {
	state string
	body  string
	pull  bool
}

type fakeComment struct {
	id   int64
	body string
}

// fakeGitHub covers every REST + GraphQL endpoint Dispatch can call and
// records "<method> <path>[?query]" for assertions. Issues and comments are
// stateful so follow-up tests can seed a thread and read back what was posted.
type fakeGitHub struct {
	calls     []string
	issues    map[int]fakeIssue
	comments  map[int][]fakeComment
	nextIssue int
	nextID    int64
}

func newDispatchTestServer(t *testing.T) (*github.Client, *fakeGitHub) {
	t.Helper()
	gh := &fakeGitHub{issues: map[int]fakeIssue{}, comments: map[int][]fakeComment{}, nextIssue: 100, nextID: 1000}
	record := func(r *http.Request) {
		p := r.URL.Path
		if r.URL.RawQuery != "" {
			p += "?" + r.URL.RawQuery
		}
		gh.calls = append(gh.calls, r.Method+" "+p)
	}
	number := func(r *http.Request) int {
		n, _ := strconv.Atoi(r.PathValue("number"))
		return n
	}
	issueURL := func(r *http.Request, n int) string {
		return fmt.Sprintf("https://github.com/%s/%s/issues/%d", r.PathValue("owner"), r.PathValue("repo"), n)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var req struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gh.nextIssue++
		gh.issues[gh.nextIssue] = fakeIssue{state: "open", body: req.Body}
		fmt.Fprintf(w, `{"number":%d,"html_url":%q}`, gh.nextIssue, issueURL(r, gh.nextIssue))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		n := number(r)
		issue, ok := gh.issues[n]
		if !ok {
			issue = fakeIssue{state: "open"}
		}
		pull := ""
		if issue.pull {
			pull = `,"pull_request":{"url":"https://api.github.com/x"}`
		}
		fmt.Fprintf(w, `{"number":%d,"node_id":"node-%d","state":%q,"body":%q,"html_url":%q%s}`, n, n, issue.state, issue.body, issueURL(r, n), pull)
	})
	// GET .../issues/{number}/comments (list) and GET .../issues/comments/{id}
	// (one comment) overlap for ServeMux, so one handler serves both.
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{first}/{second}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.PathValue("first") == "comments" {
			fmt.Fprint(w, `{"id":1,"body":"parent comment"}`)
			return
		}
		n, _ := strconv.Atoi(r.PathValue("first"))
		items := make([]string, 0, len(gh.comments[n]))
		for _, c := range gh.comments[n] {
			items = append(items, fmt.Sprintf(`{"id":%d,"body":%q,"html_url":"%s#issuecomment-%d"}`, c.id, c.body, issueURL(r, n), c.id))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		n := number(r)
		var req struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gh.nextID++
		gh.comments[n] = append(gh.comments[n], fakeComment{id: gh.nextID, body: req.Body})
		fmt.Fprintf(w, `{"id":%d,"body":%q,"html_url":"%s#issuecomment-%d"}`, gh.nextID, req.Body, issueURL(r, n), gh.nextID)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"data":{"addSubIssue":{"issue":{"id":"x"},"subIssue":{"id":"y"}}}}`)
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"id":1,"body":"parent comment\n\n-> #101"}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := github.NewClient(srv.Client())
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	client.BaseURL = base
	return client, gh
}

func callsContain(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func countCalls(calls []string, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// threadBody is a valid dispatch thread body for seeding the fake.
func threadBody(t *testing.T) string {
	t.Helper()
	marker, err := BuildMetaMarker(MetaMarker{RequestID: "seed", Urgency: UrgencyMed})
	if err != nil {
		t.Fatal(err)
	}
	return BuildThreadBody(marker, "S", "C", "Q")
}

func TestCreateThreadParentless(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	result, err := CreateThread(context.Background(), client, DispatchInput{
		Repo:     "acme/widgets",
		Subject:  "Pick a color",
		Context:  "Building the palette.",
		Question: "Blue or red?",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if result.Thread == 0 || result.URL == "" {
		t.Fatalf("expected populated result, got %+v", result)
	}
	if !callsContain(gh.calls, "/repos/acme/widgets/issues") {
		t.Errorf("expected an issue-create call against acme/widgets, got %v", gh.calls)
	}
	if callsContain(gh.calls, "graphql") {
		t.Errorf("parent-less dispatch must not call the sub-issue mutation, got %v", gh.calls)
	}
	if callsContain(gh.calls, "/comments/") {
		t.Errorf("parent-less dispatch must not touch a breadcrumb comment, got %v", gh.calls)
	}
}

func TestCreateThreadQualifiedParentBeatsRepo(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	_, err := CreateThread(context.Background(), client, DispatchInput{
		Repo:     "ignored/repo",
		Parent:   "qualified/repo#42",
		Subject:  "S",
		Context:  "C",
		Question: "Q",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if callsContain(gh.calls, "ignored/repo") {
		t.Errorf("qualified parent must override the repo arg, but a call touched ignored/repo: %v", gh.calls)
	}
	if !callsContain(gh.calls, "/repos/qualified/repo/issues") {
		t.Errorf("expected calls against qualified/repo, got %v", gh.calls)
	}
	if !callsContain(gh.calls, "graphql") {
		t.Errorf("expected the sub-issue mutation for a given parent, got %v", gh.calls)
	}
}

func TestCreateThreadBareParentUsesRepoArg(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	_, err := CreateThread(context.Background(), client, DispatchInput{
		Repo:     "acme/widgets",
		Parent:   "42",
		Subject:  "S",
		Context:  "C",
		Question: "Q",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if !callsContain(gh.calls, "/repos/acme/widgets/issues/42") {
		t.Errorf("expected the bare parent to resolve against the repo arg, got %v", gh.calls)
	}
	if !callsContain(gh.calls, "graphql") {
		t.Errorf("expected the sub-issue mutation for a given parent, got %v", gh.calls)
	}
}

func TestCreateThreadErrorWhenNoRepo(t *testing.T) {
	_, err := CreateThread(context.Background(), nil, DispatchInput{
		Subject:  "S",
		Context:  "C",
		Question: "Q",
	})
	if err == nil {
		t.Fatalf("expected an error when neither repo nor a qualified parent is given")
	}
	want := "no repo: pass repo=owner/name (the plugin fills it from the working directory when one is a GitHub repo)"
	if err.Error() != want {
		t.Errorf("got %q\nwant %q", err.Error(), want)
	}
}

// TestCreateThreadRejectsUnusableInput: a blank required field or an urgency
// the marker parsers refuse must fail before any GitHub call — otherwise the
// thread exists on GitHub but the dashboard's marker filter drops it.
func TestCreateThreadRejectsUnusableInput(t *testing.T) {
	base := DispatchInput{Repo: "acme/widgets", Subject: "S", Context: "C", Question: "Q"}
	cases := []struct {
		name   string
		mutate func(*DispatchInput)
		want   string
	}{
		{"blank subject", func(in *DispatchInput) { in.Subject = " \n" }, "subject is required"},
		{"blank context", func(in *DispatchInput) { in.Context = "" }, "context is required"},
		{"blank question", func(in *DispatchInput) { in.Question = "\t" }, "question is required"},
		{"bad urgency", func(in *DispatchInput) { in.Urgency = "urgent" }, `invalid urgency "urgent"`},
		{"repo with search qualifier", func(in *DispatchInput) { in.Repo = "acme/widgets is:closed" }, "invalid repo slug"},
		{"repo from qualified parent with quote", func(in *DispatchInput) { in.Parent = `acme/wid"gets#4` }, "invalid repo slug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, gh := newDispatchTestServer(t)
			input := base
			tc.mutate(&input)
			_, err := CreateThread(context.Background(), client, input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got err %v, want containing %q", err, tc.want)
			}
			if len(gh.calls) != 0 {
				t.Errorf("expected no GitHub calls, got %v", gh.calls)
			}
		})
	}
}

// TestCreateThreadQualifiedParentWithComment: the spec's
// owner/name#<n>#<commentId> form names the repo, links under issue n, and
// appends the breadcrumb to the comment.
func TestCreateThreadQualifiedParentWithComment(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	_, err := CreateThread(context.Background(), client, DispatchInput{
		Parent:   "qualified/repo#42#3216548790",
		Subject:  "S",
		Context:  "C",
		Question: "Q",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if !callsContain(gh.calls, "/repos/qualified/repo/issues") {
		t.Errorf("expected calls against qualified/repo, got %v", gh.calls)
	}
	if !callsContain(gh.calls, "/repos/qualified/repo/issues/comments/3216548790") {
		t.Errorf("expected the breadcrumb comment edit, got %v", gh.calls)
	}
}

func TestCreateThreadWritesAskIDs(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	_, err := CreateThread(context.Background(), client, DispatchInput{
		Repo: "acme/widgets", Subject: "S", Context: "C", Question: "Q",
		Ask: []QuestionInfo{{Question: "a?"}, {Question: "b?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := gh.issues[101]
	parsed := ParseMetaMarker(created.body)
	if parsed == nil {
		t.Fatalf("created body has no thread marker: %q", created.body)
	}
	if parsed.Ask[0].AskID != parsed.RequestID || parsed.Ask[1].AskID != parsed.RequestID+".1" {
		t.Errorf("askIds: %+v (requestId %s)", parsed.Ask, parsed.RequestID)
	}
}

func TestDispatchRejectsProseOverCapBeforeAnyGitHubCall(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	cases := []struct {
		name  string
		input DispatchInput
		want  string
	}{
		{"open context", DispatchInput{Repo: "acme/widgets", Subject: "S", Context: long(1201), Question: "Q"}, "context is 1201 characters; the limit is 1200"},
		{"open question", DispatchInput{Repo: "acme/widgets", Subject: "S", Context: "C", Question: long(801)}, "question is 801 characters; the limit is 800"},
		{"continue context", DispatchInput{Repo: "acme/widgets", Thread: "42", Context: long(1201), Question: "Q"}, "context is 1201 characters; the limit is 1200"},
		{"continue question", DispatchInput{Repo: "acme/widgets", Thread: "42", Context: "C", Question: long(801)}, "question is 801 characters; the limit is 800"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, gh := newDispatchTestServer(t)
			_, err := Dispatch(context.Background(), client, tc.input)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if len(gh.calls) != 0 {
				t.Errorf("expected no GitHub calls, got %v", gh.calls)
			}
		})
	}
	client, _ := newDispatchTestServer(t)
	if _, err := Dispatch(context.Background(), client, DispatchInput{Repo: "acme/widgets", Subject: "S", Context: long(1200), Question: long(800)}); err != nil {
		t.Errorf("exactly at the caps must pass: %v", err)
	}
}

func TestContinueThreadPostsAskCommentAndCreatesNoIssue(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	gh.issues[42] = fakeIssue{state: "open", body: threadBody(t)}
	result, err := Dispatch(context.Background(), client, DispatchInput{
		Repo: "acme/widgets", Thread: "42", Context: "More context.", Question: "Revised?",
		Origin: &Origin{Host: "omp", SessionID: "ses_2", SessionTitle: "renamed"},
		Ask:    []QuestionInfo{{Question: "Which?", Options: []QuestionOption{{Label: "a"}}}},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Thread != 42 || result.URL != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("result must point at the existing issue: %+v", result)
	}
	if result.Comment != "https://github.com/acme/widgets/issues/42#issuecomment-1001" {
		t.Errorf("result.Comment: %q", result.Comment)
	}
	if countCalls(gh.calls, "POST /repos/acme/widgets/issues") != countCalls(gh.calls, "POST /repos/acme/widgets/issues/42/comments") {
		t.Errorf("a follow-up must not create an issue: %v", gh.calls)
	}
	if callsContain(gh.calls, "/search/issues") {
		t.Errorf("a follow-up dedupes over comments, not the issue search: %v", gh.calls)
	}
	posted := gh.comments[42]
	if len(posted) != 1 {
		t.Fatalf("expected one comment, got %d", len(posted))
	}
	marker := ParseAskMarker(posted[0].body)
	if marker == nil {
		t.Fatalf("comment has no ask marker: %q", posted[0].body)
	}
	if marker.Origin == nil || marker.Origin.SessionID != "ses_2" || marker.Origin.SessionTitle != "renamed" {
		t.Errorf("origin re-stamped from the call: %+v", marker.Origin)
	}
	if len(marker.Ask) != 1 || marker.Ask[0].AskID != marker.RequestID {
		t.Errorf("first ask reuses the follow-up request id: %+v", marker.Ask)
	}
	if !strings.Contains(posted[0].body, "## Context\n\nMore context.\n\n## Question\n\nRevised?") {
		t.Errorf("body layout: %q", posted[0].body)
	}
}

func TestContinueThreadDedupesOverComments(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	gh.issues[42] = fakeIssue{state: "open", body: threadBody(t)}
	input := DispatchInput{Repo: "acme/widgets", Thread: "42", Context: "C2", Question: "Q2"}
	first, err := Dispatch(context.Background(), client, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Dispatch(context.Background(), client, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.comments[42]) != 1 {
		t.Fatalf("retry posted a duplicate: %d comments", len(gh.comments[42]))
	}
	if first.Comment != second.Comment {
		t.Errorf("retry must return the existing comment: %q vs %q", first.Comment, second.Comment)
	}
	changed, err := Dispatch(context.Background(), client, DispatchInput{Repo: "acme/widgets", Thread: "42", Context: "C2", Question: "Q3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gh.comments[42]) != 2 || changed.Comment == first.Comment {
		t.Errorf("a different question is a new follow-up: %d comments, %q", len(gh.comments[42]), changed.Comment)
	}
}

func TestContinueThreadRefusesNonThreadsAndClosedThreads(t *testing.T) {
	cases := []struct {
		name  string
		issue fakeIssue
		want  string
	}{
		{"plain issue", fakeIssue{state: "open", body: "just an issue"}, "#42 is not a dispatch thread"},
		{"pull request", fakeIssue{state: "open", body: "", pull: true}, "#42 is not a dispatch thread"},
		{"closed thread", fakeIssue{state: "closed", body: ""}, "#42 is closed; open a new thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, gh := newDispatchTestServer(t)
			issue := tc.issue
			if issue.body == "" && !issue.pull {
				issue.body = threadBody(t)
			}
			gh.issues[42] = issue
			_, err := Dispatch(context.Background(), client, DispatchInput{Repo: "acme/widgets", Thread: "42", Context: "C", Question: "Q"})
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if len(gh.comments[42]) != 0 {
				t.Errorf("nothing may be posted: %v", gh.comments[42])
			}
		})
	}
}

func TestContinueThreadRejectsMixedMode(t *testing.T) {
	for _, extra := range []func(*DispatchInput){
		func(in *DispatchInput) { in.Subject = "S" },
		func(in *DispatchInput) { in.Urgency = UrgencyHigh },
		func(in *DispatchInput) { in.Parent = "7" },
	} {
		client, gh := newDispatchTestServer(t)
		input := DispatchInput{Repo: "acme/widgets", Thread: "42", Context: "C", Question: "Q"}
		extra(&input)
		_, err := Dispatch(context.Background(), client, input)
		if err == nil || err.Error() != "thread cannot be combined with subject, urgency, or parent" {
			t.Errorf("err %v", err)
		}
		if len(gh.calls) != 0 {
			t.Errorf("validation must precede GitHub calls: %v", gh.calls)
		}
	}
}

func TestContinueThreadResolvesRepo(t *testing.T) {
	client, gh := newDispatchTestServer(t)
	gh.issues[9] = fakeIssue{state: "open", body: threadBody(t)}
	if _, err := Dispatch(context.Background(), client, DispatchInput{Repo: "ignored/repo", Thread: "qualified/repo#9", Context: "C", Question: "Q"}); err != nil {
		t.Fatal(err)
	}
	if callsContain(gh.calls, "ignored/repo") || !callsContain(gh.calls, "/repos/qualified/repo/issues/9") {
		t.Errorf("qualified thread must name its repo: %v", gh.calls)
	}
	_, err := Dispatch(context.Background(), client, DispatchInput{Thread: "9", Context: "C", Question: "Q"})
	want := "no repo for thread #9: pass thread=owner/name#9 (the plugin fills repo from the working directory when one is a GitHub repo)"
	if err == nil || err.Error() != want {
		t.Errorf("bare thread without repo: got %v want %q", err, want)
	}
	_, err = Dispatch(context.Background(), client, DispatchInput{Thread: "nine", Context: "C", Question: "Q"})
	if err == nil || err.Error() != "Invalid thread: nine" {
		t.Errorf("malformed thread: %v", err)
	}
}

func TestComputeFollowUpRequestIDCoversThreadContextQuestionAsk(t *testing.T) {
	ask := []QuestionInfo{{Question: "Color?"}}
	base := ComputeFollowUpRequestID("o/r", 42, "C", "Q", nil)
	if base != ComputeFollowUpRequestID("o/r", 42, "C", "Q", []QuestionInfo{}) {
		t.Error("nil and empty ask must hash identically")
	}
	for name, other := range map[string]string{
		"thread":  ComputeFollowUpRequestID("o/r", 43, "C", "Q", nil),
		"repo":    ComputeFollowUpRequestID("o/x", 42, "C", "Q", nil),
		"context": ComputeFollowUpRequestID("o/r", 42, "C2", "Q", nil),
		"ask":     ComputeFollowUpRequestID("o/r", 42, "C", "Q", ask),
	} {
		if other == base {
			t.Errorf("request id ignored %s", name)
		}
	}
	if len(base) != 16 {
		t.Errorf("request id must be 16 hex chars, got %q", base)
	}
}
