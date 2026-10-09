package reviewthreads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// fakeGitHub is GitHub's GraphQL holding the pull request's review threads, each with whether it is
// resolved, served in pages of pageSize. It records every resolveReviewThread's thread id in order
// and refuses the ones refuse names with GitHub's own message.
type fakeGitHub struct {
	threads  []fakeThread
	pageSize int
	refuse   map[string]bool
	queries  int
	resolved []string
}

type fakeThread struct {
	id       string
	resolved bool
}

func (g *fakeGitHub) call(_ context.Context, query string, variables map[string]any, into any) error {
	if strings.Contains(query, "resolveReviewThread") {
		id := variables["threadId"].(string)
		if g.refuse[id] {
			return errors.New("GitHub: Resource not accessible by integration")
		}
		g.resolved = append(g.resolved, id)
		return nil
	}
	g.queries++
	start := 0
	if after, _ := variables["after"].(string); after != "" {
		start, _ = strconv.Atoi(strings.TrimPrefix(after, "cursor-"))
	}
	end := len(g.threads)
	if g.pageSize > 0 && start+g.pageSize < end {
		end = start + g.pageSize
	}
	nodes := []map[string]any{}
	for _, thread := range g.threads[start:end] {
		nodes = append(nodes, map[string]any{"id": thread.id, "isResolved": thread.resolved})
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < len(g.threads), "endCursor": fmt.Sprintf("cursor-%d", end)}}}}}})
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, into)
}

var widgets = ghrepo.MustParse("acme/widgets")

// The run resolves exactly the ids it is given, in the order given, after one listing of the pull
// request's threads: a thread GitHub already holds resolved is answered as resolved with
// AlreadyResolved and not written again, and so is an id named twice, so a retry after a partial
// run resolves what remains without a failed write. A thread the caller did not name is left as it
// is, whatever its state.
func TestResolveResolvesTheNamedThreadsInOrderAndAnswersAResolvedOneIdempotently(t *testing.T) {
	github := &fakeGitHub{threads: []fakeThread{{"t1", false}, {"t2", true}, {"t3", false}, {"t4", false}}, pageSize: 2}
	outcomes, err := Resolve(context.Background(), github.call, widgets, 42, []string{"t3", "t2", "t1", "t3"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []Outcome{
		{Thread: "t3", Resolved: true},
		{Thread: "t2", Resolved: true, Reason: AlreadyResolved},
		{Thread: "t1", Resolved: true},
		{Thread: "t3", Resolved: true, Reason: AlreadyResolved},
	}
	if !slices.Equal(outcomes, want) {
		t.Fatalf("outcomes %+v, want %+v", outcomes, want)
	}
	if !slices.Equal(github.resolved, []string{"t3", "t1"}) {
		t.Fatalf("GitHub resolved %v, want t3 then t1 and nothing else", github.resolved)
	}
	if github.queries != 2 {
		t.Fatalf("GitHub was asked for threads %d times, want the two pages once each", github.queries)
	}
}

// An id that is no thread of the pull request fails the run before any write, naming every such id
// in the order given: the threads among the ids that are the pull request's stay as they were.
func TestResolveRefusesAnIdThatIsNoThreadOfThePullRequestBeforeAnyWrite(t *testing.T) {
	github := &fakeGitHub{threads: []fakeThread{{"t1", false}, {"t2", true}}}
	outcomes, err := Resolve(context.Background(), github.call, widgets, 42, []string{"t1", "other-pr", "t2", "nowhere"})
	var foreign *NotOnPullRequest
	if !errors.As(err, &foreign) || !slices.Equal(foreign.Threads, []string{"other-pr", "nowhere"}) {
		t.Fatalf("Resolve = %+v, %v; want a NotOnPullRequest naming other-pr and nowhere", outcomes, err)
	}
	if outcomes != nil || len(github.resolved) != 0 {
		t.Fatalf("outcomes %+v, GitHub resolved %v; want nothing answered and nothing written", outcomes, github.resolved)
	}
	if !strings.Contains(err.Error(), "other-pr, nowhere") {
		t.Fatalf("error %q, want it to name the foreign ids", err)
	}
}

// GitHub refusing a thread stops the run: the outcomes before it are returned beside a Refused
// naming the thread and carrying GitHub's message, and the ids after it are not written.
func TestResolveStopsAtTheThreadGitHubRefuses(t *testing.T) {
	github := &fakeGitHub{threads: []fakeThread{{"t1", false}, {"t2", false}, {"t3", false}}, refuse: map[string]bool{"t2": true}}
	outcomes, err := Resolve(context.Background(), github.call, widgets, 42, []string{"t1", "t2", "t3"})
	var refused *Refused
	if !errors.As(err, &refused) || refused.Thread != "t2" || refused.Err.Error() != "GitHub: Resource not accessible by integration" {
		t.Fatalf("Resolve error = %v, want a Refused for t2 with GitHub's message", err)
	}
	if !slices.Equal(outcomes, []Outcome{{Thread: "t1", Resolved: true}}) || !slices.Equal(github.resolved, []string{"t1"}) {
		t.Fatalf("outcomes %+v, GitHub resolved %v; want t1 alone before the refusal", outcomes, github.resolved)
	}
}

// A pull request GitHub does not know fails the run before any write, naming it.
func TestResolveFailsOnAPullRequestGitHubDoesNotKnow(t *testing.T) {
	call := func(_ context.Context, _ string, _ map[string]any, into any) error {
		return json.Unmarshal([]byte(`{"data":{"repository":{"pullRequest":null}}}`), into)
	}
	if _, err := Resolve(context.Background(), call, widgets, 7, []string{"t1"}); err == nil || !strings.Contains(err.Error(), "acme/widgets#7 was not found by GitHub") {
		t.Fatalf("Resolve = %v, want the missing pull request named", err)
	}
}

// Result reads a GraphQL body as GitHub's first error, or as the shape asked for.
func TestResultAnswersGitHubsFirstErrorOrDecodes(t *testing.T) {
	if err := Result([]byte(`{"errors":[{"message":"Could not resolve to a node"},{"message":"second"}]}`), nil); err == nil || err.Error() != "GitHub: Could not resolve to a node" {
		t.Fatalf("Result with errors = %v, want GitHub's first message", err)
	}
	var page threadsPage
	if err := Result([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"t1","isResolved":true}],"pageInfo":{"hasNextPage":false}}}}}}`), &page); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if nodes := page.Data.Repository.PullRequest.ReviewThreads.Nodes; len(nodes) != 1 || nodes[0].ID != "t1" || !nodes[0].IsResolved {
		t.Fatalf("decoded %+v, want the one resolved thread", nodes)
	}
}
