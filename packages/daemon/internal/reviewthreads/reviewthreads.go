// Package reviewthreads resolves the review threads a caller names, by GraphQL node id, on one pull
// request: the GitHub GraphQL read that checks each id is a thread of that pull request, and the
// resolveReviewThread writes that follow. It reads no comment and applies no rule about who wrote
// what: the caller chose the threads. The daemon's POST /legion/v1/threads/resolve runs it for the
// reviewer as the implement App, since GitHub refuses the review App a resolve on the implementer's
// pull request.
package reviewthreads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const threadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const resolveMutation = `mutation($threadId: ID!) { resolveReviewThread(input: { threadId: $threadId }) { thread { id isResolved } } }`

// AlreadyResolved is the reason an Outcome gives for a thread GitHub already held resolved when the
// run began, which the run leaves as it is: a retry after a partial run answers the same ids
// without a write for the ones that went through.
const AlreadyResolved = "already resolved"

// Outcome is one named thread after the run: Thread is its node id, Resolved says GitHub holds it
// resolved, and Reason is AlreadyResolved when it was before the run wrote anything, empty when
// this run's resolveReviewThread resolved it.
type Outcome struct {
	Thread   string `json:"thread"`
	Resolved bool   `json:"resolved"`
	Reason   string `json:"reason,omitempty"`
}

// NotOnPullRequest is the ids the caller named that are no review thread of the pull request, in
// the order given, found before any write: a wrong id resolves nothing, on any pull request.
type NotOnPullRequest struct {
	Threads []string
}

func (e *NotOnPullRequest) Error() string {
	return fmt.Sprintf("not review threads of the pull request: %s", strings.Join(e.Threads, ", "))
}

// Refused is GitHub refusing to resolve a named thread, with its own message.
type Refused struct {
	Thread string
	Err    error
}

func (e *Refused) Error() string {
	return fmt.Sprintf("resolveReviewThread failed for %s: %v", e.Thread, e.Err)
}

func (e *Refused) Unwrap() error { return e.Err }

// Resolve lists the pull request's review threads once, every page before any write, and
// resolves the threads named by id in the order given, one resolveReviewThread each. An id that is
// no thread of the pull request fails the whole run with a *NotOnPullRequest naming every such id,
// before anything is written. A thread already resolved, by GitHub before the run or by an earlier
// id of the same run, is answered AlreadyResolved and not written again. A refusal stops the run:
// it returns the outcomes before the refused thread and a *Refused naming it.
func Resolve(ctx context.Context, call GraphQL, repository ghrepo.Repository, number int, threads []string) ([]Outcome, error) {
	resolved, err := reviewThreads(ctx, call, repository, number)
	if err != nil {
		return nil, err
	}
	var foreign []string
	for _, id := range threads {
		if _, known := resolved[id]; !known {
			foreign = append(foreign, id)
		}
	}
	if len(foreign) > 0 {
		return nil, &NotOnPullRequest{Threads: foreign}
	}
	outcomes := make([]Outcome, 0, len(threads))
	for _, id := range threads {
		if resolved[id] {
			outcomes = append(outcomes, Outcome{Thread: id, Resolved: true, Reason: AlreadyResolved})
			continue
		}
		if err := call(ctx, resolveMutation, map[string]any{"threadId": id}, nil); err != nil {
			return outcomes, &Refused{Thread: id, Err: err}
		}
		resolved[id] = true
		outcomes = append(outcomes, Outcome{Thread: id, Resolved: true})
	}
	return outcomes, nil
}

// reviewThreads is every review thread of the pull request, resolved or not, keyed by node id with
// whether GitHub holds it resolved.
func reviewThreads(ctx context.Context, call GraphQL, repository ghrepo.Repository, number int) (map[string]bool, error) {
	all := map[string]bool{}
	var after any
	for {
		var page threadsPage
		if err := call(ctx, threadsQuery, map[string]any{"owner": repository.Owner(), "name": repository.Name(), "number": number, "after": after}, &page); err != nil {
			return nil, err
		}
		if page.Data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("%s#%d was not found by GitHub", repository, number)
		}
		for _, node := range page.Data.Repository.PullRequest.ReviewThreads.Nodes {
			all[node.ID] = node.IsResolved
		}
		info := page.Data.Repository.PullRequest.ReviewThreads.PageInfo
		if !info.HasNextPage {
			return all, nil
		}
		after = info.EndCursor
	}
}

type threadsPage struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// GraphQL is one GraphQL call against GitHub as the identity its transport carries: the App whose
// token it holds (TokenGraphQL). It fails with GitHub's own message, and decodes the response into
// into when into is not nil.
type GraphQL func(ctx context.Context, query string, variables map[string]any, into any) error

// TokenGraphQL calls GitHub's GraphQL endpoint, https://api.github.com/graphql when endpoint is
// empty, with token as the bearer.
func TokenGraphQL(endpoint, token string) GraphQL {
	if endpoint == "" {
		endpoint = "https://api.github.com/graphql"
	}
	return func(ctx context.Context, query string, variables map[string]any, into any) error {
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("GitHub returned %d: %s", response.StatusCode, strings.TrimSpace(string(encoded)))
		}
		return Result(encoded, into)
	}
}

// Result is a GraphQL response body: its first error as GitHub's message, or the response decoded
// into into when into is not nil.
func Result(encoded []byte, into any) error {
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("GitHub: %s", envelope.Errors[0].Message)
	}
	if into != nil {
		return json.Unmarshal(encoded, into)
	}
	return nil
}
