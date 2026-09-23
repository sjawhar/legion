package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved opener: comments(first: 1) { nodes { author { login } url body } } newest: comments(last: 1) { nodes { author { login } url body } } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const resolveReviewThreadMutation = `mutation($threadId: ID!) { resolveReviewThread(input: { threadId: $threadId }) { thread { id isResolved } } }`

func runThreads(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "resolve" {
		fmt.Fprintln(stderr, "usage: legion threads resolve --pr <number> --repo <owner>/<repo>")
		return 2
	}
	flags := newFlags("threads resolve", stderr)
	pr := flags.String("pr", "", "pull request number (required)")
	repo := flags.String("repo", "", "repository owner/name (required)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return 2
	}
	owner, name, ok := strings.Cut(*repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(owner+name, " \t\r\n/") {
		fmt.Fprintln(stderr, "legion threads resolve: --repo must be <owner>/<repo>")
		return 2
	}
	number, err := strconv.Atoi(*pr)
	if err != nil || number <= 0 {
		fmt.Fprintln(stderr, "legion threads resolve: --pr must be a positive pull request number")
		return 2
	}
	response, err := redeemGrant(ctx, "/legion/v1/gh-token")
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: Unable to redeem LEGION_GRANT: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	var credential githubTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&credential); err != nil || credential.Token == "" {
		fmt.Fprintln(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response")
		return 1
	}
	threads, err := unresolvedReviewThreads(ctx, credential.Token, owner, name, number)
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 1
	}
	if len(threads) == 0 {
		fmt.Fprintln(stdout, "no unresolved threads")
		return 0
	}
	for _, thread := range threads {
		if thread.openerLogin == "" || thread.openerLogin != thread.newestLogin || !strings.HasPrefix(strings.TrimLeft(thread.newestBody, " \t\r\n"), "Accepted:") {
			by := thread.newestLogin
			if by == "" {
				by = "an unknown account"
			}
			fmt.Fprintf(stdout, "left open %s — newest reply by %s is not an acceptance\n", thread.url, by)
			continue
		}
		if err := graphql(ctx, credential.Token, resolveReviewThreadMutation, map[string]any{"threadId": thread.id}, nil); err != nil {
			return threadFailure(stderr, thread.url, err)
		}
		fmt.Fprintf(stdout, "resolved %s\n", thread.url)
	}
	return 0
}

func threadFailure(stderr io.Writer, url string, err error) int {
	fmt.Fprintf(stderr, "legion threads resolve: resolveReviewThread failed for %s: %v\n", url, err)
	return 1
}

type reviewThread struct {
	id, url, openerLogin, newestLogin, newestBody string
}

func unresolvedReviewThreads(ctx context.Context, token, owner, name string, number int) ([]reviewThread, error) {
	var all []reviewThread
	var after any
	for {
		var page reviewThreadsPage
		if err := graphql(ctx, token, reviewThreadsQuery, map[string]any{"owner": owner, "name": name, "number": number, "after": after}, &page); err != nil {
			return nil, err
		}
		if page.Data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("%s/%s#%d was not found by GitHub", owner, name, number)
		}
		for _, node := range page.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if node.IsResolved || len(node.Opening.Nodes) == 0 || len(node.Newest.Nodes) == 0 {
				continue
			}
			opening, newest := node.Opening.Nodes[0], node.Newest.Nodes[0]
			all = append(all, reviewThread{id: node.ID, url: opening.URL, openerLogin: opening.Author.Login, newestLogin: newest.Author.Login, newestBody: newest.Body})
		}
		info := page.Data.Repository.PullRequest.ReviewThreads.PageInfo
		if !info.HasNextPage {
			return all, nil
		}
		after = info.EndCursor
	}
}

type reviewThreadsPage struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Opening    struct {
							Nodes []reviewComment `json:"nodes"`
						} `json:"opener"`
						Newest struct {
							Nodes []reviewComment `json:"nodes"`
						} `json:"newest"`
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

type reviewComment struct {
	URL    string `json:"url"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

func graphql(ctx context.Context, token, query string, variables map[string]any, into any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	endpoint := os.Getenv("LEGION_GITHUB_GRAPHQL_URL")
	if endpoint == "" {
		endpoint = "https://api.github.com/graphql"
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
