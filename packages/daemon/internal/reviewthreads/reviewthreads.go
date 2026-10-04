// Package reviewthreads is Legion's one rule for which unresolved review thread an acceptance
// closes, and the GitHub GraphQL reads and writes that apply it. `legion threads resolve` applies it
// as the App of the role running it (or, with --gh, as the caller's own gh), except in the
// reviewer's pane, where it asks the daemon: GitHub refuses the review App a resolve on the
// implementer's pull request, so the daemon's POST /legion/v1/threads/resolve applies the rule's
// bot-thread half for the reviewer, as the implement App. One rule, so the two cannot drift.
package reviewthreads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

const threadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved opener: comments(first: 1) { nodes { author { __typename login } url } } newest: comments(last: 1) { nodes { author { __typename login } url body state } } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const resolveMutation = `mutation($threadId: ID!) { resolveReviewThread(input: { threadId: $threadId }) { thread { id isResolved } } }`

// Thread is an unresolved review thread reduced to the facts the rule reads.
type Thread struct {
	ID, URL, OpenerLogin, OpenerTypename, NewestLogin, NewestTypename, NewestBody string
	// NewestPending marks a newest comment that is a draft in a pending, unsubmitted review.
	NewestPending bool
}

// Apps is Legion's role Apps: every App's login as botSlug reads it, and the review App's. A nil
// *Apps is a caller that cannot know them, and then no thread counts as a bot's.
type Apps struct {
	logins map[string]bool
	review string
}

