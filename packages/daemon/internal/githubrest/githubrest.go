// Package githubrest is READY's and the daemon's client for GitHub's API with an installation
// token: one repository's REST API, and GitHub's GraphQL route. appauth mints those tokens with
// its own client.
package githubrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// RepositoryAPI is repository's REST base under endpoint, GitHub's API root; empty is
// https://api.github.com.
func RepositoryAPI(endpoint string, repository ghrepo.Repository) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/repos/" + repository.String()
}

// Client calls GitHub with an installation token. Its API is either a repository's REST base
// (RepositoryAPI), which Get, Post, GetPages and GetListPages take paths under, or GitHub's GraphQL
// route (GraphQLAPI), which GraphQL posts to; an empty endpoint is api.github.com for both.
type Client struct {
	Token string
	API   string
}

// Get reads path under the client's REST base into into. An answer other than 2xx is an *Answer.
func (c Client) Get(ctx context.Context, path string, into any) error {
	_, err := c.call(ctx, http.MethodGet, c.API+path, nil, into)
	return err
}

// Post sends body, as JSON, to path under the client's API, and reads GitHub's answer into into
// when into is not nil. An answer other than 2xx is an *Answer.
func (c Client) Post(ctx context.Context, path string, body, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode POST %s: %w", path, err)
	}
	_, err = c.call(ctx, http.MethodPost, c.API+path, encoded, into)
	return err
}

// GraphQLAPI is GitHub's GraphQL route under endpoint, GitHub's API root; empty is
// https://api.github.com. A Client whose API is it sends GraphQL.
func GraphQLAPI(endpoint string) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/graphql"
}

// GraphQLFailure is one error a GraphQL answer carries: its type, such as NOT_FOUND, its message,
// and the path of the field it is about, field names and list indexes as GitHub sends them.
type GraphQLFailure struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// GraphQLError is a GraphQL answer that carries errors, which GitHub sends with HTTP status 200.
type GraphQLError struct {
	Failures []GraphQLFailure
}

func (e *GraphQLError) Error() string {
	messages := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		messages = append(messages, failure.Message)
	}
	return "GitHub GraphQL: " + strings.Join(messages, "; ")
}

// NotFoundAt says whether every error is NOT_FOUND at the field path names, such as
// ("repository", "pullRequest") for a pull request number the repository does not have. A
// NOT_FOUND at another field, such as a repository the token cannot see, is not.
func (e *GraphQLError) NotFoundAt(path ...string) bool {
	for _, failure := range e.Failures {
		if failure.Type != "NOT_FOUND" || len(failure.Path) != len(path) {
			return false
		}
		for i, field := range path {
			if failure.Path[i] != field {
				return false
			}
		}
	}
	return len(e.Failures) != 0
}

