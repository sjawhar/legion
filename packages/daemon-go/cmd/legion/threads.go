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

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved opener: comments(first: 1) { nodes { author { __typename login } url } } newest: comments(last: 1) { nodes { author { __typename login } url body state } } }
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
	repository, err := ghrepo.Parse("--repo", *repo)
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
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
	var apps *legionApps
	if err := json.NewDecoder(response.Body).Decode(&credential); err == nil && credential.Token != "" {
		apps, err = legionAppsFrom(credential.LegionAppLogins)
		if err != nil {
			fmt.Fprintf(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintln(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response")
		return 1
	}
	threads, err := unresolvedReviewThreads(ctx, credential.Token, repository, number)
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 1
	}
	if len(threads) == 0 {
		fmt.Fprintln(stdout, "no unresolved threads")
		return 0
	}
	for _, thread := range threads {
		// The rule is the TypeScript CLI's (review-threads.ts resolution); threads_test.go and
		// review-threads.test.ts share vectors.
		how, reason := resolution(thread, apps)
		if how == "" {
			by := thread.newestLogin
			if by == "" {
				by = "an unknown account"
			}
			fmt.Fprintf(stdout, "left open %s — newest reply by %s is %s\n", thread.url, by, reason)
			continue
		}
		if err := graphql(ctx, credential.Token, resolveReviewThreadMutation, map[string]any{"threadId": thread.id}, nil); err != nil {
			return threadFailure(stderr, thread.url, err)
		}
		fmt.Fprintf(stdout, "resolved %s — %s\n", thread.url, how)
	}
	return 0
}

func threadFailure(stderr io.Writer, url string, err error) int {
	fmt.Fprintf(stderr, "legion threads resolve: resolveReviewThread failed for %s: %v\n", url, err)
	return 1
}

// reviewThread is an unresolved review thread reduced to the facts the rule reads.
type reviewThread struct {
	id, url, openerLogin, openerTypename, newestLogin, newestTypename, newestBody string
	// newestPending marks a newest comment that is a draft in a pending, unsubmitted review.
	newestPending bool
}

// legionApps is Legion's role Apps, from the daemon's gh-token answer: every App's login as botSlug
// reads it, and the review App's. A nil *legionApps is a session that cannot know them (the daemon
// named none), and then no thread counts as a bot's.
type legionApps struct {
	logins map[string]bool
	review string
}

// legionAppsFrom is the daemon's role-keyed logins as legionApps, nil when it named none. The
// daemon names every role App or none (the contract's legionAppLogins: an App's git identity,
// "<slug>[bot]", for each of implement and review), so an answer naming some, or one whose login
// is not an App's, is an invalid response rather than a session that knows some of Legion's Apps.
func legionAppsFrom(logins map[string]string) (*legionApps, error) {
	if logins == nil {
		return nil, nil
	}
	apps := &legionApps{logins: map[string]bool{}}
	for _, role := range []string{"implement", "review"} {
		slug := botSlug(logins[role])
		if !strings.HasSuffix(logins[role], "[bot]") || slug == "" {
			return nil, fmt.Errorf("legionAppLogins names no App login for %s", role)
		}
		apps.logins[slug] = true
	}
	if len(logins) != 2 {
		return nil, fmt.Errorf("legionAppLogins names %d Apps, not implement and review", len(logins))
	}
	apps.review = botSlug(logins["review"])
	return apps, nil
}

// botSlug is how both ends of every login comparison read an account: GitHub GraphQL names a Bot
// by its bare slug and the daemon by its git identity, "<slug>[bot]", and a login's case never
// distinguishes two accounts.
func botSlug(login string) string {
	return strings.ToLower(strings.TrimSuffix(login, "[bot]"))
}

// firstLine is a reply's first line, after any leading space, tab, CR or LF.
func firstLine(body string) string {
	line, _, _ := strings.Cut(strings.TrimLeft(body, " \t\r\n"), "\n")
	return strings.TrimSuffix(line, "\r")
}

// isAcceptance is the reviewer's acceptance reply form: its first line begins "Accepted:". Only
// space, tab, CR and LF may precede it; after other whitespace, such as a no-break space, it is
// not one.
func isAcceptance(body string) bool {
	return strings.HasPrefix(firstLine(body), "Accepted:")
}

// resolution says whether thread is resolved and on whose acceptance, or, when it is left open,
// why. Every account it compares is identified by what GitHub asserts about it, its type and its
// login together, never a login alone: a login is a string anyone may register (the review App's
// bare slug is a free username on a public repository), and every weaker proxy for "who wrote
// this" was forgeable by someone who read the rule. The subject of a finding never closes it: a thread closes only on its newest submitted
// comment being an Accepted: from its opener, or, on a thread a Bot that is none of Legion's role
// Apps opened, from Legion's review App. GitHub cannot tell a CI bot from a person whose gh is
// routed to an App, and such a bot may never accept, so the Legion reviewer is the independent
// party who adjudicates its finding; the reviewer may accept a finding an App-routed person
// raised, which the resolved line then says. The pull request's author (the implementer, whose
// App the merger shares) closes nothing: its reply is an answer, not an acceptance. The reviewer's
// acceptance need not follow an answer from the author: accepting is the reviewer's judgement of
// the finding, and a required prior reply would be a ceremony the implementer could satisfy with
// an empty one. A draft in a pending review never counts, since GitHub shows it only to its author.
func resolution(thread reviewThread, apps *legionApps) (how, reason string) {
	if thread.newestPending {
		return "", "an unsubmitted draft in a pending review"
	}
	acceptance := isAcceptance(thread.newestBody)
	if acceptance && thread.openerLogin != "" && thread.newestLogin != "" && thread.openerTypename == thread.newestTypename &&
		botSlug(thread.openerLogin) == botSlug(thread.newestLogin) {
		return "its opener's acceptance", ""
	}
	if thread.openerTypename != "Bot" {
		return "", "not an acceptance"
	}
	if apps == nil {
		return "", "not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:"
	}
	if apps.logins[botSlug(thread.openerLogin)] {
		return "", "not an acceptance"
	}
	if acceptance && thread.newestTypename == "Bot" && thread.newestLogin != "" && botSlug(thread.newestLogin) == apps.review {
		return "the Legion reviewer's acceptance of a bot's thread", ""
	}
	return "", "not its opener's or the Legion reviewer's acceptance"
}

func unresolvedReviewThreads(ctx context.Context, token string, repository ghrepo.Repository, number int) ([]reviewThread, error) {
	var all []reviewThread
	var after any
	for {
		var page reviewThreadsPage
		if err := graphql(ctx, token, reviewThreadsQuery, map[string]any{"owner": repository.Owner(), "name": repository.Name(), "number": number, "after": after}, &page); err != nil {
			return nil, err
		}
		if page.Data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("%s#%d was not found by GitHub", repository, number)
		}
		for _, node := range page.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if node.IsResolved {
				continue
			}
			if len(node.Opening.Nodes) == 0 || len(node.Newest.Nodes) == 0 {
				return nil, fmt.Errorf("review thread %s has no comments", node.ID)
			}
			opening, newest := node.Opening.Nodes[0], node.Newest.Nodes[0]
			// GitHub always answers state when the query selects it. Anything else means the query or
			// the response changed shape, and reading it as submitted would resolve on a draft.
			if newest.State != "PENDING" && newest.State != "SUBMITTED" {
				return nil, fmt.Errorf("review thread %s: its newest comment carried state %q, not \"PENDING\" or \"SUBMITTED\"", node.ID, newest.State)
			}
			all = append(all, reviewThread{id: node.ID, url: opening.URL, openerLogin: opening.Author.Login, openerTypename: opening.Author.Typename,
				newestLogin: newest.Author.Login, newestTypename: newest.Author.Typename, newestBody: newest.Body, newestPending: newest.State == "PENDING"})
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
	URL  string `json:"url"`
	Body string `json:"body"`
	// State is GitHub's PullRequestReviewCommentState (PENDING or SUBMITTED), selected on newest.
	State  string `json:"state"`
	Author struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
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
