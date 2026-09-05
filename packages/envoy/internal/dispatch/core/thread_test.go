package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// newDispatchTestServer spins up a fake GitHub REST+GraphQL server covering
// every endpoint CreateThread can call, and returns a *github.Client pointed
// at it plus a running log of "<method> <path>[?query]" strings for
// assertions.
func newDispatchTestServer(t *testing.T) (*github.Client, *[]string) {
	t.Helper()
	var calls []string
	nextIssue := 100
	record := func(r *http.Request) {
		p := r.URL.Path
		if r.URL.RawQuery != "" {
			p += "?" + r.URL.RawQuery
		}
		calls = append(calls, r.Method+" "+p)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		nextIssue++
		fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/%s/%s/issues/%d"}`,
			nextIssue, r.PathValue("owner"), r.PathValue("repo"), nextIssue)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprintf(w, `{"number":%s,"node_id":"node-%s"}`, r.PathValue("number"), r.PathValue("number"))
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"data":{"addSubIssue":{"issue":{"id":"x"},"subIssue":{"id":"y"}}}}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fmt.Fprint(w, `{"id":1,"body":"parent comment"}`)
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
	return client, &calls
}

func callsContain(calls []string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func TestCreateThreadParentless(t *testing.T) {
	client, calls := newDispatchTestServer(t)
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
	if !callsContain(*calls, "/repos/acme/widgets/issues") {
		t.Errorf("expected an issue-create call against acme/widgets, got %v", *calls)
	}
	if callsContain(*calls, "graphql") {
		t.Errorf("parent-less dispatch must not call the sub-issue mutation, got %v", *calls)
	}
	if callsContain(*calls, "/comments/") {
		t.Errorf("parent-less dispatch must not touch a breadcrumb comment, got %v", *calls)
	}
}

func TestCreateThreadQualifiedParentBeatsRepo(t *testing.T) {
	client, calls := newDispatchTestServer(t)
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
	if callsContain(*calls, "ignored/repo") {
		t.Errorf("qualified parent must override the repo arg, but a call touched ignored/repo: %v", *calls)
	}
	if !callsContain(*calls, "/repos/qualified/repo/issues") {
		t.Errorf("expected calls against qualified/repo, got %v", *calls)
	}
	if !callsContain(*calls, "graphql") {
		t.Errorf("expected the sub-issue mutation for a given parent, got %v", *calls)
	}
}

func TestCreateThreadBareParentUsesRepoArg(t *testing.T) {
	client, calls := newDispatchTestServer(t)
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
	if !callsContain(*calls, "/repos/acme/widgets/issues/42") {
		t.Errorf("expected the bare parent to resolve against the repo arg, got %v", *calls)
	}
	if !callsContain(*calls, "graphql") {
		t.Errorf("expected the sub-issue mutation for a given parent, got %v", *calls)
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
	want := "no repo: pass repo=owner/name (the shim fills it from the working directory when one is a GitHub repo)"
	if err.Error() != want {
		t.Errorf("got %q\nwant %q", err.Error(), want)
	}
}

// TestCreateThreadRejectsUnusableInput: a blank required field or an urgency
// the marker parsers refuse must fail before any GitHub call — otherwise the
// thread exists on GitHub but the dashboard's frontmatter filter drops it.
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
			client, calls := newDispatchTestServer(t)
			input := base
			tc.mutate(&input)
			_, err := CreateThread(context.Background(), client, input)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got err %v, want containing %q", err, tc.want)
			}
			if len(*calls) != 0 {
				t.Errorf("expected no GitHub calls, got %v", *calls)
			}
		})
	}
}

// TestCreateThreadQualifiedParentWithComment: the spec's
// owner/name#<n>#<commentId> form names the repo, links under issue n, and
// appends the breadcrumb to the comment.
func TestCreateThreadQualifiedParentWithComment(t *testing.T) {
	client, calls := newDispatchTestServer(t)
	_, err := CreateThread(context.Background(), client, DispatchInput{
		Parent:   "qualified/repo#42#3216548790",
		Subject:  "S",
		Context:  "C",
		Question: "Q",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if !callsContain(*calls, "/repos/qualified/repo/issues") {
		t.Errorf("expected calls against qualified/repo, got %v", *calls)
	}
	if !callsContain(*calls, "/repos/qualified/repo/issues/comments/3216548790") {
		t.Errorf("expected the breadcrumb comment edit, got %v", *calls)
	}
}