// AppsFrom is the daemon's role-keyed logins (each App's git identity, "<slug>[bot]") as Apps, nil
// when it named none. The daemon names every role App or none (the contract's legionAppLogins), so
// logins naming some, or one whose login is not an App's, is an invalid answer rather than a caller
// that knows some of Legion's Apps.
func AppsFrom(logins map[appauth.AppRole]string) (*Apps, error) {
	if logins == nil {
		return nil, nil
	}
	if len(logins) != len(appauth.Roles) {
		return nil, fmt.Errorf("legionAppLogins names %d Apps, not one for each of %v", len(logins), appauth.Roles)
	}
	apps := &Apps{logins: map[string]bool{}, review: botSlug(logins[appauth.Review])}
	for _, role := range appauth.Roles {
		login := logins[role]
		if !strings.HasSuffix(login, "[bot]") || botSlug(login) == "" {
			return nil, fmt.Errorf("legionAppLogins names no App login for %s", role)
		}
		apps.logins[botSlug(login)] = true
	}
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

// Acceptance is whose acceptance closes a thread, as the resolved line says it.
type Acceptance string

const (
	// OpenersAcceptance closes a thread whose newest submitted comment is its opener's Accepted:.
	OpenersAcceptance Acceptance = "its opener's acceptance"
	// ReviewersAcceptanceOfABot closes a thread a bot that is none of Legion's role Apps opened,
	// whose newest submitted comment is the Legion review App's Accepted:.
	ReviewersAcceptanceOfABot Acceptance = "the Legion reviewer's acceptance of a bot's thread"
)

// Resolution says whose acceptance closes thread, or, when none does, why it is left open. Every
// account it compares is identified by what GitHub asserts about it, its type and its login
// together, never a login alone: a login is a string anyone may register (the review App's bare
// slug is a free username on a public repository), and every weaker proxy for "who wrote this" was
// forgeable by someone who read the rule. The subject of a finding never closes it: a thread closes
// only on its newest submitted comment being an Accepted: from its opener, or, on a thread a Bot
// that is none of Legion's role Apps opened, from Legion's review App. GitHub cannot tell a CI bot
// from a person whose gh is routed to an App, and such a bot may never accept, so the Legion
// reviewer is the independent party who adjudicates its finding; the reviewer may accept a finding
// an App-routed person raised, which the resolved line then says. The pull request's author (the
// implementer, whose App the merger shares) closes nothing: its reply is an answer, not an
// acceptance. The reviewer's acceptance need not follow an answer from the author: accepting is the
// reviewer's judgement of the finding, and a required prior reply would be a ceremony the
// implementer could satisfy with an empty one. A draft in a pending review never counts, since
// GitHub shows it only to its author.
func Resolution(thread Thread, apps *Apps) (Acceptance, string) {
	if thread.NewestPending {
		return "", "an unsubmitted draft in a pending review"
	}
	acceptance := isAcceptance(thread.NewestBody)
	if acceptance && thread.OpenerLogin != "" && thread.NewestLogin != "" && thread.OpenerTypename == thread.NewestTypename &&
		botSlug(thread.OpenerLogin) == botSlug(thread.NewestLogin) {
		return OpenersAcceptance, ""
	}
	if thread.OpenerTypename != "Bot" {
		return "", "not an acceptance"
	}
	if apps == nil {
		return "", "not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:"
	}
	if apps.logins[botSlug(thread.OpenerLogin)] {
		return "", "not an acceptance"
	}
	if acceptance && thread.NewestTypename == "Bot" && thread.NewestLogin != "" && botSlug(thread.NewestLogin) == apps.review {
		return ReviewersAcceptanceOfABot, ""
	}
	return "", "not its opener's or the Legion reviewer's acceptance"
}

// Outcome is what applying the rule did with one unresolved thread: resolved it on whose
// acceptance (Resolved), or left it open and why (LeftOpen), with its newest comment's author.
type Outcome struct {
	URL      string     `json:"url"`
	Resolved Acceptance `json:"resolved,omitempty"`
	LeftOpen string     `json:"leftOpen,omitempty"`
	NewestBy string     `json:"newestBy,omitempty"`
}

// String is the outcome as `legion threads resolve` prints it: `resolved <url> — <whose
// acceptance>`, or `left open <url> — newest reply by <login> is <why>`.
func (o Outcome) String() string {
	if o.Resolved != "" {
		return fmt.Sprintf("resolved %s — %s", o.URL, o.Resolved)
	}
	by := o.NewestBy
	if by == "" {
		by = "an unknown account"
	}
	return fmt.Sprintf("left open %s — newest reply by %s is %s", o.URL, by, o.LeftOpen)
}

// Refused is GitHub refusing to resolve a thread the rule closes, with its own message.
type Refused struct {
	URL string
	Err error
}

func (e *Refused) Error() string {
	return fmt.Sprintf("resolveReviewThread failed for %s: %v", e.URL, e.Err)
}

func (e *Refused) Unwrap() error { return e.Err }

// Resolve reads every unresolved review thread of the pull request, every page before any write,
// and resolves, one resolveReviewThread each, every thread closes says an acceptance closes. It
// returns each thread's outcome in GitHub's order. A refusal stops it: it returns the outcomes
// before the refused thread and a *Refused naming it.
func Resolve(ctx context.Context, call GraphQL, repository ghrepo.Repository, number int, closes func(Thread) (Acceptance, string)) ([]Outcome, error) {
	threads, err := unresolved(ctx, call, repository, number)
	if err != nil {
		return nil, err
	}
	outcomes := make([]Outcome, 0, len(threads))
	for _, thread := range threads {
		acceptance, reason := closes(thread)
		if acceptance == "" {
			outcomes = append(outcomes, Outcome{URL: thread.URL, LeftOpen: reason, NewestBy: thread.NewestLogin})
			continue
		}
		if err := call(ctx, resolveMutation, map[string]any{"threadId": thread.ID}, nil); err != nil {
			return outcomes, &Refused{URL: thread.URL, Err: err}
		}
		outcomes = append(outcomes, Outcome{URL: thread.URL, Resolved: acceptance, NewestBy: thread.NewestLogin})
	}
	return outcomes, nil
}

func unresolved(ctx context.Context, call GraphQL, repository ghrepo.Repository, number int) ([]Thread, error) {
	var all []Thread
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
			all = append(all, Thread{ID: node.ID, URL: opening.URL, OpenerLogin: opening.Author.Login, OpenerTypename: opening.Author.Typename,
				NewestLogin: newest.Author.Login, NewestTypename: newest.Author.Typename, NewestBody: newest.Body, NewestPending: newest.State == "PENDING"})
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
						Opening    struct {
							Nodes []comment `json:"nodes"`
						} `json:"opener"`
						Newest struct {
							Nodes []comment `json:"nodes"`
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

type comment struct {
	URL  string `json:"url"`
	Body string `json:"body"`
	// State is GitHub's PullRequestReviewCommentState (PENDING or SUBMITTED), selected on newest.
	State  string `json:"state"`
	Author struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
}

// GraphQL is one GraphQL call against GitHub as the identity its transport carries: the App whose
// token it holds (TokenGraphQL), or whoever the caller's own gh authenticates as. It fails with
// GitHub's own message, and decodes the response into into when into is not nil.
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