// GraphQL sends query and variables to the client's API, a GraphQL route (GraphQLAPI), and reads
// the answer's data into into when into is not nil. An answer that carries errors is a
// *GraphQLError, and one other than 2xx an *Answer.
func (c Client) GraphQL(ctx context.Context, query string, variables map[string]any, into any) error {
	var response struct {
		Data   json.RawMessage  `json:"data"`
		Errors []GraphQLFailure `json:"errors"`
	}
	if err := c.Post(ctx, "", map[string]any{"query": query, "variables": variables}, &response); err != nil {
		return err
	}
	if len(response.Errors) != 0 {
		return &GraphQLError{Failures: response.Errors}
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return errors.New("GitHub GraphQL answered without data")
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(response.Data, into); err != nil {
		return fmt.Errorf("decode GitHub GraphQL data: %w", err)
	}
	return nil
}

// GetPages reads every page of the JSON array GitHub answers at path, a hundred items to a page,
// following the Link header's rel="next" until GitHub names none.
func GetPages[T any](ctx context.Context, c Client, path string) ([]T, error) {
	return getPages[T](ctx, c, path, "")
}

// GetListPages reads every page of the list GitHub answers at path as the field of a JSON object,
// as its check-run and workflow-run lists answer ({"total_count": n, "check_runs": [...]}), paged as
// GetPages pages an array. An answer without the field is an error, never an empty list.
func GetListPages[T any](ctx context.Context, c Client, path, field string) ([]T, error) {
	return getPages[T](ctx, c, path, field)
}

// getPages is GetPages, reading each page's items from the field of the object it answers when
// field is not empty. A next page on another scheme or host than the REST base's is refused, so the
// token goes nowhere but the API it was configured for.
func getPages[T any](ctx context.Context, c Client, path, field string) ([]T, error) {
	base, err := url.Parse(c.API)
	if err != nil {
		return nil, fmt.Errorf("parse the REST base %q: %w", c.API, err)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	var all []T
	for target := c.API + path + separator + "per_page=100"; target != ""; {
		var page []T
		var object map[string]json.RawMessage
		var into any = &page
		if field != "" {
			into = &object
		}
		next, err := c.call(ctx, http.MethodGet, target, nil, into)
		if err != nil {
			return nil, err
		}
		if field != "" {
			list, ok := object[field]
			if !ok {
				return nil, fmt.Errorf("GitHub answered GET %s with no %q list", path, field)
			}
			if err := json.Unmarshal(list, &page); err != nil {
				return nil, fmt.Errorf("decode the %q list GitHub answered GET %s with: %w", field, path, err)
			}
		}
		all = append(all, page...)
		if next != "" {
			nextURL, err := url.Parse(next)
			if err != nil || nextURL.Scheme != base.Scheme || !strings.EqualFold(nextURL.Host, base.Host) {
				return nil, fmt.Errorf("GET %s: GitHub's next page %q is not on %s://%s, the only origin sent the token", path, next, base.Scheme, base.Host)
			}
		}
		target = next
	}
	return all, nil
}

// call sends method to target, with body as JSON when it is not nil, reads a 2xx answer into into
// when into is not nil, and returns the next page's URL its Link header names, "" for none.
func (c Client) call(ctx context.Context, method, target string, body []byte, into any) (string, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		path := c.answerPath(request.URL)
		return "", &Answer{Method: method, Path: path, Status: response.StatusCode, Body: strings.TrimSpace(string(answer)),
			RateLimited: RateLimited(response, answer), RetryAfter: retryAfter(response, answer, time.Now())}
	}
	if into == nil {
		return nextPage(response.Header.Get("Link")), nil
	}
	return nextPage(response.Header.Get("Link")), json.Unmarshal(answer, into)
}

// answerPath is the path an Answer names for target, its query left out: the path under the
// client's API, or GitHub's whole path for a URL outside it, such as a later page GitHub names
// under /repositories/<id>/, or for a call to the API itself, as GraphQL's post is.
func (c Client) answerPath(target *url.URL) string {
	if base, err := url.Parse(c.API); err == nil {
		if rest, found := strings.CutPrefix(target.Path, base.Path); found && strings.HasPrefix(rest, "/") {
			return rest
		}
	}
	return target.Path
}

// secondaryLimitMessages are what GitHub's secondary rate limit says in the body of the 403 it can
// answer with instead of a 429: "You have exceeded a secondary rate limit", with the older wording
// of the same limit, "You have triggered an abuse detection mechanism", beside it.
var secondaryLimitMessages = []string{"secondary rate limit", "abuse detection mechanism"}

// RateLimited says whether response, whose body is body, is GitHub's rate limit: a 429, or a 403
// carrying x-ratelimit-remaining: 0 or a retry-after, or one whose body says it is the secondary
// limit, which GitHub's REST documentation allows with neither header set.
func RateLimited(response *http.Response, body []byte) bool {
	if response.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "" {
		return true
	}
	message := strings.ToLower(string(body))
	for _, named := range secondaryLimitMessages {
		if strings.Contains(message, named) {
			return true
		}
	}
	return false
}

// retryAfter is how long GitHub asks a caller its rate limit answered to wait before calling again,
// as its REST documentation's "Exceeding the rate limit" says: the retry-after header's seconds,
// else, when x-ratelimit-remaining is 0, until the x-ratelimit-reset epoch second, capped at an
// hour, GitHub's primary rate limit's window, so a host clock behind GitHub's never stretches the
// wait past it; else at least a minute, which is what a secondary limit named by its message alone
// is waited. A reset this host's clock already reads as past is waited the minute too. Zero for an
// answer that is not a rate limit.
func retryAfter(response *http.Response, body []byte, now time.Time) time.Duration {
	if !RateLimited(response, body) {
		return 0
	}
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Unix(reset, 0).Sub(now); wait > 0 {
				if wait > time.Hour {
					return time.Hour
				}
				return wait
			}
		}
	}
	return time.Minute
}

// nextPage is the URL a Link header names rel="next", "" when it names none.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, found := strings.Cut(strings.TrimSpace(part), ";")
		if found && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// Answer is a GitHub REST answer other than 2xx: the request's method and path (its query left
// out), GitHub's HTTP status, the body GitHub sent with it, and whether it is a rate limit, which
// every further call on the same token meets until the limit resets, with how long GitHub asks the
// caller to wait before its next call (retryAfter; zero when it is not one).
type Answer struct {
	Method      string
	Path        string
	Status      int
	Body        string
	RateLimited bool
	RetryAfter  time.Duration
}

func (a *Answer) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", a.Method, a.Path, a.Status, a.Body)
}
